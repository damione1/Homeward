package server

import (
	"net/http"

	"connectrpc.com/connect"
	configurationv1connect "github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
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

func registerConfigDocumentsRoutes(plan *PublicPlan, service *configdocuments.Service, coordinator *sessionCoordinator, dependencies ...configurationRPCDependencies) error {
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
		{configurationv1connect.ConfigurationServiceGetConfigurationRevisionProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
		{configurationv1connect.ConfigurationServiceGetConfigurationGenerationProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
		{configurationv1connect.ConfigurationServiceGetConfigurationSchemaProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
		{configurationv1connect.ConfigurationServiceGetConfigurationDocumentProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
		{configurationv1connect.ConfigurationServiceListConfigurationDocumentsProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
		{configurationv1connect.ConfigurationServiceListConfigurationRevisionsProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
		{configurationv1connect.ConfigurationServiceListConfigurationGenerationsProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
		{configurationv1connect.ConfigurationServiceListConfigurationSchemasProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
		{configurationv1connect.ConfigurationServicePublishConfigurationDocumentProcedure, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, RouteFlowUnary},
		{configurationv1connect.ConfigurationServiceRollbackConfigurationDocumentProcedure, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, RouteFlowUnary},
		{configurationv1connect.ConfigurationServiceWatchConfigurationProcedure, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowServerStream},
	}
	procedures := make([]ConnectProcedure, 0, len(routes))
	for _, route := range routes {
		procedures = append(procedures, ConnectProcedure{Path: route.path, Access: RouteAccessProtected, Flow: route.flow, Transport: RouteTransportTLSRequired})
	}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return configurationv1connect.NewConfigurationServiceHandler(&configDocumentsHandler{service: service, authority: coordinator.authority, names: firstConfigurationRPC(dependencies).names, operations: firstConfigurationRPC(dependencies).operations}, options...)
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
