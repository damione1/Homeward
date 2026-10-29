package server

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/gen/home/actions/v1/actionsv1connect"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/state/v1/statev1connect"
	"github.com/damione1/Homeward/internal/authorization"
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
	views, err := composition.NewViewService(configuration, sequencer.RealClock{})
	if err != nil {
		return nil, err
	}
	grants, ok := composition.ViewAuthorizer().(view.GrantChangeNotifier)
	if !ok || nilReactiveInterface(grants) {
		return nil, ErrInvalidPublicPlan
	}
	return &coreRPCHandler{views: views, grants: grants, commands: commands, epoch: sequence}, nil
}

func registerCoreRPCRoutes(plan *PublicPlan, handler *coreRPCHandler) error {
	if plan == nil || handler == nil || handler.views == nil || nilReactiveInterface(handler.grants) || handler.commands == nil || nilReactiveInterface(handler.epoch) {
		return ErrInvalidPublicPlan
	}
	stateRoutes := []ConnectProcedure{
		{Path: statev1connect.StateServiceOpenViewProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		{Path: statev1connect.StateServiceReadPageProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		{Path: statev1connect.StateServiceReleaseViewProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		{Path: statev1connect.StateServiceWatchViewProcedure, Access: RouteAccessProtected, Flow: RouteFlowServerStream, Transport: RouteTransportTLSRequired},
	}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return statev1connect.NewStateServiceHandler(handler, options...)
	}, stateRoutes...); err != nil {
		return err
	}
	for _, route := range stateRoutes {
		permission, err := authorization.NewPermission(authv1.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY, authv1.ScopeOperation_SCOPE_OPERATION_READ)
		if err != nil {
			return err
		}
		if err := plan.BindPermission(RouteKindConnect, RouteMethodPost, route.Path, permission); err != nil {
			return err
		}
	}
	actionRoutes := []struct {
		path      string
		operation authv1.ScopeOperation
		flow      RouteFlow
	}{
		{actionsv1connect.ActionServiceSubmitActionProcedure, authv1.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
		{actionsv1connect.ActionServiceGetActionProcedure, authv1.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
		{actionsv1connect.ActionServiceWatchActionProcedure, authv1.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowServerStream},
	}
	procedures := make([]ConnectProcedure, 0, len(actionRoutes))
	for _, route := range actionRoutes {
		procedures = append(procedures, ConnectProcedure{Path: route.path, Access: RouteAccessProtected, Flow: route.flow, Transport: RouteTransportTLSRequired})
	}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return actionsv1connect.NewActionServiceHandler(handler, options...)
	}, procedures...); err != nil {
		return err
	}
	for _, route := range actionRoutes {
		permission, err := authorization.NewPermission(authv1.ScopeFamily_SCOPE_FAMILY_ACTIONS, route.operation)
		if err != nil {
			return err
		}
		if err := plan.BindPermission(RouteKindConnect, RouteMethodPost, route.path, permission); err != nil {
			return err
		}
	}
	return nil
}
