package integrations

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	ErrLightObservationUnavailable = errors.New("fresh observed light brightness is unavailable")
	ErrLightPreparationUnsupported = errors.New("light preparation is unsupported by this route")
)

// LightPreparationProjection snapshots the exact route and state under one
// sequencer lock. Host holds its lock around the catalogue read and this call.
type LightPreparationProjection interface {
	CaptureLightRoute(CommandRoute) (domain.ObservedState, bool, error)
}

// LightCapture is a value-owned view of one route. Its private profile is not
// part of the public capability or CommandRequest wire. A later mutation does
// not change this capture; the final lease must compare it with the current one.
type LightCapture struct {
	router         *CommandRouter
	internal       *internalOccurrence
	alias          domain.HAAlias
	entity         domain.EntityID
	binding        Binding
	description    *wireregistry.CapabilityDescription
	profile        *lightEncodingSnapshot
	state          domain.ObservedState
	hasState       bool
	epoch          string
	configRevision uint64
}

// CaptureLight uses the same generation -> Host -> catalogue -> sequencer lock
// order as command admission, with no inflight reservation or driver call.
func (r *CommandRouter) CaptureLight(ctx context.Context, alias domain.HAAlias) (result LightCapture, resultErr error) {
	defer func() {
		if resultErr == nil {
			resultErr = r.filterCapture(ctx, result.entity, result.description, &result.state, &result.hasState)
		}
		resultErr = r.captureError(ctx, resultErr)
		if resultErr != nil {
			result = LightCapture{}
		}
	}()
	if r == nil || r.host == nil || ctx == nil {
		return LightCapture{}, ErrInvalidInput
	}
	internal, err := r.internalFromContext(ctx)
	if err != nil {
		return LightCapture{}, err
	}
	if _, err := alias.Value(); err != nil {
		return LightCapture{}, ErrInvalidInput
	}
	projection, ok := r.projection.(LightPreparationProjection)
	if !ok || nilInterface(projection) {
		return LightCapture{}, ErrCommandRouteUnavailable
	}
	epochAuthority, ok := r.projection.(CommandEpochAuthority)
	if !ok || nilInterface(epochAuthority) {
		return LightCapture{}, ErrCommandRouteUnavailable
	}
	// This prelookup and epoch read precede the generation/Host snapshot. They
	// are hints only: CaptureLightRoute rechecks both under the sequencer lock
	// while Host still holds its binding and private profile. A concurrent
	// registry/epoch mutation therefore refuses rather than mixing captures.
	entity, err := r.projection.ResolveCommandAlias(alias)
	if err != nil {
		return LightCapture{}, err
	}
	epoch, err := epochAuthority.CurrentAdmissionEpoch()
	if err != nil || epoch == "" {
		return LightCapture{}, ErrCommandRouteUnavailable
	}
	var capture LightCapture
	err = r.generation.WithCurrent(ctx, func(revision uint64) error {
		if revision == 0 {
			return ErrCommandRouteUnavailable
		}
		h := r.host
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.admission != admissionOpen || h.retirementExhausted {
			return ErrAdmissionsClosed
		}
		binding, exists := h.owners[entity]
		if !exists {
			return ErrCommandRouteUnavailable
		}
		if _, retiring := h.retiring[entity]; retiring {
			return ErrCommandRouteUnavailable
		}
		if _, retiring := h.retiringSources[retiringSource{binding.instance, binding.key}]; retiring {
			return ErrCommandRouteUnavailable
		}
		session := h.active[binding.instance]
		if session == nil || !session.live() || session.bindings[entity] != binding || nilInterface(session.commandDriver) {
			return ErrCommandRouteUnavailable
		}
		descriptions, err := h.catalog.Descriptions(entity)
		if err != nil {
			return err
		}
		var description *wireregistry.CapabilityDescription
		for _, candidate := range descriptions {
			if candidate != nil && candidate.SchemaId == contracts.LightSchema {
				if description != nil {
					return ErrCommandRouteAmbiguous
				}
				description = candidate
			}
		}
		if description == nil {
			return ErrLightPreparationUnsupported
		}
		profile := h.lightProfiles[entity]
		if profile == nil || profile.binding != binding || profile.endpoint != description.Endpoint || !proto.Equal(profile.description, description) ||
			profile.kind != lightProfileMQTT && profile.kind != lightProfileCasetaHA100V1 {
			return ErrLightPreparationUnsupported
		}
		if profile.kind == lightProfileCasetaHA100V1 && profile.casetaAlias != alias {
			return ErrLightPreparationUnsupported
		}
		slot := commandOwnerSlot{binding.device, description.SchemaId, description.Endpoint}
		route := CommandRoute{
			Entity: entity, Alias: &alias, Binding: binding, Description: description,
			Designated: h.commandOwners[slot], Epoch: epoch,
		}
		state, hasState, err := projection.CaptureLightRoute(route)
		if err != nil {
			return err
		}
		capture = LightCapture{
			router: r, internal: internal, alias: alias, entity: entity, binding: binding,
			description: proto.Clone(description).(*wireregistry.CapabilityDescription),
			profile:     profile, state: state, hasState: hasState,
			epoch: epoch, configRevision: revision,
		}
		return nil
	})
	if err != nil {
		return LightCapture{}, err
	}
	return capture, nil
}

