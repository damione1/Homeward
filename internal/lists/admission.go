package lists

import (
	"context"
	"sync"

	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
)

// Admission keeps the existing object's authority alive during representation
// admission. It is not a commit proof. Mutations still obtain their own current
// permit and execute through the repository's account/policy SQL fence.
type Admission struct {
	permit  *authorization.Permit
	service *Service
	cancel  context.CancelFunc
	stop    func() bool
	once    sync.Once
}

func (a *Admission) Context() context.Context {
	if a == nil || a.permit == nil {
		return nil
	}
	return a.permit.Context()
}
func (a *Admission) Ready() bool { return a != nil && a.permit != nil && a.permit.Ready() }
func (a *Admission) Release() {
	if a == nil {
		return
	}
	a.once.Do(func() {
		if a.permit != nil {
			a.permit.Release()
		}
		if a.stop != nil {
			a.stop()
		}
		if a.cancel != nil {
			a.cancel()
		}
		if a.service != nil {
			a.service.leave()
		}
	})
}

func (s *Service) admit(ctx context.Context, op authv1.ScopeOperation, object authorization.Object) (*Admission, error) {
	if ctx == nil {
		return nil, failure(Invalid)
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return nil, failure(Denied)
	}
	if err := s.enter(); err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	permission, err := listPermission(op)
	if err == nil {
		var permit *authorization.Permit
		permit, err = s.authority.Permit(callCtx, principal, permission, object)
		if err == nil {
			return &Admission{permit: permit, service: s, cancel: cancel, stop: stop}, nil
		}
	}
	stop()
	cancel()
	s.leave()
	return nil, failure(Denied)
}

// AdmitObject checks rights without reading list existence, revision or content.
// The caller must retain Admission until validation/result selection is complete.
func (s *Service) AdmitObject(ctx context.Context, id domain.ListID, op authv1.ScopeOperation) (*Admission, error) {
	object, err := listObject(id)
	if err != nil {
		return nil, failure(Denied)
	}
	return s.admit(ctx, op, object)
}

// AdmitCollection lets a caller with parent-wide rights receive a precise bad
// identity diagnostic. An object-limited caller instead gets the uniform deny.
func (s *Service) AdmitCollection(ctx context.Context, op authv1.ScopeOperation) (*Admission, error) {
	return s.admit(ctx, op, collection())
}

// CursorBinding binds network continuations to the current native credential
// and committed rights. Call before entering a SQL transaction; this method
// never runs under account/policy or owner SQL locks.
func (s *Service) CursorBinding(ctx context.Context) ([32]byte, error) {
	if s == nil || s.authority == nil {
		return [32]byte{}, failure(Denied)
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return [32]byte{}, failure(Denied)
	}
	current, err := s.authority.Current(principal)
	if err != nil {
		return [32]byte{}, failure(Denied)
	}
	_, binding, err := current.Binding()
	return binding, err
}
