package server

// Explicit synthetic qualification only. This adapter is not compiled into the
// production server and never starts an integration or a device driver.

import (
	"connectrpc.com/connect"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	av "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"net/http"
)

func nhf015RegisterCalendarRoutes(plan *PublicPlan, handler *nhf015CalendarHandler) error {
	if plan == nil || handler == nil {
		return ErrInvalidPublicPlan
	}
	routes := calendarResourceRoutes[:]
	procedures := make([]ConnectProcedure, 0, len(routes))
	for _, r := range routes {
		procedures = append(procedures, ConnectProcedure{Path: r.path, Access: RouteAccessProtected, Flow: r.flow, Transport: RouteTransportTLSRequired})
	}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return apiv2connect.NewCalendarServiceHandler(handler.calendarResourceHandler, append(options, connect.WithInterceptors(handler))...)
	}, procedures...); err != nil {
		return err
	}
	for _, r := range routes {
		permission, err := authorization.NewPermission(av.ScopeFamily_SCOPE_FAMILY_CALENDARS, r.op)
		if err != nil {
			return err
		}
		if err := plan.BindPermission(RouteKindConnect, RouteMethodPost, r.path, permission); err != nil {
			return err
		}
	}
	return nil
}
