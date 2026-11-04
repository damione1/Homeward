package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/config"
	"golang.org/x/net/http2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// This helper sends an ordinary unary Connect protobuf request over verified
// TLS/H2. The real advertised one-byte receive window blocks the response; the
// body is never drained to manufacture cancellation progress.
func authUnreadH2(t *testing.T, ctx context.Context, server *httptest.Server, procedure string, message proto.Message, bearer string) *http2.ClientConn {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport := &http.Transport{HTTP2: &http.HTTP2Config{MaxReceiveBufferPerStream: 1}}
	h2, err := http2.ConfigureTransports(transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(transport.CloseIdleConnections)
	connection, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", strings.TrimPrefix(server.URL, "https://"), &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if connection.ConnectionState().NegotiatedProtocol != "h2" {
		t.Fatal("HTTP/2 was not negotiated")
	}
	wire := &nha147H2SettingsConn{Conn: connection}
	client, err := h2.NewClientConn(wire)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if actual, present := wire.initialWindow(); !present || actual != 1 {
		t.Fatal("one-byte window was not advertised", actual, present)
	}
	body, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+procedure, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/proto")
	request.Header.Set("Connect-Protocol-Version", "1")
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := client.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if response.ProtoMajor != 2 || response.StatusCode != http.StatusOK {
		t.Fatal("AUTH response did not enter H2 send", response.Proto, response.StatusCode)
	}
	if client.State().StreamsActive != 1 {
		t.Fatal("response completed despite unread one-byte window")
	}
	return client
}

