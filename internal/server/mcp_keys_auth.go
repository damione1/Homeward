package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/mcpkeys"
	"github.com/damione1/Homeward/internal/storage"
)

type mcpRouteContextKey struct{}

func mcpBearer(header http.Header) bool {
	values := header.Values("Authorization")
	return len(values) == 1 && len(values[0]) >= 7 && strings.EqualFold(values[0][:6], "Bearer") && values[0][6] == ' ' && strings.HasPrefix(values[0][7:], mcpkeys.Prefix)
}
func (a *sessionAuth) authenticateMCP(ctx context.Context, request *http.Request) (domain.ActorID, context.Context, func(), error) {
	if a == nil || a.store == nil || a.coordinator == nil || ctx == nil || request == nil || request.TLS == nil {
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	route, ok := ctx.Value(mcpRouteContextKey{}).(RouteKey)
	if !ok || route.Kind() != RouteKindMCP {
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	values := request.Header.Values("Authorization")
	if !mcpBearer(request.Header) {
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	secret, err := mcpkeys.ParseSecret(values[0][7:])
	if err != nil {
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	digest, err := secret.Digest()
	if err != nil {
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	owner, err := a.store.LookupMCPKeyOwner(ctx, digest)
	if err != nil {
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	guard, err := a.coordinator.enter(ctx, owner.Account)
	if err != nil {
		return domain.ActorID{}, nil, nil, err
	}
	defer guard.release()
	record, err := a.store.AdmitMCPKey(ctx, owner, digest, a.coordinator.clock.Now())
	if err != nil {
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	if err := guard.refreshAuthorization(ctx); err != nil {
		guard.quarantine()
		guard.cancelLeases(ctx, nil)
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	scopes, err := authorization.NewScopeSet(record.Scopes)
	if err != nil {
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	principal, err := authorization.ScopedPrincipal(owner.Account, owner.Key, scopes)
	if err != nil {
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	actor, err := principal.Actor()
	if err != nil {
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	current, err := a.coordinator.authority.Current(principal)
	if err != nil {
		return domain.ActorID{}, nil, nil, err
	}
	_, binding, err := current.Binding()
	if err != nil {
		return domain.ActorID{}, nil, nil, err
	}
	subscription, err := a.coordinator.authority.Subscribe(ctx, principal)
	if err != nil {
		return domain.ActorID{}, nil, nil, err
	}
	// SQL admission and the authorization refresh can outlast the credential.
	// A past-due timer fires asynchronously, so reject before admitting a lease.
	if !a.coordinator.clock.Now().Before(record.ExpiresAt) {
		subscription.Close()
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	leaseCtx, lease := guard.register(ctx, storage.SessionOwner{Account: owner.Account})
	lease.mcpKey = owner.Key
	lease.mcpPrincipal = principal
	lease.mcpBinding = binding
	leaseCtx, err = authorization.WithPrincipal(leaseCtx, principal)
	if err != nil {
		subscription.Close()
		lease.release()
		return domain.ActorID{}, nil, nil, err
	}
	timer := a.coordinator.clock.At(record.ExpiresAt, func() {
		lease.terminate(authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_CREDENTIAL_EXPIRED)
	})
	if timer == nil || typedNilPublic(timer) || leaseCtx.Err() != nil {
		if timer != nil && !typedNilPublic(timer) {
			timer.Stop()
		}
		subscription.Close()
		lease.release()
		return domain.ActorID{}, nil, nil, errSessionUnauthenticated
	}
	// The existing authority signals committed rights mutations. A changed
	// binding permanently closes this lease even if a right is later regranted.
	go func() {
		defer subscription.Close()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-subscription.Changes():
				latest, e := a.coordinator.authority.Current(principal)
				if e != nil {
					lease.terminate(authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_CREDENTIAL_REVOKED)
					return
				}
				_, next, e := latest.Binding()
				if e != nil || next != binding {
					lease.terminate(authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_CREDENTIAL_REVOKED)
					return
				}
			}
		}
	}()
	return actor, leaseCtx, func() { timer.Stop(); lease.release() }, nil
}
func (a *sessionAuth) authorizeMCP(ctx context.Context, actor domain.ActorID, route RouteKey) error {
	if route.Kind() != RouteKindMCP {
		return errSessionUnauthenticated
	}
	lease, ok := ctx.Value(sessionLeaseContextKey{}).(*sessionLease)
	if !ok || lease == nil {
		return errSessionUnauthenticated
	}
	if _, err := lease.mcpKey.Value(); err != nil {
		return errSessionUnauthenticated
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil || principal.Native() {
		return errSessionUnauthenticated
	}
	actual, err := principal.Actor()
	if err != nil || actual != actor {
		return errSessionUnauthenticated
	}
	permission, _, known := nativeRoutePermission(route)
	if !known {
		return authorization.ErrDenied
	}
	current, err := a.coordinator.authority.Current(principal)
	if err != nil {
		return err
	}
	return current.AdmitRoute(permission)
}
func (g *sessionGuard) cancelMCPLeases(id contracts.McpKeyID) {
	var selected []*sessionLease
	g.coordinator.mu.Lock()
	for lease := range g.gate.leases {
		if lease.mcpKey == id {
			selected = append(selected, lease)
		}
	}
	g.coordinator.mu.Unlock()
	for _, lease := range selected {
		lease.terminate(authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_CREDENTIAL_REVOKED)
	}
}

// Called under the existing account mutation gate after a confirmed rights
// change, before returning success or releasing that gate.
func (g *sessionGuard) cancelAllMCPLeases() {
	var selected []*sessionLease
	g.coordinator.mu.Lock()
	for lease := range g.gate.leases {
		if _, err := lease.mcpKey.Value(); err == nil {
			selected = append(selected, lease)
		}
	}
	g.coordinator.mu.Unlock()
	for _, lease := range selected {
		lease.terminate(authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_CREDENTIAL_REVOKED)
	}
}

// Runs under the account gate (and the mutation barrier for rights changes),
// before Authority.Publish. It performs no SQL and never waits on a watcher.
func (g *sessionGuard) cancelChangedMCPLeases(next authorization.AccountState) {
	var selected []*sessionLease
	g.coordinator.mu.Lock()
	for lease := range g.gate.leases {
		if _, err := lease.mcpKey.Value(); err != nil {
			continue
		}
		binding, err := next.CredentialBinding(lease.mcpPrincipal)
		if err != nil || binding != lease.mcpBinding {
			selected = append(selected, lease)
		}
	}
	g.coordinator.mu.Unlock()
	for _, lease := range selected {
		lease.terminate(authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_RIGHTS_CHANGED)
	}
}
