package integrations

import (
	"context"
	"errors"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var ErrFanPreparationUnsupported = errors.New("fan preparation is unsupported by this route")
var ErrFanObservationUnavailable = errors.New("fresh observed fan speed is unavailable")

type FanCapture struct {
	router         *CommandRouter
	internal       *internalOccurrence
	alias          domain.HAAlias
	entity         domain.EntityID
	binding        Binding
	description    *wireregistry.CapabilityDescription
	profile        *fanProfileSnapshot
	state          domain.ObservedState
	hasState       bool
	epoch          string
	configRevision uint64
}

// CaptureFan uses the same generation -> Host -> catalogue -> sequencer
// order as light preparation, including the final sequencer route recheck.
// CaptureLightRoute's implementation is schema-neutral despite its old name.
func (r *CommandRouter) CaptureFan(ctx context.Context, alias domain.HAAlias) (result FanCapture, resultErr error) {
	defer func() {
		if resultErr == nil {
			resultErr = r.filterCapture(ctx, result.entity, result.description, &result.state, &result.hasState)
		}
		resultErr = r.captureError(ctx, resultErr)
		if resultErr != nil {
			result = FanCapture{}
		}
	}()
	if r == nil || r.host == nil || ctx == nil {
		return FanCapture{}, ErrInvalidInput
	}
	internal, err := r.internalFromContext(ctx)
	if err != nil {
		return FanCapture{}, err
	}
	if _, err := alias.Value(); err != nil {
		return FanCapture{}, ErrInvalidInput
	}
	projection, ok := r.projection.(LightPreparationProjection)
	if !ok || nilInterface(projection) {
		return FanCapture{}, ErrCommandRouteUnavailable
	}
	epochAuthority, ok := r.projection.(CommandEpochAuthority)
	if !ok || nilInterface(epochAuthority) {
		return FanCapture{}, ErrCommandRouteUnavailable
	}
	entity, err := r.projection.ResolveCommandAlias(alias)
	if err != nil {
		return FanCapture{}, err
	}
	epoch, err := epochAuthority.CurrentAdmissionEpoch()
	if err != nil || epoch == "" {
		return FanCapture{}, ErrCommandRouteUnavailable
	}
	var capture FanCapture
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
			if candidate != nil && candidate.SchemaId == contracts.FanSchema {
				if description != nil {
					return ErrCommandRouteAmbiguous
				}
				description = candidate
			}
		}
		if description == nil {
			return ErrFanPreparationUnsupported
		}
		profile := h.fanProfiles[entity]
		if profile == nil || profile.binding != binding || profile.alias != alias ||
			!proto.Equal(profile.description, description) || !validCasetaHAFan4V1Description([]*wireregistry.CapabilityDescription{description}) {
			return ErrFanPreparationUnsupported
		}
		slot := commandOwnerSlot{binding.device, description.SchemaId, description.Endpoint}
		route := CommandRoute{Entity: entity, Alias: &alias, Binding: binding, Description: description,
			Designated: h.commandOwners[slot], Epoch: epoch}
		state, hasState, err := projection.CaptureLightRoute(route)
		if err != nil {
			return err
		}
		capture = FanCapture{router: r, internal: internal, alias: alias, entity: entity, binding: binding,
			description: proto.Clone(description).(*wireregistry.CapabilityDescription), profile: profile,
			state: state, hasState: hasState, epoch: epoch, configRevision: revision}
		return nil
	})
	return capture, err
}

func (c FanCapture) criticalRevision() (domain.FanSpeed, uint64, error) {
	if c.router == nil || c.profile == nil || !c.hasState {
		return domain.FanSpeedUnspecified, 0, ErrFanObservationUnavailable
	}
	_, _, _, _, revision, err := c.state.Components()
	if err != nil {
		return domain.FanSpeedUnspecified, 0, ErrFanObservationUnavailable
	}
	value, critical := c.state.CriticalValue()
	if !critical {
		return domain.FanSpeedUnspecified, 0, ErrFanObservationUnavailable
	}
	speed, ok := value.Fan()
	if !ok {
		return domain.FanSpeedUnspecified, 0, ErrFanObservationUnavailable
	}
	if _, err := speed.String(); err != nil {
		return domain.FanSpeedUnspecified, 0, ErrFanObservationUnavailable
	}
	return speed, revision, nil
}

