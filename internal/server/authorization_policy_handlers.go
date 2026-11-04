package server

import (
	"connectrpc.com/connect"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/storage"
	"net/http"
)

func registerAuthorizationPolicyRoutes(plan *PublicPlan, store *storage.Store, coordinator *sessionCoordinator, names administrationNames) error {
	if plan == nil || store == nil || coordinator == nil || nilReactiveInterface(names.home) || names.store != store {
		return ErrInvalidPublicPlan
	}
	handler := &accountResourceHandler{accountHandler: &accountHandler{store: store, coordinator: coordinator}, names: authResourceNames{names}}
	return plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return apiv2connect.NewAuthorizationPolicyServiceHandler(handler, options...)
	},
		ConnectProcedure{Path: apiv2connect.AuthorizationPolicyServiceGetAuthorizationPolicyProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: apiv2connect.AuthorizationPolicyServiceReplaceAuthorizationPolicyProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired})
}
