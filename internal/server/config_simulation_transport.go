package server

import (
	"errors"
	"net/http"

	"connectrpc.com/connect"
	configurationsimulationv1connect "github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configdocuments"
)

var errorsSimulationUnavailable = errors.New("configuration simulation unavailable")

func registerConfigSimulationRoutes(plan *PublicPlan, documents *configdocuments.Service, coordinator *sessionCoordinator, dependencies ...configurationRPCDependencies) error {
	if documents == nil {
		return nil
	}
	if plan == nil || coordinator == nil || coordinator.authority == nil {
		return ErrInvalidPublicPlan
	}
	path := configurationsimulationv1connect.ConfigurationSimulationServiceSimulateConfigurationPlanProcedure
	handler := &configSimulationHandler{documents: &configDocumentsHandler{service: documents, authority: coordinator.authority, names: firstConfigurationRPC(dependencies).names}}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return configurationsimulationv1connect.NewConfigurationSimulationServiceHandler(handler, options...)
	}, ConnectProcedure{Path: path, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired}); err != nil {
		return err
	}
	permission, err := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
	if err != nil {
		return err
	}
	return plan.BindPermission(RouteKindConnect, RouteMethodPost, path, permission)
}
