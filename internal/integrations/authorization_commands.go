package integrations

import (
	"context"
	"encoding/binary"
	"errors"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

var ErrCommandAuthorizationDenied = errors.New("command permission denied")

type expectedAuthorizationTargetKey struct{}

func withExpectedAuthorizationTarget(ctx context.Context, target authorization.CommandTarget) context.Context {
	if _, _, err := target.Values(); err != nil {
		return ctx
	}
	return context.WithValue(ctx, expectedAuthorizationTargetKey{}, target)
}
func preparedAuthorizationContext(ctx context.Context, route *preparedCommandRoute) context.Context {
	if ctx == nil || route == nil {
		return ctx
	}
	var entity domain.EntityID
	var description *registry.CapabilityDescription
	switch route.kind {
	case preparedCommandLight:
		if route.light != nil {
			entity, description = route.light.capture.entity, route.light.capture.description
		}
	case preparedCommandFan:
		if route.fan != nil {
			entity, description = route.fan.capture.entity, route.fan.capture.description
		}
	case preparedCommandDirect:
		if route.direct != nil {
			entity, description = route.direct.capture.entity, route.direct.capture.description
		}
	default:
		return ctx
	}
	if description == nil {
		return ctx
	}
	slot, err := authorization.NewCapabilitySlot(description.SchemaId, description.Endpoint)
	if err != nil {
		return ctx
	}
	target, err := authorization.NewCommandTarget(entity, slot)
	if err != nil {
		return ctx
	}
	return withExpectedAuthorizationTarget(ctx, target)
}
func (r *CommandRouter) authorizeMissingRecord(ctx context.Context, actor domain.ActorID, wire *actions.CommandRequest) error {
	if target, ok := ctx.Value(expectedAuthorizationTargetKey{}).(authorization.CommandTarget); ok {
		return r.authorizeRemembered(ctx, actor, target, authv1.ScopeOperation_SCOPE_OPERATION_READ)
	}
	_, err := r.authorizeRequest(ctx, actor, wire, authv1.ScopeOperation_SCOPE_OPERATION_READ)
	return err
}
func (r *CommandRouter) resolvedAuthorizationTarget(wire *actions.CommandRequest) (authorization.CommandTarget, error) {
	if r == nil || r.projection == nil || wire == nil || wire.Target == nil {
		return authorization.CommandTarget{}, ErrCommandAuthorizationDenied
	}
	var entity domain.EntityID
	var err error
	switch target := wire.Target.Reference.(type) {
	case *registry.EntityRef_EntityId:
		if target == nil {
			return authorization.CommandTarget{}, ErrCommandAuthorizationDenied
		}
		entity, err = domain.ParseEntityID(target.EntityId)
	case *registry.EntityRef_HaAlias:
		if target == nil {
			return authorization.CommandTarget{}, ErrCommandAuthorizationDenied
		}
		alias, parseErr := domain.ParseHAAlias(target.HaAlias)
		if parseErr != nil {
			return authorization.CommandTarget{}, ErrCommandAuthorizationDenied
		}
		entity, err = r.projection.ResolveCommandAlias(alias)
	default:
		return authorization.CommandTarget{}, ErrCommandAuthorizationDenied
	}
	if err != nil {
		return authorization.CommandTarget{}, ErrCommandAuthorizationDenied
	}
	slot, err := authorization.NewCapabilitySlot(wire.CapabilitySchemaId, wire.Endpoint)
	if err != nil {
		return authorization.CommandTarget{}, ErrCommandAuthorizationDenied
	}
	return authorization.NewCommandTarget(entity, slot)
}
func (r *CommandRouter) authorizeRemembered(ctx context.Context, actor domain.ActorID, target authorization.CommandTarget, operation authv1.ScopeOperation) error {
	if r == nil || r.host == nil || ctx == nil {
		return ErrCommandAuthorizationDenied
	}
	authority := r.host.commandAuthority()
	if authority == nil {
		return nil
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return ErrCommandAuthorizationDenied
	}
	admitted, err := principal.Actor()
	if err != nil || admitted != actor {
		return ErrCommandAuthorizationDenied
	}
	object, err := target.Object()
	if err != nil {
		return ErrCommandAuthorizationDenied
	}
	permission, err := authorization.NewPermission(authv1.ScopeFamily_SCOPE_FAMILY_ACTIONS, operation)
	if err != nil {
		return ErrCommandAuthorizationDenied
	}
	current, err := authority.Current(principal)
	if err != nil {
		return ErrCommandAuthorizationDenied
	}
	if current.Authorize(permission, object) != nil {
		return ErrCommandAuthorizationDenied
	}
	return nil
}
func (r *CommandRouter) authorizeRequest(ctx context.Context, actor domain.ActorID, wire *actions.CommandRequest, operation authv1.ScopeOperation) (authorization.CommandTarget, error) {
	if r == nil || r.host == nil || ctx == nil {
		return authorization.CommandTarget{}, ErrCommandAuthorizationDenied
	}
	if r.host.commandAuthority() == nil {
		return authorization.CommandTarget{}, nil
	}
	target, err := r.resolvedAuthorizationTarget(wire)
	if err != nil {
		return authorization.CommandTarget{}, ErrCommandAuthorizationDenied
	}
	if expected, ok := ctx.Value(expectedAuthorizationTargetKey{}).(authorization.CommandTarget); ok && expected != target {
		return authorization.CommandTarget{}, ErrCommandAuthorizationDenied
	}
	if err := r.authorizeRemembered(ctx, actor, target, operation); err != nil {
		return authorization.CommandTarget{}, err
	}
	return target, nil
}
func (r *CommandRouter) commandPermit(ctx context.Context, actor domain.ActorID, wire *actions.CommandRequest) (authorization.CommandTarget, *authorization.Permit, error) {
	target, err := r.authorizeRequest(ctx, actor, wire, authv1.ScopeOperation_SCOPE_OPERATION_WRITE)
	if err != nil {
		return authorization.CommandTarget{}, nil, err
	}
	authority := r.host.commandAuthority()
	if authority == nil {
		return target, nil, nil
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return authorization.CommandTarget{}, nil, ErrCommandAuthorizationDenied
	}
	permission, err := authorization.NewPermission(authv1.ScopeFamily_SCOPE_FAMILY_ACTIONS, authv1.ScopeOperation_SCOPE_OPERATION_WRITE)
	if err != nil {
		return authorization.CommandTarget{}, nil, err
	}
	object, err := target.Object()
	if err != nil {
		return authorization.CommandTarget{}, nil, err
	}
	permit, err := authority.Permit(ctx, principal, permission, object)
	if err != nil {
		return authorization.CommandTarget{}, nil, ErrCommandAuthorizationDenied
	}
	return target, permit, nil
}
func withCurrentPermission(permit *authorization.Permit, fn func() error) error {
	if permit == nil {
		return fn()
	}
	if err := permit.WithCurrent(fn); err != nil {
		if errors.Is(err, authorization.ErrDenied) || errors.Is(err, authorization.ErrUnavailable) {
			return ErrCommandAuthorizationDenied
		}
		return err
	}
	return nil
}

// Stored status has no permission to reinterpret its old alias. The exact
// fingerprint supplies the historical actor, while the stored target supplies
// the historical entity/slot. Missing proof fails closed in a native Host.
func (r *CommandRouter) authorizeStored(ctx context.Context, fingerprint []byte, target authorization.CommandTarget, present bool) error {
	if r == nil || r.host == nil {
		return ErrCommandAuthorizationDenied
	}
	if r.host.commandAuthority() == nil {
		return nil
	}
	if !present {
		return ErrCommandAuthorizationDenied
	}
	size, n := binary.Uvarint(fingerprint)
	if n <= 0 || size == 0 || size > uint64(len(fingerprint)-n) {
		return ErrCommandAuthorizationDenied
	}
	actor, err := domain.ParseActorID(string(fingerprint[n : n+int(size)]))
	if err != nil {
		return ErrCommandAuthorizationDenied
	}
	return r.authorizeRemembered(ctx, actor, target, authv1.ScopeOperation_SCOPE_OPERATION_READ)
}

func (r *CommandRouter) filterCapture(ctx context.Context, entity domain.EntityID, description *registry.CapabilityDescription, state *domain.ObservedState, hasState *bool) error {
	if r == nil || r.host == nil {
		return ErrCommandAuthorizationDenied
	}
	authority := r.host.commandAuthority()
	if authority == nil {
		return nil
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil || description == nil {
		return ErrCommandAuthorizationDenied
	}
	current, err := authority.Current(principal)
	if err != nil {
		return ErrCommandAuthorizationDenied
	}
	permission, err := authorization.NewPermission(authv1.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY, authv1.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return ErrCommandAuthorizationDenied
	}
	allows := func(d *registry.CapabilityDescription) bool {
		if d == nil {
			return false
		}
		slot, e := authorization.NewCapabilitySlot(d.SchemaId, d.Endpoint)
		if e != nil {
			return false
		}
		object, e := authorization.EntityObject(entity, slot)
		return e == nil && current.Authorize(permission, object) == nil
	}
	if !allows(description) {
		return ErrCommandAuthorizationDenied
	}
	descriptions, err := r.host.catalog.Descriptions(entity)
	if err != nil {
		return ErrCommandAuthorizationDenied
	}
	present := false
	complete := len(descriptions) > 0
	for _, candidate := range descriptions {
		if proto.Equal(candidate, description) {
			present = true
		}
		if !allows(candidate) {
			complete = false
		}
	}
	if !present {
		return ErrCommandAuthorizationDenied
	}
	if !complete {
		*state = domain.ObservedState{}
		*hasState = false
	}
	return nil
}
func (r *CommandRouter) captureError(err error) error {
	if err != nil && r != nil && r.host != nil && r.host.commandAuthority() != nil {
		return ErrCommandAuthorizationDenied
	}
	return err
}

func authorizePreparedRead(ctx context.Context, router *CommandRouter, actor domain.ActorID, route *preparedCommandRoute) error {
	if router == nil || router.host == nil {
		return ErrCommandAuthorizationDenied
	}
	if router.host.commandAuthority() == nil {
		return nil
	}
	ctx = preparedAuthorizationContext(ctx, route)
	if ctx == nil {
		return ErrCommandAuthorizationDenied
	}
	target, ok := ctx.Value(expectedAuthorizationTargetKey{}).(authorization.CommandTarget)
	if !ok {
		return ErrCommandAuthorizationDenied
	}
	return router.authorizeRemembered(ctx, actor, target, authv1.ScopeOperation_SCOPE_OPERATION_READ)
}
func (p PreparedLight) AuthorizeRead(ctx context.Context) error {
	if p.route == nil {
		return ErrCommandAuthorizationDenied
	}
	return authorizePreparedRead(ctx, p.route.capture.router, p.actor, preparedLightCommand(p.route))
}
func (p PreparedFan) AuthorizeRead(ctx context.Context) error {
	if p.route == nil {
		return ErrCommandAuthorizationDenied
	}
	return authorizePreparedRead(ctx, p.route.capture.router, p.actor, preparedFanCommand(p.route))
}
func (p PreparedDirect) AuthorizeRead(ctx context.Context) error {
	if p.route == nil {
		return ErrCommandAuthorizationDenied
	}
	r := p.route.capture.router
	if err := authorizePreparedRead(ctx, r, p.actor, preparedDirectCommand(p.route)); err != nil {
		return err
	}
	if r.host.commandAuthority() == nil {
		return nil
	}
	state, has := p.route.capture.state, p.route.capture.hasState
	if err := r.filterCapture(ctx, p.route.capture.entity, p.route.capture.description, &state, &has); err != nil {
		return err
	}
	if !has {
		return ErrCommandAuthorizationDenied
	}
	return nil
}

// Scene diagnostics without a prepared slot may reveal a complete source
// member. Native composition admits them only when its complete current
// entity/capability set is readable; no alias or reason escapes on denial.
func (r *CommandRouter) authorizeSceneAlias(ctx context.Context, alias domain.HAAlias) error {
	if r == nil || r.host == nil {
		return ErrCommandAuthorizationDenied
	}
	authority := r.host.commandAuthority()
	if authority == nil {
		return nil
	}
	entity, err := r.projection.ResolveCommandAlias(alias)
	if err != nil {
		return ErrCommandAuthorizationDenied
	}
	descriptions, err := r.host.catalog.Descriptions(entity)
	if err != nil || len(descriptions) == 0 {
		return ErrCommandAuthorizationDenied
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return ErrCommandAuthorizationDenied
	}
	current, err := authority.Current(principal)
	if err != nil {
		return ErrCommandAuthorizationDenied
	}
	read, _ := authorization.NewPermission(authv1.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY, authv1.ScopeOperation_SCOPE_OPERATION_READ)
	for _, description := range descriptions {
		if description == nil {
			return ErrCommandAuthorizationDenied
		}
		slot, err := authorization.NewCapabilitySlot(description.SchemaId, description.Endpoint)
		if err != nil {
			return ErrCommandAuthorizationDenied
		}
		object, err := authorization.EntityObject(entity, slot)
		if err != nil || current.Authorize(read, object) != nil {
			return ErrCommandAuthorizationDenied
		}
	}
	return nil
}
func (s *VolatileCommandService) AuthorizeSceneAlias(ctx context.Context, alias domain.HAAlias) error {
	if s == nil {
		return ErrCommandAuthorizationDenied
	}
	return s.router.authorizeSceneAlias(ctx, alias)
}
func (s *DurableCommandService) AuthorizeSceneAlias(ctx context.Context, alias domain.HAAlias) error {
	if s == nil {
		return ErrCommandAuthorizationDenied
	}
	return s.router.authorizeSceneAlias(ctx, alias)
}

// AttachAuthorization is an explicit trusted composition boundary. Run always
// attaches its hydrated native authority before Configure or any pilot. A
// standalone legacy Host is an explicitly separate internal composition, not
// a way for a native transport to omit its principal.
func (h *Manager) AttachAuthorization(authority *authorization.Authority) error {
	if h == nil || authority == nil {
		return ErrInvalidInput
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.composition != compositionOpen || h.authorizationAuthority != nil || len(h.active) != 0 || len(h.commandPolicies) != 0 || h.unprotectedCommandDispatch {
		return ErrSupervisorState
	}
	h.authorizationAuthority = authority
	return nil
}
func (h *Manager) commandAuthority() *authorization.Authority {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.authorizationAuthority
}
