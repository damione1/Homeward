package server

import (
	"context"
	"sync"
	"time"

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
)

// A physical intention has a short deadline. A retained credential cannot turn
// an HTTP request into an unbounded background task or a durable retry queue.
const maximumOperationLease = time.Minute

// retainOperationLease carries the authenticated credential into one admitted
// operation. It retains its account/session or MCP key, expiry and revocation
// registration, independently of the HTTP request that returns the handle.
// It neither authorizes an object nor admits an effect. Admission must still
// use the original request context; only a confirmed admitted occurrence may
// continue on the returned context. Native dispatcher ownership stays unchanged.
func retainOperationLease(ctx context.Context, until time.Time) (context.Context, func(), error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, nil, authorization.ErrDenied
	}
	source, ok := ctx.Value(sessionLeaseContextKey{}).(*sessionLease)
	if !ok || source == nil || source.coordinator == nil || source.gate == nil || source.deadline.IsZero() {
		return nil, nil, authorization.ErrDenied
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return nil, nil, authorization.ErrDenied
	}
	account, err := principal.Account()
	if err != nil || account != source.owner.Account {
		return nil, nil, authorization.ErrDenied
	}
	coordinator := source.coordinator
	guard, err := coordinator.enter(ctx, account)
	if err != nil {
		return nil, nil, err
	}
	defer guard.release()
	coordinator.mu.Lock()
	_, registered := guard.gate.leases[source]
	coordinator.mu.Unlock()
	if !registered || source.gate != guard.gate || ctx.Err() != nil {
		return nil, nil, authorization.ErrDenied
	}
	now := coordinator.clock.Now()
	if !now.Before(source.deadline) || !now.Before(until) || until.After(now.Add(maximumOperationLease)) {
		return nil, nil, authorization.ErrDenied
	}
	current, err := coordinator.authority.Current(principal)
	if err != nil {
		return nil, nil, err
	}
	sourcePrincipal := source.mcpPrincipal
	if principal.Native() {
		sourcePrincipal = source.nativePrincipal
	}
	sourceCurrent, err := coordinator.authority.Current(sourcePrincipal)
	if err != nil {
		return nil, nil, authorization.ErrDenied
	}
	_, expected, err := sourceCurrent.Binding()
	if err != nil {
		return nil, nil, authorization.ErrDenied
	}
	if !principal.Native() && source.mcpBinding != expected {
		return nil, nil, authorization.ErrDenied
	}
	_, actual, err := current.Binding()
	if err != nil || expected != actual {
		return nil, nil, authorization.ErrDenied
	}
	retained, lease := guard.register(context.WithoutCancel(ctx), source.owner)
	lease.deadline = source.deadline
	lease.nativePrincipal = source.nativePrincipal
	lease.mcpKey, lease.mcpPrincipal, lease.mcpBinding = source.mcpKey, source.mcpPrincipal, source.mcpBinding
	deadline := source.deadline
	credentialExpiry := !until.Before(deadline)
	if !credentialExpiry {
		deadline = until
	}
	timer := coordinator.clock.At(deadline, func() {
		if credentialExpiry {
			lease.terminate(auth.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_CREDENTIAL_EXPIRED)
		} else {
			lease.cancelCause(context.DeadlineExceeded)
		}
	})
	if timer == nil || typedNilPublic(timer) || ctx.Err() != nil || retained.Err() != nil {
		if timer != nil && !typedNilPublic(timer) {
			timer.Stop()
		}
		lease.release()
		return nil, nil, authorization.ErrDenied
	}
	var once sync.Once
	return retained, func() { once.Do(func() { timer.Stop(); lease.release() }) }, nil
}
