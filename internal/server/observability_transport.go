package server

import (
	"connectrpc.com/connect"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"net/http"
)

type observationRoute struct {
	path   string
	family auth.ScopeFamily
}

var observationRoutes = [...]observationRoute{
	{apiv2connect.ObservabilityServiceGetHealthProcedure, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION},
	{apiv2connect.ObservabilityServiceGetMetricsProcedure, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION},
	{apiv2connect.ObservabilityServiceGetIntegrationHealthProcedure, auth.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS},
	{apiv2connect.ObservabilityServiceQueryRuntimeLogsProcedure, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION},
	{apiv2connect.ObservabilityServiceQueryAuditsProcedure, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION},
	{apiv2connect.ObservabilityServiceQueryRunTraceProcedure, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION},
	{apiv2connect.ObservabilityServiceQueryRecorderCoverageProcedure, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION},
}

func registerObservationRoutes(plan *PublicPlan, runtime *observationRuntime, names administrationNames) error {
	if plan == nil || runtime == nil {
		return ErrInvalidPublicPlan
	}
	procedures := make([]ConnectProcedure, 0, len(observationRoutes))
	for _, route := range observationRoutes {
		procedures = append(procedures, ConnectProcedure{Path: route.path, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired})
	}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return apiv2connect.NewObservabilityServiceHandler(&observationV2Handler{reader: &observationReader{runtime: runtime}, names: names}, options...)
	}, procedures...); err != nil {
		return err
	}
	for _, route := range observationRoutes {
		permission, err := authorization.NewPermission(route.family, auth.ScopeOperation_SCOPE_OPERATION_READ)
		if err != nil {
			return err
		}
		if err := plan.BindPermission(RouteKindConnect, RouteMethodPost, route.path, permission); err != nil {
			return err
		}
	}
	return nil
}
