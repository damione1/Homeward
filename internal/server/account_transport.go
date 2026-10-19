package server

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"github.com/damione1/Homeward/internal/storage"
)

// accountTransportAudit records only route and admitted identity metadata.
// The public transport still refuses every protected route until NHA-141
// supplies an authenticator and authorizer through the composition plan.
type accountTransportAudit struct{ store *storage.Store }

func (a accountTransportAudit) Record(ctx context.Context, event AuditEvent) error {
	var actor *contracts.AccountID
	if event.HasActor {
		value, err := event.Actor.Value()
		if err != nil {
			return errors.New("invalid admitted account actor")
		}
		id, err := contracts.ParseAccountID(value)
		if err != nil {
			return errors.New("invalid admitted account actor")
		}
		actor = &id
	}
	return a.store.RecordAuthTransportAudit(ctx, actor, event.Route.Path(), event.Outcome == AuditOutcomeAdmitted)
}

func registerAccountRoutes(plan *PublicPlan, store *storage.Store, coordinator *sessionCoordinator) (*PublicPlan, error) {
	if store == nil || coordinator == nil {
		return nil, ErrInvalidPublicPlan
	}
	var err error
	if plan == nil {
		plan, err = NewPublicPlan(nil, nil, accountTransportAudit{store: store})
		if err != nil {
			return nil, err
		}
	}
	handler := &accountHandler{store: store, coordinator: coordinator}
	err = plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return authv1connect.NewAccountServiceHandler(handler, options...)
	},
		ConnectProcedure{Path: authv1connect.AccountServiceCreateAccountProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: authv1connect.AccountServiceGetAccountProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: authv1connect.AccountServiceListAccountsProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: authv1connect.AccountServiceUpdateAccountProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: authv1connect.AccountServiceDeleteAccountProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: authv1connect.AccountServiceCreateInvitationProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: authv1connect.AccountServiceAcceptInvitationProcedure, Access: RouteAccessAnonymous, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: authv1connect.AccountServiceListInvitationsProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: authv1connect.AccountServiceRevokeInvitationProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: authv1connect.AccountServiceGetSecondFactorPolicyProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: authv1connect.AccountServiceSetSecondFactorPolicyProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
	)
	return plan, err
}
