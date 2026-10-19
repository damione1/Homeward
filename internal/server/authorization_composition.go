package server

import (
	"context"
	"errors"

	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/view"
)

// AuthorizationComposition exposes the native authority to existing domain
// services without inventing a public StateService façade. Run supplies this
// exact adapter, sharing the session, policy-edit and command authority.
type AuthorizationComposition interface {
	ViewAuthorizer() view.Authorizer
	NewViewService(view.Configuration, sequencer.Clock) (*view.Service, error)
}
type runtimeAuthorizationComposition struct{ views *policyViewAuthorizer }

func (c runtimeAuthorizationComposition) ViewAuthorizer() view.Authorizer { return c.views }
func (c runtimeAuthorizationComposition) NewViewService(config view.Configuration, clock sequencer.Clock) (*view.Service, error) {
	if c.views == nil {
		return nil, authorization.ErrInvalid
	}
	return view.NewService(c.views.source, c.views, config, clock)
}
func authorizationComposition(ctx context.Context, c *sessionCoordinator, source *sequencer.Sequencer, identity integrations.IdentityAuthority) (AuthorizationComposition, error) {
	if c == nil || c.authority == nil || source == nil {
		return nil, authorization.ErrInvalid
	}
	snapshot, err := source.Capture()
	if err != nil {
		return nil, err
	}
	if !snapshot.RegistryReady {
		projection, ok := identity.(interface {
			AttachProjection(context.Context, integrations.ProjectionSink) error
		})
		if !ok || nilReactiveInterface(projection) {
			return nil, authorization.ErrUnavailable
		}
		if err := projection.AttachProjection(ctx, source); err != nil {
			return nil, err
		}
	}
	return runtimeAuthorizationComposition{views: &policyViewAuthorizer{authority: c.authority, source: source}}, nil
}

// prepareLocalAuthorization runs before Host callbacks and admissions. It is
// also valid with zero accounts: the first Unix bootstrap publishes its newly
// committed seed through the same coordinator without restarting Run.
func prepareLocalAuthorization(ctx context.Context, store *storage.Store, clock sessionClock) (*sessionCoordinator, error) {
	c, err := newSessionCoordinatorWithClock(store, clock)
	if err != nil {
		return nil, err
	}
	if err := store.EnsureAuthorizationPolicies(ctx); err != nil {
		return nil, err
	}
	err = c.authority.HydrateFrom(func(yield func(authorization.AccountState) error) error {
		return store.ForEachAuthorizationPolicy(ctx, yield)
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// refreshAuthorization is called while holding the account gate, after the
// transaction is settled, and never under Host/generation/sequencer locks.
func (g *sessionGuard) refreshAuthorization(ctx context.Context) error {
	if g == nil || g.coordinator == nil || g.coordinator.authority == nil {
		return storage.ErrAccountUnavailable
	}
	snapshot, err := g.coordinator.store.AuthorizationPolicy(ctx, g.gate.account)
	if errors.Is(err, storage.ErrAccountNotFound) {
		return g.coordinator.authority.RemoveAccount(g.gate.account)
	}
	if err != nil {
		return err
	}
	state, err := snapshot.State()
	if err != nil {
		return err
	}
	if status, err := state.Status(); err == nil && status == authv1.AccountStatus_ACCOUNT_STATUS_DISABLED {
		g.termination = authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_ACCOUNT_DISABLED
	}
	return g.coordinator.authority.Publish(state)
}
