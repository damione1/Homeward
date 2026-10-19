package server

import (
	"context"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authmfa"
)

func (f *mfaPolicyFence) status() mfaPolicyFenceState {
	if f == nil {
		return mfaPolicyFenceInvalid
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

// Called only before taking an account gate. Admin bearer admission uses
// tryEnter directly after its SQL read and never waits/reconciles under a gate.
func (c *sessionCoordinator) enterMFAPolicy(ctx context.Context) (*mfaPolicyTicket, error) {
	if c == nil || c.store == nil || c.policy == nil {
		return nil, authmfa.NewError(authmfa.ErrorUnavailable)
	}
	if err := c.reconcileMFAPolicy(ctx); err != nil {
		return nil, err
	}
	current, err := c.store.MFAPolicy(ctx)
	if err != nil {
		return nil, err
	}
	version, err := current.Version()
	if err != nil {
		return nil, err
	}
	return c.policy.tryEnter(version)
}

func (c *sessionCoordinator) reconcileMFAPolicy(ctx context.Context) error {
	switch c.policy.status() {
	case mfaPolicyFenceOpen:
		return nil
	case mfaPolicyFenceChanging:
		return authmfa.NewError(authmfa.ErrorBusy)
	case mfaPolicyFenceUnknown:
	case mfaPolicyFenceClosed, mfaPolicyFenceInvalid:
		return authmfa.NewError(authmfa.ErrorUnavailable)
	default:
		return authmfa.NewError(authmfa.ErrorIntegrity)
	}
	owner, known, err := c.policy.beginReconcile(ctx)
	if err != nil {
		return err
	}
	defer owner.abandon()
	current, err := c.store.ReconcileMFAPolicyPrimary(owner.ctx, known)
	if err == nil {
		err = c.publishMFAPolicyAccounts(owner.ctx, known)
	}
	if err != nil {
		_ = owner.setUnknown(known)
		c.quarantineMFAPolicyAccounts(known)
		return err
	}
	version, err := current.Version()
	if err == nil {
		err = owner.publish(version)
	}
	if err != nil {
		_ = owner.setUnknown(known)
		c.quarantineMFAPolicyAccounts(known)
	}
	return err
}

func (c *sessionCoordinator) publishMFAPolicyAccounts(ctx context.Context, known []contracts.AccountID) error {
	for _, id := range known {
		if err := ctx.Err(); err != nil {
			return err
		}
		guard, err := c.enter(ctx, id)
		if err != nil {
			return err
		}
		err = guard.reconcilePrimary(ctx)
		if err == nil {
			guard.cancelLeases(ctx, nil)
		}
		guard.release()
		if err != nil {
			return err
		}
	}
	return nil
}

// Unknown outcomes quarantine only the explicit SQL cohort. No SQL occurs
// here; leases are selected under the directory mutex and cancelled outside it.
func (c *sessionCoordinator) quarantineMFAPolicyAccounts(known []contracts.AccountID) {
	for _, id := range known {
		_ = c.authority.QuarantineAccount(id)
		key, err := id.Value()
		if err != nil {
			continue
		}
		var leases []*sessionLease
		c.mu.Lock()
		if gate := c.accounts[key]; gate != nil {
			gate.quarantined = true
			for lease := range gate.leases {
				leases = append(leases, lease)
			}
		}
		c.mu.Unlock()
		for _, lease := range leases {
			lease.terminate(authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_AUTHORITY_UNAVAILABLE)
		}
	}
}
