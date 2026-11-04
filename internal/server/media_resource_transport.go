package server

import (
	"connectrpc.com/connect"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"net/http"
)

var _ apiv2connect.MediaServiceHandler = (*mediaResourceHandler)(nil)

func registerMediaResourceRoutes(plan *PublicPlan, handler *mediaResourceHandler) error {
	if plan == nil || handler == nil {
		return ErrInvalidPublicPlan
	}
	routes := []struct {
		path string
		flow RouteFlow
	}{
		{apiv2connect.MediaServiceListMediaSourcesProcedure, RouteFlowUnary}, {apiv2connect.MediaServiceGetMediaSourceProcedure, RouteFlowUnary},
		{apiv2connect.MediaServiceCreateMediaSessionProcedure, RouteFlowUnary}, {apiv2connect.MediaServiceGetMediaSessionProcedure, RouteFlowUnary}, {apiv2connect.MediaServiceListMediaSessionsProcedure, RouteFlowUnary}, {apiv2connect.MediaServiceDeleteMediaSessionProcedure, RouteFlowUnary},
		{apiv2connect.MediaServiceReadMediaSessionProcedure, RouteFlowUnary}, {apiv2connect.MediaServiceQueryMediaSessionEventsProcedure, RouteFlowUnary},
		{apiv2connect.MediaServiceWatchMediaSessionProcedure, RouteFlowServerStream}, {apiv2connect.MediaServiceStreamMediaSessionProcedure, RouteFlowServerStream},
	}
	procedures := make([]ConnectProcedure, 0, len(routes))
	for _, route := range routes {
		procedures = append(procedures, ConnectProcedure{Path: route.path, Access: RouteAccessProtected, Flow: route.flow, Transport: RouteTransportTLSRequired})
	}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return apiv2connect.NewMediaServiceHandler(handler, append(options, connect.WithReadMaxBytes(8192))...)
	}, procedures...); err != nil {
		return err
	}
	permission, err := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_MEDIA, auth.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return err
	}
	for _, route := range routes {
		if err := plan.BindPermission(RouteKindConnect, RouteMethodPost, route.path, permission); err != nil {
			return err
		}
	}
	return nil
}
