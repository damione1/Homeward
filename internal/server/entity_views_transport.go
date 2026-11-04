package server

import (
	"connectrpc.com/connect"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"net/http"
)

func registerEntityViewRoutes(plan *PublicPlan, handler *homeResourceHandler) error {
	if plan == nil || handler == nil || handler.views == nil {
		return ErrInvalidPublicPlan
	}
	paths := []string{
		apiv2connect.EntityViewServiceCreateEntityViewProcedure,
		apiv2connect.EntityViewServiceGetEntityViewProcedure,
		apiv2connect.EntityViewServiceListEntityViewsProcedure,
		apiv2connect.EntityViewServiceDeleteEntityViewProcedure,
		apiv2connect.EntityViewServiceQueryEntityViewProcedure,
		apiv2connect.EntityViewServiceWatchEntityViewProcedure,
	}
	procedures := make([]ConnectProcedure, 0, len(paths))
	for _, path := range paths {
		flow := RouteFlowUnary
		if path == apiv2connect.EntityViewServiceWatchEntityViewProcedure {
			flow = RouteFlowServerStream
		}
		procedures = append(procedures, ConnectProcedure{Path: path, Access: RouteAccessProtected, Flow: flow, Transport: RouteTransportTLSRequired})
	}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return apiv2connect.NewEntityViewServiceHandler(handler, options...)
	}, procedures...); err != nil {
		return err
	}
	for _, path := range paths {
		if err := plan.BindPermission(RouteKindConnect, RouteMethodPost, path, homeReadPermission()); err != nil {
			return err
		}
	}
	return nil
}
