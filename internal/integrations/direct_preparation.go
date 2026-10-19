package integrations

import (
	"context"
	"errors"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var ErrDirectObservationUnavailable = errors.New("fresh observed direct command value is unavailable")
var ErrDirectProofExpired = errors.New("direct command deadline passed before equality proof")

// DirectKind admits only scene switch and native fan restoration. It is
// separate from the Caseta HA percentage projection and from direct SetFan.
type DirectKind uint8

const (
	DirectInvalid DirectKind = iota
	DirectSwitch
	DirectFan
)

func (k DirectKind) schema() string {
	switch k {
	case DirectSwitch:
		return contracts.SwitchSchema
	case DirectFan:
		return contracts.FanSchema
	default:
		return ""
	}
}

type DirectCapture struct {
	router         *CommandRouter
	kind           DirectKind
	alias          domain.HAAlias
	entity         domain.EntityID
	binding        Binding
	session        *Session
	designated     domain.EntityID
	description    *wireregistry.CapabilityDescription
	state          domain.ObservedState
	hasState       bool
	epoch          string
	configRevision uint64
}

func (r *CommandRouter) CaptureDirect(ctx context.Context, alias domain.HAAlias, kind DirectKind) (result DirectCapture, resultErr error) {
	defer func() {
		if resultErr == nil {
			resultErr = r.filterCapture(ctx, result.entity, result.description, &result.state, &result.hasState)
		}
		resultErr = r.captureError(resultErr)
		if resultErr != nil {
			result = DirectCapture{}
		}
	}()
	if r == nil || r.host == nil || ctx == nil || kind.schema() == "" {
		return DirectCapture{}, ErrInvalidInput
	}
	if _, err := alias.Value(); err != nil {
		return DirectCapture{}, ErrInvalidInput
	}
	projection, ok := r.projection.(LightPreparationProjection)
	if !ok || nilInterface(projection) {
		return DirectCapture{}, ErrCommandRouteUnavailable
	}
	authority, ok := r.projection.(CommandEpochAuthority)
	if !ok || nilInterface(authority) {
		return DirectCapture{}, ErrCommandRouteUnavailable
	}
	entity, err := r.projection.ResolveCommandAlias(alias)
	if err != nil {
		return DirectCapture{}, err
	}
	epoch, err := authority.CurrentAdmissionEpoch()
	if err != nil || epoch == "" {
		return DirectCapture{}, ErrCommandRouteUnavailable
	}
	var capture DirectCapture
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
			if candidate != nil && candidate.SchemaId == kind.schema() {
				if description != nil {
					return ErrCommandRouteAmbiguous
				}
				description = candidate
			}
		}
		if description == nil {
			return ErrCommandRouteUnavailable
		}
		slot := commandOwnerSlot{binding.device, description.SchemaId, description.Endpoint}
		route := CommandRoute{Entity: entity, Alias: &alias, Binding: binding,
			Description: description, Designated: h.commandOwners[slot], Epoch: epoch}
		state, hasState, err := projection.CaptureLightRoute(route)
		if err != nil {
			return err
		}
		capture = DirectCapture{router: r, kind: kind, alias: alias, entity: entity, binding: binding,
			session: session, designated: h.commandOwners[slot],
			description: proto.Clone(description).(*wireregistry.CapabilityDescription),
			state:       state, hasState: hasState, epoch: epoch, configRevision: revision}
		return nil
	})
	return capture, err
}

type PreparedDirect struct {
	actor   domain.ActorID
	request *actions.CommandRequest
	route   *preparedDirectRoute
}
type preparedDirectRoute struct {
	capture DirectCapture
	actor   domain.ActorID
	request *actions.CommandRequest
}

func (p PreparedDirect) Request() (*actions.CommandRequest, error) {
	if p.route == nil || p.request == nil {
		return nil, ErrInvalidInput
	}
	return proto.Clone(p.request).(*actions.CommandRequest), nil
}
func (p PreparedDirect) Actor() (domain.ActorID, error) {
	if p.route == nil || p.request == nil {
		return domain.ActorID{}, ErrInvalidInput
	}
	return p.actor, nil
}

