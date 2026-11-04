package server

import (
	"connectrpc.com/connect"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/storage"
	"net/http"
)

func registerMCPKeyRoutes(plan *PublicPlan, store *storage.Store, coordinator *sessionCoordinator, names administrationNames) error {
	if plan == nil || store == nil || coordinator == nil || nilReactiveInterface(names.home) || names.store != store {
		return ErrInvalidPublicPlan
	}
	pages, err := newAPIPageCipher()
	if err != nil {
		return err
	}
	handler := &mcpKeyResourceHandler{store: store, coordinator: coordinator, names: authResourceNames{names}, pages: pages}
	return plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return apiv2connect.NewMcpKeyServiceHandler(handler, options...)
	},
		ConnectProcedure{Path: apiv2connect.McpKeyServiceIssueMcpKeyProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: apiv2connect.McpKeyServiceListMcpKeysProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: apiv2connect.McpKeyServiceRevokeMcpKeyProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired})
}
