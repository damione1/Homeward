package server

import (
	"net/http"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"github.com/damione1/Homeward/internal/storage"
)

// registerAuthRoutes runs before PublicPlan is compiled. A caller-supplied
// authenticator/authorizer pair remains authoritative for custom factories.
// The default plan gets the session provider and the existing account audit.
func registerAuthRoutes(plan *PublicPlan, store *storage.Store, coordinator *sessionCoordinator, provider factorProvider) (*PublicPlan, error) {
	handler, err := newAuthHandler(store, coordinator, provider)
	if err != nil {
		return nil, err
	}
	auth := &sessionAuth{store: store, coordinator: coordinator}
	if plan == nil {
		plan, err = NewPublicPlan(auth, auth, accountTransportAudit{store: store})
		if err != nil {
			return nil, err
		}
	} else {
		plan.mu.Lock()
		if !plan.valid || plan.frozen || plan.routes == nil {
			plan.mu.Unlock()
			return nil, ErrInvalidPublicPlan
		}
		if plan.authenticator == nil && plan.authorizer == nil {
			plan.authenticator, plan.authorizer = auth, auth
		}
		plan.mu.Unlock()
	}
	err = plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return authv1connect.NewAuthServiceHandler(handler, options...)
	},
		ConnectProcedure{Path: authv1connect.AuthServiceStartLoginProcedure, Access: RouteAccessAnonymous, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: authv1connect.AuthServiceCompleteLoginProcedure, Access: RouteAccessAnonymous, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: authv1connect.AuthServiceRefreshSessionProcedure, Access: RouteAccessAnonymous, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: authv1connect.AuthServiceListSessionsProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: authv1connect.AuthServiceRevokeSessionProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: authv1connect.AuthServiceBeginFactorEnrollmentProcedure, Access: RouteAccessAnonymous, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: authv1connect.AuthServiceCompleteFactorEnrollmentProcedure, Access: RouteAccessAnonymous, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: authv1connect.AuthServiceListFactorsProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: authv1connect.AuthServiceRemoveFactorProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: authv1connect.AuthServiceReplaceRecoveryCodesProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
	)
	return plan, err
}
