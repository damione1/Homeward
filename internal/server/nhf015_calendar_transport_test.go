package server

// Explicit synthetic qualification only. This adapter is not compiled into the
// production server and never starts an integration or a device driver.

import (
	"connectrpc.com/connect"
	av "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/calendars/v1/calendarsv1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"net/http"
)

func nhf015RegisterCalendarRoutes(plan *PublicPlan, handler *nhf015CalendarHandler) error {
	if plan == nil || handler == nil {
		return ErrInvalidPublicPlan
	}
	routes := []struct {
		path      string
		operation av.ScopeOperation
		flow      RouteFlow
	}{
		{calendarsv1connect.CalendarServiceResolveLocalTimeProcedure, av.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
		{calendarsv1connect.CalendarServiceListCalendarsProcedure, av.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
		{calendarsv1connect.CalendarServiceGetCalendarProcedure, av.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
		{calendarsv1connect.CalendarServiceMutateCalendarProcedure, av.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
		{calendarsv1connect.CalendarServiceGetEventProcedure, av.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
		{calendarsv1connect.CalendarServiceMutateEventProcedure, av.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
		{calendarsv1connect.CalendarServiceQueryOccurrencesProcedure, av.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
		{calendarsv1connect.CalendarServiceWatchCalendarProcedure, av.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowServerStream},
	}
	procedures := make([]ConnectProcedure, 0, len(routes))
	for _, r := range routes {
		procedures = append(procedures, ConnectProcedure{Path: r.path, Access: RouteAccessProtected, Flow: r.flow, Transport: RouteTransportTLSRequired})
	}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return calendarsv1connect.NewCalendarServiceHandler(handler, options...)
	}, procedures...); err != nil {
		return err
	}
	for _, r := range routes {
		permission, err := authorization.NewPermission(av.ScopeFamily_SCOPE_FAMILY_CALENDARS, r.operation)
		if err != nil {
			return err
		}
		if err := plan.BindPermission(RouteKindConnect, RouteMethodPost, r.path, permission); err != nil {
			return err
		}
	}
	return nil
}