// ProveSwitchSatisfied rechecks the original direct route and a current
// CriticalValue under the same capture locks as admission. It creates no
// ledger record or driver effect. The scene occurrence owns the no-op latch.
func (p PreparedDirect) ProveSwitchSatisfied(ctx context.Context) (bool, uint64, error) {
	if ctx == nil || p.route == nil || p.request == nil || p.route.capture.kind != DirectSwitch ||
		p.route.capture.router == nil || p.actor != p.route.actor || !proto.Equal(p.request, p.route.request) {
		return false, 0, ErrInvalidInput
	}
	set := p.request.GetArguments().GetSetSwitch()
	if set == nil || p.request.ExpectedRevision == nil || *p.request.ExpectedRevision == 0 {
		return false, 0, ErrInvalidInput
	}
	original := p.route.capture
	if original.router.host.commandAuthority() != nil {
		bound := preparedAuthorizationContext(ctx, preparedDirectCommand(p.route))
		target, ok := bound.Value(expectedAuthorizationTargetKey{}).(authorization.CommandTarget)
		if !ok {
			return false, 0, ErrCommandAuthorizationDenied
		}
		if err := original.router.authorizeRemembered(ctx, p.actor, target, authv1.ScopeOperation_SCOPE_OPERATION_WRITE); err != nil {
			return false, 0, err
		}
	}
	if p.request.ExpiresAt == nil || p.request.ExpiresAt.CheckValid() != nil ||
		!p.request.ExpiresAt.AsTime().After(original.router.clock.Now()) {
		return false, 0, ErrDirectProofExpired
	}
	current, err := original.router.CaptureDirect(ctx, original.alias, DirectSwitch)
	if err != nil {
		return false, 0, err
	}
	if !p.request.ExpiresAt.AsTime().After(original.router.clock.Now()) {
		return false, 0, ErrDirectProofExpired
	}
	if current.entity != original.entity || current.binding != original.binding || current.session != original.session ||
		current.designated != original.designated || current.epoch != original.epoch ||
		current.configRevision != original.configRevision || !proto.Equal(current.description, original.description) ||
		current.alias != original.alias {
		return false, 0, ErrCommandRouteUnavailable
	}
	if !current.hasState {
		return false, 0, ErrDirectObservationUnavailable
	}
	_, _, _, _, revision, err := current.state.Components()
	if err != nil || revision == 0 {
		return false, 0, ErrDirectObservationUnavailable
	}
	if revision != *p.request.ExpectedRevision {
		return false, 0, ErrCommandRevisionChanged
	}
	value, critical := current.state.CriticalValue()
	if !critical {
		return false, 0, ErrDirectObservationUnavailable
	}
	on, ok := value.Switch()
	if !ok {
		return false, 0, ErrDirectObservationUnavailable
	}
	return on == set.On, revision, nil
}

func (c DirectCapture) PrepareSwitch(actor domain.ActorID, id domain.CommandID, expiresAt time.Time, on bool) (PreparedDirect, error) {
	if c.kind != DirectSwitch {
		return PreparedDirect{}, ErrInvalidInput
	}
	return c.prepare(actor, id, expiresAt, &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: on}}})
}
func (c DirectCapture) PrepareFan(actor domain.ActorID, id domain.CommandID, expiresAt time.Time, speed domain.FanSpeed) (PreparedDirect, error) {
	if c.kind != DirectFan {
		return PreparedDirect{}, ErrInvalidInput
	}
	name, err := speed.String()
	if err != nil {
		return PreparedDirect{}, err
	}
	return c.prepare(actor, id, expiresAt, &actions.ActionArgs{Arguments: &actions.ActionArgs_SetFan{SetFan: &actions.SetFan{Speed: &name}}})
}
func (c DirectCapture) prepare(actor domain.ActorID, id domain.CommandID, expiresAt time.Time, args *actions.ActionArgs) (PreparedDirect, error) {
	if c.router == nil || c.description == nil || !c.hasState || expiresAt.IsZero() || !expiresAt.After(c.router.clock.Now()) {
		return PreparedDirect{}, ErrInvalidInput
	}
	if _, err := actor.Value(); err != nil {
		return PreparedDirect{}, err
	}
	idText, err := id.Value()
	if err != nil {
		return PreparedDirect{}, err
	}
	_, _, _, _, revision, err := c.state.Components()
	if err != nil {
		return PreparedDirect{}, ErrDirectObservationUnavailable
	}
	value, critical := c.state.CriticalValue()
	if !critical {
		return PreparedDirect{}, ErrDirectObservationUnavailable
	}
	switch c.kind {
	case DirectSwitch:
		if _, ok := value.Switch(); !ok {
			return PreparedDirect{}, ErrDirectObservationUnavailable
		}
	case DirectFan:
		if _, ok := value.Fan(); !ok {
			return PreparedDirect{}, ErrFanObservationUnavailable
		}
	default:
		return PreparedDirect{}, ErrInvalidInput
	}
	aliasText, _ := c.alias.Value()
	wire := &actions.CommandRequest{CommandId: idText, AdmissionEpoch: c.epoch, ConfigRevision: c.configRevision,
		Target:             &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_HaAlias{HaAlias: aliasText}},
		CapabilitySchemaId: c.kind.schema(), Endpoint: c.description.Endpoint, Arguments: args,
		ExpectedRevision: &revision, ExpiresAt: timestamppb.New(expiresAt)}
	if wire.ExpiresAt.CheckValid() != nil {
		return PreparedDirect{}, ErrInvalidInput
	}
	resolved := proto.Clone(wire).(*actions.CommandRequest)
	entityText, _ := c.entity.Value()
	resolved.Target = &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_EntityId{EntityId: entityText}}
	if _, contractErr := c.router.host.catalog.PrepareCapabilityCommand(resolved); contractErr != nil {
		return PreparedDirect{}, ErrCommandRouteUnavailable
	}
	owned := proto.Clone(wire).(*actions.CommandRequest)
	bound := &preparedDirectRoute{capture: c, actor: actor, request: proto.Clone(owned).(*actions.CommandRequest)}
	return PreparedDirect{actor: actor, request: owned, route: bound}, nil
}
