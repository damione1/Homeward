package server

import (
	"connectrpc.com/connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/observability/v1/observabilityv1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"net/http"
)

type observationRoute struct {
	path   string
	family auth.ScopeFamily
}

var observationRoutes = [...]observationRoute{
	{observabilityv1connect.ObservabilityServiceGetHealthProcedure, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION},
	{observabilityv1connect.ObservabilityServiceGetMetricsProcedure, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION},
	{observabilityv1connect.ObservabilityServiceGetIntegrationHealthProcedure, auth.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS},
	{observabilityv1connect.ObservabilityServiceQueryRuntimeLogsProcedure, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION},
	{observabilityv1connect.ObservabilityServiceQueryAuditsProcedure, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION},
	{observabilityv1connect.ObservabilityServiceReadRunTraceProcedure, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION},
	{observabilityv1connect.ObservabilityServiceReadRecorderCoverageProcedure, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION},
}

func registerObservationRoutes(plan *PublicPlan, runtime *observationRuntime) error {
	if plan == nil || runtime == nil {
		return ErrInvalidPublicPlan
	}
	procedures := make([]ConnectProcedure, 0, len(observationRoutes))
	for _, route := range observationRoutes {
		procedures = append(procedures, ConnectProcedure{Path: route.path, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired})
	}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return observabilityv1connect.NewObservabilityServiceHandler(&observationHandler{runtime: runtime}, options...)
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
