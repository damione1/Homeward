package server

import (
	"connectrpc.com/connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/history/v1/historyv1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"net/http"
)

func registerHistoryRoutes(plan *PublicPlan, runtime *historyRuntime) error {
	if plan == nil || runtime == nil || runtime.reader == nil || runtime.codec == nil {
		return ErrInvalidPublicPlan
	}
	routes := []struct {
		path   string
		family auth.ScopeFamily
	}{
		{historyv1connect.HistoryServiceQueryObservationsProcedure, auth.ScopeFamily_SCOPE_FAMILY_HISTORY},
		{historyv1connect.HistoryServiceQueryAggregatesProcedure, auth.ScopeFamily_SCOPE_FAMILY_HISTORY},
		{historyv1connect.HistoryServiceQueryExecutionTracesProcedure, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION},
	}
	procedures := make([]ConnectProcedure, 0, len(routes))
	for _, r := range routes {
		procedures = append(procedures, ConnectProcedure{Path: r.path, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired})
	}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		options = append(options, connect.WithCodec(historyJSONCodec{name: "json"}), connect.WithCodec(historyJSONCodec{name: "json; charset=utf-8"}))
		prefix, handler := historyv1connect.NewHistoryServiceHandler(&historyHandler{runtime: runtime}, options...)
		return prefix, historyReservedTransport(runtime, handler)
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
