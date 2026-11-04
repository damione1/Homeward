package server

import "github.com/damione1/Homeward/internal/storage"

func composeSessionsWithCoordinator(plan *PublicPlan, store *storage.Store, coordinator *sessionCoordinator, names administrationNames) (*PublicPlan, *sessionCoordinator, error) {
	if store == nil || coordinator == nil {
		return nil, nil, ErrInvalidPublicPlan
	}
	var err error
	var provider factorProvider
	if coordinator.mfa != nil {
		provider = coordinator.mfa
	}
	plan, err = registerAuthRoutes(plan, store, coordinator, provider, names)
	if err != nil {
		return nil, nil, err
	}
	plan, err = registerAccountRoutes(plan, store, coordinator, names)
	if err != nil {
		return nil, nil, err
	}
	if err := registerAuthorizationPolicyRoutes(plan, store, coordinator, names); err != nil {
		return nil, nil, err
	}
	if err := registerMCPKeyRoutes(plan, store, coordinator, names); err != nil {
		return nil, nil, err
	}
	return plan, coordinator, nil
}
