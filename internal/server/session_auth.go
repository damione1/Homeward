package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/authorization"
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
	if request == nil || request.TLS == nil || ctx == nil {
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	return a.authenticateAccessHeaders(context.WithValue(ctx, nativeTLSContextKey{}, nativeTLSAdmission{}), request.Header)
}

// Only the common HTTP guard or AuthenticateWithLease can attest actual TLS.
// Anonymous enrollment routes use this same access admission, never a fake
// http.Request/TLS state or a second credential parser.
func (a *sessionAuth) authenticateAccessHeaders(ctx context.Context, header http.Header) (domain.ActorID, context.Context, func(), error) {
	if a == nil || a.store == nil || a.coordinator == nil || ctx == nil {
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	if _, ok := ctx.Value(nativeTLSContextKey{}).(nativeTLSAdmission); !ok {
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	values := header.Values("Authorization")
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
	switch admission.Role {
	case authv1.AccountRole_ACCOUNT_ROLE_ADMIN:
		ticket, err := a.coordinator.policy.tryEnter(admission.PolicyVersion)
		if err != nil {
			return domain.ActorID{}, nil, nil, err
		}
		defer ticket.release()
	case authv1.AccountRole_ACCOUNT_ROLE_USER:
	default:
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	if err := guard.refreshAuthorization(ctx); err != nil {
		guard.quarantine()
		guard.cancelLeases(ctx, nil)
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	principal, err := authorization.NativePrincipal(owner.Account, owner.Session)
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
	leaseCtx, err = authorization.WithPrincipal(leaseCtx, principal)
	if err != nil {
		lease.release()
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	timer := a.coordinator.clock.At(deadline, func() {
		lease.terminate(authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_CREDENTIAL_EXPIRED)
	})
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
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return authorization.ErrDenied
	}
	principalActor, err := principal.Actor()
	if err != nil || principalActor != actor {
		return authorization.ErrDenied
	}
	current, err := a.coordinator.authority.Current(principal)
	if err != nil {
		return err
	}
	permission, self, known := nativeRoutePermission(route)
	if !known {
		return authorization.ErrDenied
	}
	if self {
		if !principal.Native() {
			return authorization.ErrDenied
		}
		return nil
	}
	return current.AdmitRoute(permission)
}