type PreparedFan struct {
	actor   domain.ActorID
	request *actions.CommandRequest
	route   *preparedFanRoute
}
type preparedFanRoute struct {
	capture FanCapture
	actor   domain.ActorID
	request *actions.CommandRequest
}

func (p PreparedFan) Request() (*actions.CommandRequest, error) {
	if p.route == nil || p.request == nil {
		return nil, ErrInvalidInput
	}
	return proto.Clone(p.request).(*actions.CommandRequest), nil
}
func (p PreparedFan) Actor() (domain.ActorID, error) {
	if p.route == nil || p.request == nil {
		return domain.ActorID{}, ErrInvalidInput
	}
	if p.route.capture.internal != nil {
		return domain.ActorID{}, ErrCommandAuthorizationDenied
	}
	return p.actor, nil
}

// PrepareHA maps the qualified four-speed HA percentage exactly once. A
// scene OFF ignores stale exported percentage, as HA fan reproduction does.
func (c FanCapture) PrepareHA(actor domain.ActorID, id domain.CommandID, expiresAt time.Time, on bool, percentage *uint8) (PreparedFan, error) {
	if c.router == nil || c.profile == nil || c.description == nil || expiresAt.IsZero() || !expiresAt.After(c.router.clock.Now()) {
		return PreparedFan{}, ErrInvalidInput
	}
	if !captureActorValid(actor, c.internal) {
		return PreparedFan{}, ErrInvalidInput
	}
	idText, err := id.Value()
	if err != nil {
		return PreparedFan{}, ErrInvalidInput
	}
	_, revision, err := c.criticalRevision()
	if err != nil {
		return PreparedFan{}, err
	}
	speed := domain.FanSpeedOff
	if on {
		if percentage == nil {
			return PreparedFan{}, ErrFanPreparationUnsupported
		}
		switch *percentage {
		case 0:
			speed = domain.FanSpeedOff
		case 25:
			speed = domain.FanSpeedLow
		case 50:
			speed = domain.FanSpeedMedium
		case 75:
			speed = domain.FanSpeedMediumHigh
		case 100:
			speed = domain.FanSpeedHigh
		default:
			return PreparedFan{}, ErrFanPreparationUnsupported
		}
	} else if percentage != nil {
		return PreparedFan{}, ErrInvalidInput
	}
	speedText, _ := speed.String()
	aliasText, _ := c.alias.Value()
	wire := &actions.CommandRequest{CommandId: idText, AdmissionEpoch: c.epoch, ConfigRevision: c.configRevision,
		Target:             &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_HaAlias{HaAlias: aliasText}},
		CapabilitySchemaId: contracts.FanSchema, Endpoint: c.description.Endpoint,
		Arguments:        &actions.ActionArgs{Arguments: &actions.ActionArgs_SetFan{SetFan: &actions.SetFan{Speed: &speedText}}},
		ExpectedRevision: &revision, ExpiresAt: timestamppb.New(expiresAt)}
	if wire.ExpiresAt.CheckValid() != nil {
		return PreparedFan{}, ErrInvalidInput
	}
	resolved := proto.Clone(wire).(*actions.CommandRequest)
	entityText, _ := c.entity.Value()
	resolved.Target = &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_EntityId{EntityId: entityText}}
	if _, contractErr := c.router.host.catalog.PrepareCapabilityCommand(resolved); contractErr != nil {
		return PreparedFan{}, ErrFanPreparationUnsupported
	}
	owned := proto.Clone(wire).(*actions.CommandRequest)
	bound := &preparedFanRoute{capture: c, actor: actor, request: proto.Clone(owned).(*actions.CommandRequest)}
	return PreparedFan{actor: actor, request: owned, route: bound}, nil
}
