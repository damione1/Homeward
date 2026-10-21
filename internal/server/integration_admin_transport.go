package server

import (
	"connectrpc.com/connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/integrationadmin/v1/integrationadminv1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configdocuments"
	"net/http"
)

func registerIntegrationAdminRoutes(plan *PublicPlan, documents *configdocuments.Service, sessions *sessionCoordinator, runtime *observationRuntime) error {
	if documents == nil {
		return nil
	}
	handler, err := newIntegrationAdminHandler(documents, sessions, runtime)
	if err != nil || plan == nil {
		return ErrInvalidPublicPlan
	}
	routes := []struct {
		path      string
		operation auth.ScopeOperation
	}{
		{integrationadminv1connect.IntegrationServiceListIntegrationTypesProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ},
		{integrationadminv1connect.IntegrationServiceGetIntegrationTypeProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ},
		{integrationadminv1connect.IntegrationServiceListInstancesProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ},
		{integrationadminv1connect.IntegrationServiceGetInstanceProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ},
		{integrationadminv1connect.IntegrationServiceCreateInstanceProcedure, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER},
		{integrationadminv1connect.IntegrationServiceUpdateInstanceProcedure, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER},
		{integrationadminv1connect.IntegrationServiceDeleteInstanceProcedure, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER},
		{integrationadminv1connect.IntegrationServiceSetInstanceEnabledProcedure, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER},
	}
	procedures := make([]ConnectProcedure, 0, len(routes))
	for _, route := range routes {
		procedures = append(procedures, ConnectProcedure{Path: route.path, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired})
	}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return integrationadminv1connect.NewIntegrationServiceHandler(handler, options...)
	}, procedures...); err != nil {
		return err
	}
	for _, route := range routes {
		permission, err := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS, route.operation)
		if err != nil {
			return err
		}
		if err := plan.BindPermission(RouteKindConnect, RouteMethodPost, route.path, permission); err != nil {
			return err
		}
	}
	return nil
}
