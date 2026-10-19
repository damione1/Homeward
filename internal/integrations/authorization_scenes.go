package integrations

import (
	"context"

	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
)

type sceneReadMode uint8

const (
	sceneReadInvalid sceneReadMode = iota
	sceneReadNative
	sceneReadLegacy
)

// SceneReadAccess is valid only inside WithSceneRead's bounded callback. A
// zero access never grants permission. Native callers cannot construct one.
type SceneReadAccess struct {
	mode    sceneReadMode
	current authorization.Current
}

// RequiresEntityAdmission preserves the explicit legacy CommandPort contract
// whose synthetic snapshots may omit registry identities. Only the admitted
// legacy mode can skip that proof; zero/unknown and native modes require it.
func (a SceneReadAccess) RequiresEntityAdmission() bool { return a.mode != sceneReadLegacy }

func (a SceneReadAccess) AdmitEntity(entity domain.EntityID, descriptions []*registry.CapabilityDescription) error {
	switch a.mode {
	case sceneReadLegacy:
		return nil
	case sceneReadNative:
	case sceneReadInvalid:
		return ErrCommandAuthorizationDenied
	default:
		return ErrCommandAuthorizationDenied
	}
	if _, err := entity.Value(); err != nil || len(descriptions) == 0 {
		return ErrCommandAuthorizationDenied
	}
	read, err := authorization.NewPermission(authv1.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY, authv1.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return ErrCommandAuthorizationDenied
	}
	for _, description := range descriptions {
		if description == nil {
			return ErrCommandAuthorizationDenied
		}
		slot, err := authorization.NewCapabilitySlot(description.SchemaId, description.Endpoint)
		if err != nil {
			return ErrCommandAuthorizationDenied
		}
		object, err := authorization.EntityObject(entity, slot)
		if err != nil || a.current.Authorize(read, object) != nil {
			return ErrCommandAuthorizationDenied
		}
	}
	return nil
}
func (r *CommandRouter) withSceneRead(ctx context.Context, fn func(SceneReadAccess) error) error {
	if r == nil || r.host == nil || ctx == nil || fn == nil {
		return ErrCommandAuthorizationDenied
	}
	authority := r.host.commandAuthority()
	if authority == nil {
		return fn(SceneReadAccess{mode: sceneReadLegacy})
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return ErrCommandAuthorizationDenied
	}
	return authority.WithCurrent(ctx, principal, func(current authorization.Current) error {
		return fn(SceneReadAccess{mode: sceneReadNative, current: current})
	})
}
func (s *VolatileCommandService) WithSceneRead(ctx context.Context, fn func(SceneReadAccess) error) error {
	if s == nil {
		return ErrCommandAuthorizationDenied
	}
	return s.router.withSceneRead(ctx, fn)
}
func (s *DurableCommandService) WithSceneRead(ctx context.Context, fn func(SceneReadAccess) error) error {
	if s == nil {
		return ErrCommandAuthorizationDenied
	}
	return s.router.withSceneRead(ctx, fn)
}

func (r *CommandRouter) sceneAuthorizationBinding(ctx context.Context) ([32]byte, error) {
	if r == nil || r.host == nil {
		return [32]byte{}, ErrCommandAuthorizationDenied
	}
	authority := r.host.commandAuthority()
	if authority == nil {
		return [32]byte{}, nil
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return [32]byte{}, ErrCommandAuthorizationDenied
	}
	current, err := authority.Current(principal)
	if err != nil {
		return [32]byte{}, ErrCommandAuthorizationDenied
	}
	_, binding, err := current.Binding()
	return binding, err
}
func (s *VolatileCommandService) SceneAuthorizationBinding(ctx context.Context) ([32]byte, error) {
	if s == nil {
		return [32]byte{}, ErrCommandAuthorizationDenied
	}
	return s.router.sceneAuthorizationBinding(ctx)
}
func (s *DurableCommandService) SceneAuthorizationBinding(ctx context.Context) ([32]byte, error) {
	if s == nil {
		return [32]byte{}, ErrCommandAuthorizationDenied
	}
	return s.router.sceneAuthorizationBinding(ctx)
}
func (r *CommandRouter) authorizeSceneEntity(ctx context.Context, entity domain.EntityID) error {
	if r == nil || r.host == nil {
		return ErrCommandAuthorizationDenied
	}
	if r.host.commandAuthority() == nil {
		return nil
	}
	descriptions, err := r.host.catalog.Descriptions(entity)
	if err != nil {
		return ErrCommandAuthorizationDenied
	}
	return r.withSceneRead(ctx, func(access SceneReadAccess) error { return access.AdmitEntity(entity, descriptions) })
}
func (s *VolatileCommandService) AuthorizeSceneEntity(ctx context.Context, entity domain.EntityID) error {
	if s == nil {
		return ErrCommandAuthorizationDenied
	}
	return s.router.authorizeSceneEntity(ctx, entity)
}
func (s *DurableCommandService) AuthorizeSceneEntity(ctx context.Context, entity domain.EntityID) error {
	if s == nil {
		return ErrCommandAuthorizationDenied
	}
	return s.router.authorizeSceneEntity(ctx, entity)
}
