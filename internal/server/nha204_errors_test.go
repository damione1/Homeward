package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	native "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

func nha204CanonicalReason(t *testing.T, err error, expected api.ApiErrorReason) {
	t.Helper()
	var rpc *connect.Error
	if !errors.As(err, &rpc) || rpc == nil {
		t.Fatal("missing RPC error", err)
	}
	if len(rpc.Details()) != 1 {
		t.Fatal("expected one canonical ErrorInfo, without a historical termination detail")
	}
	value, decodeErr := rpc.Details()[0].Value()
	info, ok := value.(*errdetails.ErrorInfo)
	if decodeErr != nil || !ok || info.Domain != apierrors.Domain() || info.Reason != strings.TrimPrefix(expected.String(), "API_ERROR_REASON_") {
		t.Fatal("invalid canonical transport error", decodeErr)
	}
	if rpc.Code() != connect.Code(apierrors.New(expected).Code()) {
		t.Fatal("reason and RPC code disagree")
	}
}

func TestNHA204PublicGuardCanonicalErrorsOnConnectAndGRPC(t *testing.T) {
	rig := nha142NewServerRig(t)
	plan, _, err := composeSessionsWithCoordinator(nil, rig.store, rig.coordinator, nha203AuthFixtureNames(rig.store))
	if err != nil {
		t.Fatal(err)
	}
	settings := config.DefaultPublicSettings()
	compiled, err := compilePublicPlan(plan, settings)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(compiled.handler(settings, newPublicAdmissions()))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	for _, grpc := range []bool{false, true} {
		t.Run(map[bool]string{false: "Connect", true: "gRPC"}[grpc], func(t *testing.T) {
			var options []connect.ClientOption
			if grpc {
				options = append(options, connect.WithGRPC())
			}
			client := apiv2connect.NewAccountServiceClient(server.Client(), server.URL, options...)
			_, err := client.GetAccount(rig.ctx, connect.NewRequest(&api.GetAccountRequest{Name: rig.accountName.String()}))
			nha204CanonicalReason(t, err, api.ApiErrorReason_API_ERROR_REASON_AUTHENTICATION_REQUIRED)
		})
	}
}

func TestNHA204AuthorizationTerminationRemainsCanonical(t *testing.T) {
	cases := []struct {
		cause  native.AuthorizationTerminationReason
		reason api.ApiErrorReason
	}{
		{native.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_CREDENTIAL_REVOKED, api.ApiErrorReason_API_ERROR_REASON_AUTHENTICATION_REQUIRED},
		{native.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_CREDENTIAL_EXPIRED, api.ApiErrorReason_API_ERROR_REASON_AUTHENTICATION_REQUIRED},
		{native.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_ACCOUNT_DISABLED, api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED},
		{native.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_RIGHTS_CHANGED, api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED},
		{native.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_AUTHORITY_UNAVAILABLE, api.ApiErrorReason_API_ERROR_REASON_AUTHORITY_UNAVAILABLE},
	}
	for _, test := range cases {
		t.Run(test.cause.String(), func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(authorizationTerminationCause{reason: test.cause})
			nha204CanonicalReason(t, authorizationTerminationError(ctx), test.reason)
			wrapped := (authorizationTerminationInterceptor{}).WrapUnary(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
				return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE).Connect()
			})
			_, err := wrapped(ctx, connect.NewRequest(&api.GetHomeRequest{}))
			nha204CanonicalReason(t, err, test.reason)
		})
	}
	if err := authorizationTerminationError(context.Background()); err != nil {
		t.Fatal("ordinary context manufactured a revocation", err)
	}
}

func TestNHA204CompleteLoginDeliveryStopsOnNativeRevocation(t *testing.T) {
	rig := nha142NewServerRig(t)
	initial := rig.start(t).GetAuthenticated()
	if initial == nil {
		t.Fatal("missing initial password session")
	}
	key := nha142NewAuthenticator(t)
	rig.enroll(t, key, initial.AccessToken, "")
	challenge := rig.start(t).GetChallenge()
	if challenge == nil {
		t.Fatal("missing real factor challenge")
	}
	plan, _, err := composeSessionsWithCoordinator(nil, rig.store, rig.coordinator, nha203AuthFixtureNames(rig.store))
	if err != nil {
		t.Fatal(err)
	}
	settings := config.DefaultPublicSettings()
	compiled, err := compilePublicPlan(plan, settings)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(compiled.handler(settings, newPublicAdmissions()))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	blocked := authUnreadH2(t, rig.ctx, server, apiv2connect.AuthServiceCompleteLoginProcedure, &api.CompleteLoginRequest{
		ChallengeId: challenge.ChallengeId, Proof: key.assertion(t, challenge, 0x05),
	}, "")
	sessions, err := rig.store.ListSessions(rig.ctx, rig.account, rig.account, rig.clock.Now())
	if err != nil || len(sessions) != 1 {
		t.Fatal("factor finalization did not commit exactly one active session", err)
	}
	id, err := sessions[0].ID()
	if err != nil {
		t.Fatal(err)
	}
	guard, err := rig.coordinator.enter(rig.ctx, rig.account)
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.store.RevokeSession(rig.ctx, rig.account, storage.SessionOwner{Account: rig.account, Session: id}, rig.clock.Now()); err != nil {
		guard.release()
		t.Fatal(err)
	}
	guard.cancelLeases(rig.ctx, &id)
	guard.release()
	authWaitH2Closed(t, blocked)
}
