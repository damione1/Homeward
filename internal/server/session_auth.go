package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
)

var errSessionUnauthenticated = errors.New("server: unauthenticated session")

type sessionAuth struct {
	store       *storage.Store
	coordinator *sessionCoordinator
}

var _ Authenticator = (*sessionAuth)(nil)
var _ LeasedAuthenticator = (*sessionAuth)(nil)
var _ Authorizer = (*sessionAuth)(nil)

// The built-in provider must be used through the lease-aware transport path.
func (*sessionAuth) Authenticate(context.Context, *http.Request) (domain.ActorID, error) {
	return domain.ActorID{}, errSessionUnauthenticated
}

func (a *sessionAuth) AuthenticateWithLease(ctx context.Context, request *http.Request) (domain.ActorID, context.Context, func(), error) {
	if a == nil || a.store == nil || a.coordinator == nil || ctx == nil || request == nil {
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	if request.TLS == nil {
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	values := request.Header.Values("Authorization")
	if len(values) != 1 || len(values[0]) < 8 || !strings.EqualFold(values[0][:6], "Bearer") || values[0][6] != ' ' {
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	secret, err := accounts.ParseAccessSecret(values[0][7:])
	if err != nil {
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	digest, err := secret.Digest()
	if err != nil {
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	owner, err := a.store.LookupAccessOwner(ctx, digest)
	if err != nil {
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	guard, err := a.coordinator.enter(ctx, owner.Account)
	if err != nil {
		return domain.ActorID{}, nil, nil, err
	}
	defer guard.release()
	admission, err := a.store.AdmitAccess(ctx, owner, digest, a.coordinator.clock.Now())
	if err != nil {
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	account, err := owner.Account.Value()
	if err != nil {
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	actor, err := domain.ParseActorID(account)
	if err != nil {
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	deadline := admission.AccessExpiresAt
	if admission.SessionExpiresAt.Before(deadline) {
		deadline = admission.SessionExpiresAt
	}
	if !a.coordinator.clock.Now().Before(deadline) {
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	leaseCtx, lease := guard.register(ctx, owner)
	timer := a.coordinator.clock.At(deadline, lease.cancel)
	if timer == nil || typedNilPublic(timer) || leaseCtx.Err() != nil {
		if timer != nil && !typedNilPublic(timer) {
			timer.Stop()
		}
		lease.release()
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	return actor, leaseCtx, func() { timer.Stop(); lease.release() }, nil
}

func (a *sessionAuth) Authorize(ctx context.Context, actor domain.ActorID, route RouteKey) error {
	if ctx == nil || ctx.Err() != nil || route.Path() == "" || route.Kind() == RouteKindMCP {
		return errSessionUnauthenticated
	}
	lease, ok := ctx.Value(sessionLeaseContextKey{}).(*sessionLease)
	if !ok || lease == nil {
		return errSessionUnauthenticated
	}
	account, err := lease.owner.Account.Value()
	if err != nil {
		return errSessionUnauthenticated
	}
	value, err := actor.Value()
	if err != nil || value != account {
		return errSessionUnauthenticated
	}
	return nil
}
