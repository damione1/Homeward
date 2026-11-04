package server

import (
	"connectrpc.com/connect"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"net/http"
)

func registerHistoryRoutes(plan *PublicPlan, runtime *historyRuntime) error {
	if plan == nil || runtime == nil || runtime.reader == nil || runtime.codec == nil || nilReactiveInterface(runtime.names) {
		return ErrInvalidPublicPlan
	}
	handler, err := newHistoryV2Handler(runtime, runtime.names)
	if err != nil {
		return err
	}
	routes := []struct {
		path   string
		family auth.ScopeFamily
	}{
		{apiv2connect.HistoryServiceQueryObservationsProcedure, auth.ScopeFamily_SCOPE_FAMILY_HISTORY},
		{apiv2connect.HistoryServiceQueryAggregatesProcedure, auth.ScopeFamily_SCOPE_FAMILY_HISTORY},
		{apiv2connect.HistoryServiceQueryExecutionTracesProcedure, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION},
	}
	procedures := make([]ConnectProcedure, 0, len(routes))
	for _, r := range routes {
		procedures = append(procedures, ConnectProcedure{Path: r.path, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired})
	}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		options = append(options, connect.WithCodec(historyV2JSONCodec{name: "json"}), connect.WithCodec(historyV2JSONCodec{name: "json; charset=utf-8"}))
		prefix, service := apiv2connect.NewHistoryServiceHandler(handler, options...)
		return prefix, historyReservedTransport(runtime, service)
	}, procedures...); err != nil {
		return err
	}
	for _, r := range routes {
		permission, err := authorization.NewPermission(r.family, auth.ScopeOperation_SCOPE_OPERATION_READ)
		if err != nil {
			return err
		}
		if err = plan.BindPermission(RouteKindConnect, RouteMethodPost, r.path, permission); err != nil {
			return err
		}
	}
	return nil
}