// criticalRevision fences every prepared action to a fresh observed light
// state. Absolute and OFF actions do not read brightness, but still retain the
// state revision whose quality was trusted at preparation time.
func (c LightCapture) criticalRevision() (domain.LightValue, uint64, error) {
	if c.router == nil || c.profile == nil || !c.hasState {
		return domain.LightValue{}, 0, ErrLightObservationUnavailable
	}
	_, _, _, _, revision, err := c.state.Components()
	if err != nil {
		return domain.LightValue{}, 0, ErrLightObservationUnavailable
	}
	value, critical := c.state.CriticalValue()
	if !critical {
		return domain.LightValue{}, 0, ErrLightObservationUnavailable
	}
	light, ok := value.Light()
	if !ok {
		return domain.LightValue{}, 0, ErrLightObservationUnavailable
	}
	return light, revision, nil
}

// ObservedHABrightness is used only for a relative HA step. An observed OFF
// supplies a known zero; an observed ON without brightness does not.
func (c LightCapture) ObservedHABrightness() (uint8, uint64, error) {
	light, revision, err := c.criticalRevision()
	if err != nil {
		return 0, 0, ErrLightObservationUnavailable
	}
	on, percent, _, err := light.Components()
	if err != nil {
		return 0, 0, ErrLightObservationUnavailable
	}
	if !on {
		return 0, revision, nil
	}
	if percent == nil {
		return 0, 0, ErrLightObservationUnavailable
	}
	if *percent <= 0 {
		// MQTT JSON ignores an incoming zero brightness while ON, retaining
		// its prior HA attribute. Homeward's zero cannot reconstruct that
		// prior HA value. Caseta also reports ON only for a positive level.
		return 0, 0, ErrLightObservationUnavailable
	}
	var brightness float64
	switch c.profile.kind {
	case lightProfileMQTT:
		// Homeward's MQTT observation is a native percentage; HA's
		// value_to_brightness projects that observed level onto 0..255.
		brightness = math.RoundToEven(*percent * 255 / 100)
	case lightProfileCasetaHA100V1:
		// HA's Caseta adapter uses floor(level * 255 / 100) on observation.
		brightness = math.Floor(*percent * 255 / 100)
	default:
		return 0, 0, ErrLightPreparationUnsupported
	}
	return uint8(max(1, min(255, brightness))), revision, nil
}

// PreparedLight is an opaque, occurrence-owned envelope. Request returns a
// copy; no public constructor or token decoder exists after slot loss/reboot.
type PreparedLight struct {
	actor   domain.ActorID
	request *actions.CommandRequest
	route   *preparedLightRoute
}

type preparedLightRoute struct {
	capture       LightCapture
	actor         domain.ActorID
	request       *actions.CommandRequest
	target        lightTarget
	sourceHA      uint8
	hasSourceHA   bool
	nativePercent float64
	hasNative     bool
}

func (p PreparedLight) Request() (*actions.CommandRequest, error) {
	if p.route == nil || p.request == nil {
		return nil, ErrInvalidInput
	}
	return proto.Clone(p.request).(*actions.CommandRequest), nil
}

func (p PreparedLight) Actor() (domain.ActorID, error) {
	if p.route == nil || p.request == nil {
		return domain.ActorID{}, ErrInvalidInput
	}
	if p.route.capture.internal != nil {
		return domain.ActorID{}, ErrCommandAuthorizationDenied
	}
	return p.actor, nil
}

