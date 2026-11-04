package server

import (
	"context"

	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/view"
)

func prepareCoreRPC(ctx context.Context, sessions *sessionCoordinator, sequence *sequencer.Sequencer,
	identity integrations.IdentityAuthority, configuration view.Configuration, commands *integrations.DurableCommandService, profiles ...view.DisplayProfileSource) (*coreRPCHandler, error) {
	if nilReactiveInterface(configuration) || commands == nil {
		return nil, ErrInvalidPublicPlan
	}
	composition, err := authorizationComposition(ctx, sessions, sequence, identity, configuration)
	if err != nil {
		return nil, err
	}
	if len(profiles) > 1 {
		return nil, ErrInvalidPublicPlan
	}
	if len(profiles) == 1 && !nilReactiveInterface(profiles[0]) {
		authorizer, ok := composition.ViewAuthorizer().(*policyViewAuthorizer)
		if !ok {
			return nil, ErrInvalidPublicPlan
		}
		authorizer.profiles = profiles[0]
	}
	handler := &coreRPCHandler{commands: commands, epoch: sequence}
	owner, hasResources := identity.(homeResourceOwner)
	var projectors []view.CapabilityProjector
	if hasResources && !nilReactiveInterface(owner) {
		projectors = append(projectors, handler.capabilityProjector(owner, sessions.authority))
	}
	views, err := composition.NewViewService(configuration, sequencer.RealClock{}, projectors...)
	if err != nil {
		return nil, err
	}
	handler.views = views
	if len(projectors) == 1 {
		handler.home, err = newHomeResourceHandler(owner, sessions.authority, views)
		if err != nil {
			return nil, err
		}
	}
	grants, ok := composition.ViewAuthorizer().(view.GrantChangeNotifier)
	if !ok || nilReactiveInterface(grants) {
		return nil, ErrInvalidPublicPlan
	}
	handler.grants = grants
	return handler, nil
}
