package server

import (
	"net/http"

	"connectrpc.com/connect"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/lists/v1/listsv1connect"
	"github.com/damione1/Homeward/internal/authorization"
)

type listRoute struct {
	path string
	op   authv1.ScopeOperation
	flow RouteFlow
}

// Every public list method has an exact, protected Connect route. The route
// permission is a coarse ceiling; the lists Service rechecks the actual object.
var listRoutes = [...]listRoute{
	{listsv1connect.ListServiceListListsProcedure, authv1.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
	{listsv1connect.ListServiceCountListsProcedure, authv1.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
	{listsv1connect.ListServiceGetListProcedure, authv1.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
	{listsv1connect.ListServiceCreateListProcedure, authv1.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
	{listsv1connect.ListServiceRenameListProcedure, authv1.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
	{listsv1connect.ListServiceDeleteListProcedure, authv1.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
	{listsv1connect.ListServiceListItemsProcedure, authv1.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
	{listsv1connect.ListServiceCountItemsProcedure, authv1.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
	{listsv1connect.ListServiceGetItemProcedure, authv1.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
	{listsv1connect.ListServiceAddItemProcedure, authv1.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
	{listsv1connect.ListServiceEditItemProcedure, authv1.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
	{listsv1connect.ListServiceDeleteItemProcedure, authv1.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
	{listsv1connect.ListServiceSetCompletedProcedure, authv1.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
	{listsv1connect.ListServiceMoveItemProcedure, authv1.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
	{listsv1connect.ListServiceWatchListsProcedure, authv1.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowServerStream},
}

func registerListRoutes(plan *PublicPlan, handler *listHandler) error {
	if plan == nil || handler == nil {
		return ErrInvalidPublicPlan
	}
	procedures := make([]ConnectProcedure, 0, len(listRoutes))
	for _, route := range listRoutes {
		procedures = append(procedures, ConnectProcedure{
			Path: route.path, Access: RouteAccessProtected,
			Flow: route.flow, Transport: RouteTransportTLSRequired,
		})
	}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return listsv1connect.NewListServiceHandler(handler, options...)
	}, procedures...); err != nil {
		return err
	}
	for _, route := range listRoutes {
		permission, err := authorization.NewPermission(authv1.ScopeFamily_SCOPE_FAMILY_LISTS, route.op)
		if err != nil {
			return err
		}
		if err := plan.BindPermission(RouteKindConnect, RouteMethodPost, route.path, permission); err != nil {
			return err
		}
	}
	return nil
}
