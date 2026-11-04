package server

import (
	"net/http"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/authorization"
)

func registerHomeResourceRoutes(plan *PublicPlan, handler *homeResourceHandler) error {
	if plan == nil || handler == nil || handler.views == nil {
		return ErrInvalidPublicPlan
	}
	routes := []struct {
		path       string
		permission authorization.Permission
	}{
		{apiv2connect.HomeServiceGetEntityProcedure, homeReadPermission()},
		{apiv2connect.HomeServiceListEntitiesProcedure, homeReadPermission()},
		{apiv2connect.HomeServiceUpdateEntityProcedure, homeAdminPermission()},
		{apiv2connect.HomeServiceGetCapabilityProcedure, homeReadPermission()},
		{apiv2connect.HomeServiceListCapabilitiesProcedure, homeReadPermission()},
	}
	procedures := make([]ConnectProcedure, 0, len(routes))
	for _, route := range routes {
		procedures = append(procedures, ConnectProcedure{Path: route.path, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired})
	}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return apiv2connect.NewHomeServiceHandler(handler, options...)
	}, procedures...); err != nil {
		return err
	}
	for _, route := range routes {
		if err := plan.BindPermission(RouteKindConnect, RouteMethodPost, route.path, route.permission); err != nil {
			return err
		}
	}
	return nil
}
