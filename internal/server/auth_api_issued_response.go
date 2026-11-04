package server

import (
	"context"
	"time"

	native "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/storage"
)

// retainIssuedSession runs under the native account gate after confirmed issuance
// and metadata projection. It leases only delivery of that actual session. No
// incoming principal or permission is minted for an anonymous login/refresh.
func (g *sessionGuard) retainIssuedSession(ctx context.Context, record storage.SessionRecord, accessExpiry time.Time) error {
	if _, scoped := ctx.Value(publicResponseScopeKey{}).(*publicResponseScope); !scoped {
		return nil
	}
	account, accountErr := record.AccountID()
	session, sessionErr := record.ID()
	if g == nil || g.gate == nil || accountErr != nil || sessionErr != nil || account != g.gate.account || record.RevokedAt() != nil {
		return storage.ErrSessionUnavailable
	}
	deadline := accessExpiry
	if record.ExpiresAt().Before(deadline) {
		deadline = record.ExpiresAt()
	}
	if !g.coordinator.clock.Now().Before(deadline) {
		return storage.ErrSessionExpired
	}
	leaseCtx, lease := g.register(ctx, storage.SessionOwner{Account: account, Session: session})
	lease.deadline = deadline
	timer := g.coordinator.clock.At(deadline, func() {
		lease.terminate(native.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_CREDENTIAL_EXPIRED)
	})
	if timer == nil || typedNilPublic(timer) || leaseCtx.Err() != nil {
		if timer != nil && !typedNilPublic(timer) {
			timer.Stop()
		}
		lease.release()
		return storage.ErrSessionUnavailable
	}
	release := func() { timer.Stop(); lease.release() }
	retained, err := retainPublicResponse(ctx, leaseCtx, func() bool {
		return leaseCtx.Err() == nil && g.coordinator.clock.Now().Before(deadline)
	}, release)
	if !retained {
		release()
	}
	return err
}
