package server

import (
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configdocuments"
)

var _ apiv2connect.IntegrationServiceHandler = (*integrationHandler)(nil)

func registerIntegrationRoutes(plan *PublicPlan, documents *configdocuments.Service, sessions *sessionCoordinator, runtime *observationRuntime, names administrationNames, operations *configurationOperationProvider) error {
	if documents == nil {
		return nil
	}
	if plan == nil || operations == nil || operations.integrations != nil {
		return ErrInvalidPublicPlan
	}
	native, err := newIntegrationProjection(documents, sessions, runtime, names)
	if err != nil {
		return err
	}
	handler := &integrationHandler{native: native, authority: sessions.authority, operations: operations, now: time.Now}
	handler.pages, err = newAPIPageCipher()
	if err != nil {
		return err
	}
	routes := []struct {
		path      string
		operation auth.ScopeOperation
	}{
		{apiv2connect.IntegrationServiceGetIntegrationTypeProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ},
		{apiv2connect.IntegrationServiceListIntegrationTypesProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ},
		{apiv2connect.IntegrationServiceGetIntegrationInstanceProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ},
		{apiv2connect.IntegrationServiceListIntegrationInstancesProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ},
		{apiv2connect.IntegrationServiceCreateIntegrationInstanceProcedure, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER},
		{apiv2connect.IntegrationServiceUpdateIntegrationInstanceProcedure, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER},
		{apiv2connect.IntegrationServiceDeleteIntegrationInstanceProcedure, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER},
	}
	procedures := make([]ConnectProcedure, 0, len(routes))
	for _, route := range routes {
		procedures = append(procedures, ConnectProcedure{Path: route.path, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired})
	}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return apiv2connect.NewIntegrationServiceHandler(handler, options...)
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
	operations.integrations = handler
	return nil
}
