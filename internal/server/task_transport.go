package server

import (
	"connectrpc.com/connect"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"net/http"
)

var taskRoutes = [...]businessRoute{
	{apiv2connect.TaskServiceListTaskListsProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
	{apiv2connect.TaskServiceGetTaskListProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
	{apiv2connect.TaskServiceCreateTaskListProcedure, auth.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
	{apiv2connect.TaskServiceUpdateTaskListProcedure, auth.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
	{apiv2connect.TaskServiceDeleteTaskListProcedure, auth.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
	{apiv2connect.TaskServiceListTasksProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
	{apiv2connect.TaskServiceGetTaskProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
	{apiv2connect.TaskServiceAppendTaskProcedure, auth.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
	{apiv2connect.TaskServiceUpdateTaskProcedure, auth.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
	{apiv2connect.TaskServiceDeleteTaskProcedure, auth.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
	{apiv2connect.TaskServiceMoveTaskProcedure, auth.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
	{apiv2connect.TaskServiceWatchTaskListsProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowServerStream},
}

func registerTaskRoutes(plan *PublicPlan, handler *taskHandler) error {
	if plan == nil || handler == nil {
		return ErrInvalidPublicPlan
	}
	procedures := make([]ConnectProcedure, 0, len(taskRoutes))
	for _, route := range taskRoutes {
		procedures = append(procedures, ConnectProcedure{Path: route.path, Access: RouteAccessProtected, Flow: route.flow, Transport: RouteTransportTLSRequired})
	}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return apiv2connect.NewTaskServiceHandler(handler, options...)
	}, procedures...); err != nil {
		return err
	}
	for _, route := range taskRoutes {
		permission, err := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_LISTS, route.op)
		if err != nil {
			return err
		}
		if err = plan.BindPermission(RouteKindConnect, RouteMethodPost, route.path, permission); err != nil {
			return err
		}
	}
	return nil
}
