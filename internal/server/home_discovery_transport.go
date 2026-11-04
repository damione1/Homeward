package server

import (
	"net/http"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/authorization"
)

// Discovery is available before an operational configuration or view exists.
// Only these two explicitly admitted procedures are exposed by this factory;
// generated methods do not install additional public routes.
func registerHomeDiscoveryRoutes(plan *PublicPlan, owner homeResourceOwner, authority *authorization.Authority) error {
	if plan == nil || nilReactiveInterface(owner) || authority == nil {
		return ErrInvalidPublicPlan
	}
	handler := &homeResourceHandler{owner: owner, authority: authority}
	return plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return apiv2connect.NewHomeServiceHandler(handler, options...)
	},
		ConnectProcedure{Path: apiv2connect.HomeServiceGetHomeProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: apiv2connect.HomeServiceListHomesProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
	)
}