func (p PreparedLight) HABrightness() (uint8, bool) {
	if p.route == nil {
		return 0, false
	}
	return p.route.sourceHA, p.route.hasSourceHA
}

func (p PreparedLight) NativePercent() (float64, bool) {
	if p.route == nil {
		return 0, false
	}
	return p.route.nativePercent, p.route.hasNative
}

func (p PreparedLight) MQTTTarget() (uint32, bool) {
	if p.route == nil || p.route.capture.profile == nil || p.route.capture.profile.kind != lightProfileMQTT {
		return 0, false
	}
	return p.route.target.brightness, p.route.target.hasBrightness
}

// PrepareHA binds a single HA brightness (or its explicit absence) to this
// captured route. relative records the observed revision in the original wire.
// Every retry must reuse the returned PreparedLight, including its actor/ID.
func (c LightCapture) PrepareHA(actor domain.ActorID, id domain.CommandID, expiresAt time.Time, on bool, brightness *uint8, transition *float64, relative bool) (PreparedLight, error) {
	return c.PrepareHAWithCT(actor, id, expiresAt, on, brightness, nil, transition, relative)
}

// PrepareHAWithCT keeps the HA brightness and operative CT in one immutable
// intention, target and route token. CT is a mired value already obtained by
// HA's floor(1e6/K) rule; the live capability still has to admit it.
func (c LightCapture) PrepareHAWithCT(actor domain.ActorID, id domain.CommandID, expiresAt time.Time, on bool, brightness *uint8, ctMireds *uint32, transition *float64, relative bool) (PreparedLight, error) {
	if c.router == nil || c.profile == nil || c.description == nil || expiresAt.IsZero() || !expiresAt.After(c.router.clock.Now()) {
		return PreparedLight{}, ErrInvalidInput
	}
	if !captureActorValid(actor, c.internal) {
		return PreparedLight{}, ErrInvalidInput
	}
	idText, err := id.Value()
	if err != nil {
		return PreparedLight{}, ErrInvalidInput
	}
	_, observedRevision, err := c.criticalRevision()
	if err != nil {
		return PreparedLight{}, err
	}
	aliasText, _ := c.alias.Value()
	var source uint8
	if brightness != nil {
		source = *brightness
		on = source != 0 // HA's service turns final brightness zero OFF.
	} else if relative {
		return PreparedLight{}, ErrInvalidInput
	}
	arg := &actions.SetLight{On: &on}
	if ctMireds != nil {
		if !on || c.description.GetLight() == nil || c.description.GetLight().ColorTempMireds == nil ||
			(c.profile.kind == lightProfileMQTT && c.profile.encoding.ikea30W && transition != nil) {
			return PreparedLight{}, ErrLightPreparationUnsupported
		}
		ct := *ctMireds
		arg.ColorTempMireds = &ct
	}
	var target lightTarget
	if brightness != nil && source > 0 {
		if c.description.GetLight() == nil || c.description.GetLight().BrightnessPercent == nil {
			return PreparedLight{}, ErrLightPreparationUnsupported
		}
		var native float64
		switch c.profile.kind {
		case lightProfileMQTT:
			native = float64(source) * 100 / 255
		case lightProfileCasetaHA100V1:
			native = math.RoundToEven(float64(source) * 100 / 255)
			if native == 0 {
				return PreparedLight{}, ErrLightPreparationUnsupported
			}
		default:
			return PreparedLight{}, ErrLightPreparationUnsupported
		}
		arg.BrightnessPercent = &native
		target, err = targetForPreparedHABrightness(source, c.profile)
		if err != nil {
			return PreparedLight{}, err
		}
	} else {
		target = lightTarget{profile: c.profile, proofAvailable: true}
	}
	if transition != nil {
		seconds := *transition
		arg.TransitionSeconds = &seconds
	}
	if relative {
		if _, _, err := c.ObservedHABrightness(); err != nil {
			return PreparedLight{}, err
		}
	}
	wire := &actions.CommandRequest{
		CommandId: idText, AdmissionEpoch: c.epoch, ConfigRevision: c.configRevision,
		Target:             &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_HaAlias{HaAlias: aliasText}},
		CapabilitySchemaId: contracts.LightSchema, Endpoint: c.description.Endpoint,
		Arguments:        &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: arg}},
		ExpectedRevision: &observedRevision, ExpiresAt: timestamppb.New(expiresAt),
	}
	if wire.ExpiresAt.CheckValid() != nil {
		return PreparedLight{}, ErrInvalidInput
	}
	// Validate the actual capability and transition profile before caching a
	// result; dispatch will repeat the same catalogue/fence checks later.
	resolved := proto.Clone(wire).(*actions.CommandRequest)
	entityText, _ := c.entity.Value()
	resolved.Target = &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_EntityId{EntityId: entityText}}
	if _, contractErr := c.router.host.catalog.PrepareCapabilityCommand(resolved); contractErr != nil {
		return PreparedLight{}, ErrLightPreparationUnsupported
	}
	owned := proto.Clone(wire).(*actions.CommandRequest)
	bound := &preparedLightRoute{
		capture: c, actor: actor, request: proto.Clone(owned).(*actions.CommandRequest), target: target,
		sourceHA: source, hasSourceHA: brightness != nil,
	}
	if arg.BrightnessPercent != nil {
		bound.nativePercent, bound.hasNative = *arg.BrightnessPercent, true
	}
	return PreparedLight{actor: actor, request: owned, route: bound}, nil
}

