package server

import (
	"net/http"

	"connectrpc.com/connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/configuration/v1/configurationv1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/storage"
)

func prepareConfigDocuments(startup config.StartupInputs, store *storage.Store, owner *configactivation.Owner, secrets *storage.SecretRepository) (*configdocuments.Service, error) {
	if owner == nil {
		return nil, nil
	}
	// Deliberately detached Go fixtures cannot expose a writable filesystem API.
	if _, _, physical := startup.WorkspaceDocumentLocation(); !physical {
		return nil, nil
	}
	return configdocuments.NewService(configdocuments.Input{Store: store, Owner: owner, Startup: startup, Secrets: secrets})
}

func registerConfigDocumentsRoutes(plan *PublicPlan, service *configdocuments.Service, coordinator *sessionCoordinator) error {
	if service == nil {
		return nil
	}
	if plan == nil || coordinator == nil || coordinator.authority == nil {
		return ErrInvalidPublicPlan
	}
	type route struct {
		path      string
		operation auth.ScopeOperation
		flow      RouteFlow
	}
	routes := []route{
		{configurationv1connect.ConfigurationServiceReadDocumentProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
		{configurationv1connect.ConfigurationServiceListDocumentsProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
		{configurationv1connect.ConfigurationServiceListVersionsProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
		{configurationv1connect.ConfigurationServiceListGenerationsProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
		{configurationv1connect.ConfigurationServiceListSchemasProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
		{configurationv1connect.ConfigurationServiceWriteDocumentProcedure, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, RouteFlowUnary},
		{configurationv1connect.ConfigurationServiceRestoreDocumentProcedure, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, RouteFlowUnary},
		{configurationv1connect.ConfigurationServiceWatchConfigurationProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowServerStream},
	}
	procedures := make([]ConnectProcedure, 0, len(routes))
	for _, route := range routes {
		procedures = append(procedures, ConnectProcedure{Path: route.path, Access: RouteAccessProtected, Flow: route.flow, Transport: RouteTransportTLSRequired})
	}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return configurationv1connect.NewConfigurationServiceHandler(&configDocumentsHandler{service: service, authority: coordinator.authority}, options...)
	}, procedures...); err != nil {
		return err
	}
	for _, route := range routes {
		permission, err := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, route.operation)
		if err != nil {
			return err
		}
		if err := plan.BindPermission(RouteKindConnect, RouteMethodPost, route.path, permission); err != nil {
			return err
		}
	}
	return nil
}
