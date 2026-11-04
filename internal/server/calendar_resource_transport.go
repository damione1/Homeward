package server

import (
	"connectrpc.com/connect"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"net/http"
)

var _ apiv2connect.CalendarServiceHandler = (*calendarResourceHandler)(nil)
var calendarResourceRoutes = [...]businessRoute{
	{apiv2connect.CalendarServiceQueryCalendarSourcesProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
	{apiv2connect.CalendarServiceListCalendarsProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
	{apiv2connect.CalendarServiceGetCalendarProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
	{apiv2connect.CalendarServiceCreateCalendarProcedure, auth.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
	{apiv2connect.CalendarServiceUpdateCalendarProcedure, auth.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
	{apiv2connect.CalendarServiceDeleteCalendarProcedure, auth.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
	{apiv2connect.CalendarServiceListCalendarEventsProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
	{apiv2connect.CalendarServiceGetCalendarEventProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
	{apiv2connect.CalendarServiceCreateCalendarEventProcedure, auth.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
	{apiv2connect.CalendarServiceUpdateCalendarEventProcedure, auth.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
	{apiv2connect.CalendarServiceDeleteCalendarEventProcedure, auth.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
	{apiv2connect.CalendarServiceQueryOccurrencesProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
	{apiv2connect.CalendarServiceResolveCalendarTimeProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
	{apiv2connect.CalendarServiceWatchCalendarProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowServerStream},
}

func registerCalendarResourceRoutes(plan *PublicPlan, handler *calendarResourceHandler) error {
	if plan == nil || handler == nil {
		return ErrInvalidPublicPlan
	}
	procedures := make([]ConnectProcedure, 0, len(calendarResourceRoutes))
	for _, route := range calendarResourceRoutes {
		procedures = append(procedures, ConnectProcedure{Path: route.path, Access: RouteAccessProtected, Flow: route.flow, Transport: RouteTransportTLSRequired})
	}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return apiv2connect.NewCalendarServiceHandler(handler, options...)
	}, procedures...); err != nil {
		return err
	}
	for _, route := range calendarResourceRoutes {
		permission, err := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_CALENDARS, route.op)
		if err != nil {
			return err
		}
		if err = plan.BindPermission(RouteKindConnect, RouteMethodPost, route.path, permission); err != nil {
			return err
		}
	}
	return nil
}
