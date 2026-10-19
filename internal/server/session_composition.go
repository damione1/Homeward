package server

import "github.com/damione1/Homeward/internal/storage"

// composeLocalSessions is the single pre-freeze seam for RunWithFactory. The
// same returned coordinator must also be passed to startOperatorListener.
func composeLocalSessions(plan *PublicPlan, store *storage.Store) (*PublicPlan, *sessionCoordinator, error) {
	var clock sessionClock = realSessionClock{}
	if plan != nil && plan.sessionClock != nil {
		clock = plan.sessionClock
	}
	coordinator, err := newSessionCoordinatorWithClock(store, clock)
	if err != nil {
		return nil, nil, err
	}
	return composeSessionsWithCoordinator(plan, store, coordinator)
}

func composeSessionsWithCoordinator(plan *PublicPlan, store *storage.Store, coordinator *sessionCoordinator) (*PublicPlan, *sessionCoordinator, error) {
	if store == nil || coordinator == nil {
		return nil, nil, ErrInvalidPublicPlan
	}
	var err error
	plan, err = registerAuthRoutes(plan, store, coordinator, nil)
	if err != nil {
		return nil, nil, err
	}
	plan, err = registerAccountRoutes(plan, store, coordinator)
	if err != nil {
		return nil, nil, err
	}
	if err := registerAuthorizationPolicyRoutes(plan, store, coordinator); err != nil {
		return nil, nil, err
	}
	return plan, coordinator, nil
}
