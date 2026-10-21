package integrations

import (
	"context"
	"sync"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
)

// AuthorizedActionRead binds the historical external intention to a revocable
// current read permit. It grants no dispatch or internal automation authority.
type AuthorizedActionRead struct {
	mu           sync.Mutex
	service      *DurableCommandService
	id           domain.CommandID
	status       DurableCommandStatus
	permit       *authorization.Permit
	changes      *authorization.Subscription
	subscription *ActionSubscription
	closed       bool
	once         sync.Once
}

func (s *DurableCommandService) AuthorizeActionRead(ctx context.Context, id domain.CommandID) (*AuthorizedActionRead, error) {
	if s == nil || nilInterface(ctx) || s.router == nil || s.store == nil {
		return nil, ErrCommandAuthorizationDenied
	}
	if _, err := id.Value(); err != nil {
		return nil, ErrCommandAuthorizationDenied
	}
	authority := s.router.host.commandAuthority()
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil || authority == nil {
		return nil, ErrCommandAuthorizationDenied
	}
	// Register before the first SQL/status read; a lost idle policy change must
	// never leave a public reader with unobserved historical authorization.
	changes, err := authority.Subscribe(ctx, principal)
	if err != nil {
		return nil, ErrCommandAuthorizationDenied
	}
	read := &AuthorizedActionRead{service: s, id: id, changes: changes}
	defer func() {
		if read.permit == nil {
			read.Close()
		}
	}()
	record, found, err := s.store.CommandIntent(ctx, id)
	if err != nil {
		return nil, ErrDurableLedgerUnavailable
	}
	if !found || record.OriginKind() != storage.IntentOriginExternalActor {
		return nil, ErrCommandAuthorizationDenied
	}
	target, present, err := record.AuthorizationTarget()
	if err != nil || s.router.authorizeStored(ctx, record.Fingerprint(), target, present) != nil {
		return nil, ErrCommandAuthorizationDenied
	}
	object, err := target.Object()
	if err != nil {
		return nil, ErrCommandAuthorizationDenied
	}
	permission, err := authorization.NewPermission(authv1.ScopeFamily_SCOPE_FAMILY_ACTIONS, authv1.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return nil, ErrCommandAuthorizationDenied
	}
	permit, err := authority.Permit(ctx, principal, permission, object)
	if err != nil {
		return nil, ErrCommandAuthorizationDenied
	}
	response, err := storedCommandResponse(id, record)
	if err != nil {
		permit.Release()
		return nil, err
	}
	read.status = DurableCommandStatus{State: record.State(), Response: response, ErrorKind: record.ErrorKind()}
	read.permit = permit
	return read, nil
}

func (r *AuthorizedActionRead) Ready() bool {
	if r == nil || r.permit == nil {
		return false
	}
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	return !closed && r.permit.Ready()
}
func (r *AuthorizedActionRead) Context() context.Context {
	if r == nil || r.permit == nil {
		return nil
	}
	return r.permit.Context()
}
func (r *AuthorizedActionRead) Durable() (DurableCommandStatus, error) {
	if !r.Ready() {
		return DurableCommandStatus{}, ErrCommandAuthorizationDenied
	}
	status := r.status
	if status.Response != nil {
		status.Response = proto.Clone(status.Response).(*actions.CommandResponse)
	}
	return status, nil
}
func (r *AuthorizedActionRead) Live() (ActionSnapshot, error) {
	if r == nil || r.permit == nil {
		return ActionSnapshot{}, ErrCommandAuthorizationDenied
	}
	var snapshot ActionSnapshot
	err := r.permit.WithCurrent(func() error {
		var err error
		snapshot, err = r.service.router.host.ActionStatus(r.id)
		return err
	})
	if err != nil && !r.Ready() {
		return ActionSnapshot{}, ErrCommandAuthorizationDenied
	}
	return snapshot, err
}
func (r *AuthorizedActionRead) Subscribe() (*AuthorizedActionSubscription, error) {
	if r == nil || r.permit == nil {
		return nil, ErrCommandAuthorizationDenied
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.subscription != nil {
		return nil, ErrCommandAuthorizationDenied
	}
	var subscription *ActionSubscription
	err := r.permit.WithCurrent(func() error {
		var err error
		subscription, err = r.service.router.host.SubscribeAction(r.id)
		return err
	})
	if err != nil {
		return nil, err
	}
	r.subscription = subscription
	return &AuthorizedActionSubscription{read: r, source: subscription}, nil
}
func (r *AuthorizedActionRead) Close() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.mu.Lock()
		r.closed = true
		subscription := r.subscription
		r.mu.Unlock()
		if subscription != nil {
			subscription.Close()
		}
		if r.permit != nil {
			r.permit.Release()
		}
		if r.changes != nil {
			r.changes.Close()
		}
	})
}

type AuthorizedActionSubscription struct {
	read   *AuthorizedActionRead
	source *ActionSubscription
}

func (s *AuthorizedActionSubscription) Next() (ActionSnapshot, error) {
	if s == nil || s.read == nil || s.source == nil || !s.read.Ready() {
		return ActionSnapshot{}, ErrCommandAuthorizationDenied
	}
	snapshot, err := s.source.Next(s.read.Context())
	if !s.read.Ready() {
		return ActionSnapshot{}, ErrCommandAuthorizationDenied
	}
	return snapshot, err
}
func (s *AuthorizedActionSubscription) Close() {
	if s != nil && s.source != nil {
		s.source.Close()
	}
}
