package server

import (
	"net/http"

	"connectrpc.com/connect"
	configurationapplyv1connect "github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/configplans"
)

func registerConfigApplyRoutes(plan *PublicPlan, service *configplans.Service, documents *configdocuments.Service, coordinator *sessionCoordinator, dependencies ...configurationRPCDependencies) error {
	if service == nil {
		return nil
	}
	if plan == nil || documents == nil || coordinator == nil || coordinator.authority == nil {
		return ErrInvalidPublicPlan
	}
	paths := []string{configurationapplyv1connect.ConfigurationApplyServiceApplyConfigurationPlanProcedure, configurationapplyv1connect.ConfigurationApplyServiceRollbackConfigurationProcedure}
	procedures := make([]ConnectProcedure, 0, len(paths))
	for _, path := range paths {
		procedures = append(procedures, ConnectProcedure{Path: path, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired})
	}
	handler := &configApplyHandler{service: service, catalog: firstConfigurationRPC(dependencies).plans, operations: firstConfigurationRPC(dependencies).operations, documents: &configDocumentsHandler{service: documents, authority: coordinator.authority, names: firstConfigurationRPC(dependencies).names}}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return configurationapplyv1connect.NewConfigurationApplyServiceHandler(handler, options...)
	}, procedures...); err != nil {
		return err
	}
	permission, err := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := plan.BindPermission(RouteKindConnect, RouteMethodPost, path, permission); err != nil {
			return err
		}
	}
	return nil
}
