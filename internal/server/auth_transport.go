package server

import (
	"net/http"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/storage"
)

// registerAuthRoutes runs before PublicPlan is compiled. A caller-supplied
// authenticator/authorizer pair remains authoritative for custom factories.
// The default plan gets the session provider and the existing account audit.
func registerAuthRoutes(plan *PublicPlan, store *storage.Store, coordinator *sessionCoordinator, provider factorProvider, names administrationNames) (*PublicPlan, error) {
	if nilReactiveInterface(names.home) || names.store != store {
		return nil, ErrInvalidPublicPlan
	}
	nativeHandler, err := newAuthHandler(store, coordinator, provider)
	if err != nil {
		return nil, err
	}
	pages, err := newAPIPageCipher()
	if err != nil {
		return nil, err
	}
	handler := &authResourceHandler{authHandler: nativeHandler, names: authResourceNames{names}, pages: pages}
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
		return apiv2connect.NewAuthServiceHandler(handler, options...)
	},
		ConnectProcedure{Path: apiv2connect.AuthServiceStartLoginProcedure, Access: RouteAccessAnonymous, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: apiv2connect.AuthServiceCompleteLoginProcedure, Access: RouteAccessAnonymous, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: apiv2connect.AuthServiceRefreshSessionProcedure, Access: RouteAccessAnonymous, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: apiv2connect.AuthServiceGetSessionProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: apiv2connect.AuthServiceListSessionsProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: apiv2connect.AuthServiceRevokeSessionProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: apiv2connect.AuthServiceBeginFactorEnrollmentProcedure, Access: RouteAccessAnonymous, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: apiv2connect.AuthServiceCompleteFactorEnrollmentProcedure, Access: RouteAccessAnonymous, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: apiv2connect.AuthServiceGetFactorCredentialProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: apiv2connect.AuthServiceListFactorCredentialsProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: apiv2connect.AuthServiceRemoveFactorCredentialProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: apiv2connect.AuthServiceReplaceRecoveryCodesProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
	)
	return plan, err
}