// PrepareNative is for observed Homeward snapshots, whose brightness is
// already a native percentage. It never round-trips through HA 0..255.
func (c LightCapture) PrepareNative(actor domain.ActorID, id domain.CommandID, expiresAt time.Time, on bool, percent *float64, ctMireds *uint32, transition *float64) (PreparedLight, error) {
	if c.router == nil || c.profile == nil || c.description == nil || expiresAt.IsZero() || !expiresAt.After(c.router.clock.Now()) {
		return PreparedLight{}, ErrInvalidInput
	}
	if !captureActorValid(actor, c.internal) {
		return PreparedLight{}, ErrInvalidInput
	}
	idText, err := id.Value()
	if err != nil {
		return PreparedLight{}, ErrInvalidInput
	}
	_, revision, err := c.criticalRevision()
	if err != nil {
		return PreparedLight{}, err
	}
	if on && percent == nil {
		return PreparedLight{}, ErrLightPreparationUnsupported
	}
	if !on && (percent != nil || ctMireds != nil) {
		return PreparedLight{}, ErrInvalidInput
	}
	if ctMireds != nil && (c.description.GetLight() == nil || c.description.GetLight().ColorTempMireds == nil ||
		(c.profile.kind == lightProfileMQTT && c.profile.encoding.ikea30W && transition != nil)) {
		return PreparedLight{}, ErrLightPreparationUnsupported
	}
	arg := &actions.SetLight{On: &on}
	if percent != nil {
		value := *percent
		if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 || value > 100 {
			return PreparedLight{}, ErrInvalidInput
		}
		arg.BrightnessPercent = &value
	}
	if ctMireds != nil {
		ct := *ctMireds
		arg.ColorTempMireds = &ct
	}
	if transition != nil {
		seconds := *transition
		arg.TransitionSeconds = &seconds
	}
	aliasText, _ := c.alias.Value()
	wire := &actions.CommandRequest{
		CommandId: idText, AdmissionEpoch: c.epoch, ConfigRevision: c.configRevision,
		Target:             &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_HaAlias{HaAlias: aliasText}},
		CapabilitySchemaId: contracts.LightSchema, Endpoint: c.description.Endpoint,
		Arguments:        &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: arg}},
		ExpectedRevision: &revision, ExpiresAt: timestamppb.New(expiresAt),
	}
	if wire.ExpiresAt.CheckValid() != nil {
		return PreparedLight{}, ErrInvalidInput
	}
	resolved := proto.Clone(wire).(*actions.CommandRequest)
	entityText, _ := c.entity.Value()
	resolved.Target = &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_EntityId{EntityId: entityText}}
	if _, contractErr := c.router.host.catalog.PrepareCapabilityCommand(resolved); contractErr != nil {
		return PreparedLight{}, ErrLightPreparationUnsupported
	}
	target := targetForLight(resolved, c.profile)
	if !target.proofAvailable {
		return PreparedLight{}, ErrLightPreparationUnsupported
	}
	owned := proto.Clone(wire).(*actions.CommandRequest)
	bound := &preparedLightRoute{capture: c, actor: actor, request: proto.Clone(owned).(*actions.CommandRequest), target: target}
	if percent != nil {
		bound.nativePercent, bound.hasNative = *percent, true
	}
	return PreparedLight{actor: actor, request: owned, route: bound}, nil
}
