package server

import (
	"net/http"

	"connectrpc.com/connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/configurationplan/v1/configurationplanv1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/configplans"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
)

func prepareConfigPlans(documents *configdocuments.Service, owner *configactivation.Owner, secrets *storage.SecretRepository, manifest registry.Manifest) (*configplans.Service, error) {
	if documents == nil {
		return nil, nil
	}
	return configplans.NewService(configplans.Input{Documents: documents, Owner: owner, Secrets: secrets, Manifest: manifest})
}
func registerConfigPlansRoutes(plan *PublicPlan, service *configplans.Service, documents *configdocuments.Service, coordinator *sessionCoordinator) error {
	if service == nil {
		return nil
	}
	if plan == nil || documents == nil || coordinator == nil || coordinator.authority == nil {
		return ErrInvalidPublicPlan
	}
	paths := []string{configurationplanv1connect.ConfigurationPlanServiceBuildPlanProcedure, configurationplanv1connect.ConfigurationPlanServiceRevalidatePlanProcedure}
	procedures := make([]ConnectProcedure, 0, len(paths))
	for _, path := range paths {
		procedures = append(procedures, ConnectProcedure{Path: path, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired})
	}
	handler := &configPlansHandler{service: service, documents: &configDocumentsHandler{service: documents, authority: coordinator.authority}}
	if e := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return configurationplanv1connect.NewConfigurationPlanServiceHandler(handler, options...)
	}, procedures...); e != nil {
		return e
	}
	permission, e := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
	if e != nil {
		return e
	}
	for _, path := range paths {
		if e := plan.BindPermission(RouteKindConnect, RouteMethodPost, path, permission); e != nil {
			return e
		}
	}
	return nil
}