func authWaitH2Closed(t *testing.T, client *http2.ClientConn) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for client.State().StreamsActive != 0 {
		if time.Now().After(deadline) {
			t.Fatal("native revocation retained blocked AUTH response")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNHA203AuthH2RevocationDoesNotStallIndependentReader(t *testing.T) {
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
	accounts := apiv2connect.NewAccountServiceClient(server.Client(), server.URL)
	auth := apiv2connect.NewAuthServiceClient(server.Client(), server.URL)
	login := func(t *testing.T, name, password string) *api.SessionTokens {
		t.Helper()
		// Account writes use the native wall clock; logical session issuance
		// must not predate accounts created earlier in this same fixture.
		rig.clock.Advance(time.Now().UTC().Truncate(time.Microsecond))
		response, err := auth.StartLogin(rig.ctx, connect.NewRequest(&api.StartLoginRequest{LoginName: name, Password: password}))
		if err != nil || response.Msg.Decision.GetAuthenticated() == nil {
			t.Fatal("fixture login", err)
		}
		return response.Msg.Decision.GetAuthenticated()
	}
	first := login(t, rig.login, rig.password)
	const secondLogin = "AUTH H2 independent admin"
	const secondPassword = "AUTH H2 synthetic independent password"
	created, err := accounts.CreateAccount(rig.ctx, nha141BearerRequest(&api.CreateAccountRequest{Parent: "homes/auth-fixture", Account: &api.Account{LoginName: secondLogin, DisplayName: secondLogin, Role: api.AccountRole_ACCOUNT_ROLE_ADMIN, InitialPassword: secondPassword}}, first.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	independent := login(t, secondLogin, secondPassword)
	readIndependent := func(t *testing.T) {
		t.Helper()
		ctx, cancel := context.WithTimeout(rig.ctx, time.Second)
		defer cancel()
		if _, err := accounts.GetAccount(ctx, nha141BearerRequest(&api.GetAccountRequest{Name: created.Msg.Name}, independent.AccessToken)); err != nil {
			t.Fatal("blocked AUTH send stalled independent reader", err)
		}
	}
	revoke := func(t *testing.T, name string) {
		t.Helper()
		if _, err := auth.RevokeSession(rig.ctx, nha141BearerRequest(&api.RevokeSessionRequest{Name: name}, independent.AccessToken)); err != nil {
			t.Fatal("native session revocation", err)
		}
	}

	t.Run("administrative read", func(t *testing.T) {
		blocked := authUnreadH2(t, rig.ctx, server, apiv2connect.AccountServiceGetAccountProcedure, &api.GetAccountRequest{Name: rig.accountName.String()}, first.AccessToken)
		readIndependent(t)
		current, err := accounts.GetAccount(rig.ctx, nha141BearerRequest(&api.GetAccountRequest{Name: rig.accountName.String()}, independent.AccessToken))
		if err != nil {
			t.Fatal(err)
		}
		_, err = accounts.UpdateAccount(rig.ctx, nha141BearerRequest(&api.UpdateAccountRequest{Account: &api.Account{Name: rig.accountName.String(), Etag: current.Msg.Etag, Role: api.AccountRole_ACCOUNT_ROLE_USER}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"role"}}}, independent.AccessToken))
		if err != nil {
			t.Fatal(err)
		}
		authWaitH2Closed(t, blocked)
		readIndependent(t)
	})

	t.Run("interactive enrollment lease", func(t *testing.T) {
		caller := login(t, rig.login, rig.password)
		blocked := authUnreadH2(t, rig.ctx, server, apiv2connect.AuthServiceBeginFactorEnrollmentProcedure, &api.BeginFactorEnrollmentRequest{Parent: rig.accountName.String(), Kind: api.FactorKind_FACTOR_KIND_FIDO2}, caller.AccessToken)
		readIndependent(t)
		revoke(t, caller.Session.Name)
		authWaitH2Closed(t, blocked)
		readIndependent(t)
	})

	t.Run("refresh delivery owner", func(t *testing.T) {
		caller := login(t, rig.login, rig.password)
		request := &api.RefreshSessionRequest{RefreshToken: caller.RefreshToken, RenewalKey: bytes.Repeat([]byte{0x5a}, 32)}
		blocked := authUnreadH2(t, rig.ctx, server, apiv2connect.AuthServiceRefreshSessionProcedure, request, "")
		readIndependent(t)
		revoke(t, caller.Session.Name)
		authWaitH2Closed(t, blocked)
		if _, err := auth.RefreshSession(rig.ctx, connect.NewRequest(request)); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatal("saved renewal resurrected revoked session", err)
		}
		readIndependent(t)
	})

	t.Run("one shot enrollment response", func(t *testing.T) {
		caller := login(t, rig.login, rig.password)
		begin, err := auth.BeginFactorEnrollment(rig.ctx, nha141BearerRequest(&api.BeginFactorEnrollmentRequest{Parent: rig.accountName.String(), Kind: api.FactorKind_FACTOR_KIND_FIDO2}, caller.AccessToken))
		if err != nil {
			t.Fatal(err)
		}
		authenticator := nha142NewAuthenticator(t)
		request := &api.CompleteFactorEnrollmentRequest{Parent: rig.accountName.String(), ChallengeId: begin.Msg.Challenge.ChallengeId, Attestation: authenticator.attestation(t, begin.Msg.Challenge)}
		blocked := authUnreadH2(t, rig.ctx, server, apiv2connect.AuthServiceCompleteFactorEnrollmentProcedure, request, caller.AccessToken)
		readIndependent(t)
		// Activation revoked the initiating session in storage but deliberately
		// permits its one-shot response. A later, independent account disable
		// must still revoke that real delivery lease without replaying codes.
		current, err := accounts.GetAccount(rig.ctx, nha141BearerRequest(&api.GetAccountRequest{Name: rig.accountName.String()}, independent.AccessToken))
		if err != nil || !current.Msg.SecondFactorEnabled {
			t.Fatal("enrollment did not commit before blocked response", err)
		}
		_, err = accounts.UpdateAccount(rig.ctx, nha141BearerRequest(&api.UpdateAccountRequest{Account: &api.Account{Name: rig.accountName.String(), Etag: current.Msg.Etag, State: api.AccountState_ACCOUNT_STATE_DISABLED}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"state"}}}, independent.AccessToken))
		if err != nil {
			t.Fatal(err)
		}
		authWaitH2Closed(t, blocked)
		if _, err := auth.CompleteFactorEnrollment(rig.ctx, nha141BearerRequest(request, caller.AccessToken)); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatal("one-shot enrollment was replayed", err)
		}
		readIndependent(t)
	})
}
