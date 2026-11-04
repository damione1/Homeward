package server

import (
	"context"
	"crypto/x509"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/operator/v1/operatorv1connect"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/damione1/Homeward/internal/testpg"
)

func nha142RunBearer[T any](message *T, token string) *connect.Request[T] {
	request := connect.NewRequest(message)
	request.Header().Set("Authorization", "Bearer "+token)
	return request
}

// Run has no configured pilots or device outputs in this fixture. Its normal
// composition, Unix operator socket, public TLS listener and Connect mux run.
func TestNHA142DefaultRunTLSMFAAndAdminEnrollmentBoundary(t *testing.T) {
	const origin = "https://nha142.example.test"
	const adminName = "NHA142 Run Admin"
	const adminPassword = "NHA142 synthetic Run admin password"
	const secondName = "NHA142 Run Second Admin"
	const secondPassword = "NHA142 synthetic Run second admin password"
	cluster := testpg.Start(t)
	dataDir := shortPrivateDataDir(t)
	roots := nha181CertificateFixtureWith(t, dataDir, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), func(cert *x509.Certificate) {
		cert.DNSNames = append(cert.DNSNames, "nha142.example.test")
	})
	publicYAML := strings.Replace(nha181TLSYAML(), "public:\n", "public:\n  url: "+origin+"\n", 1)
	settings := nha181Config(t, cluster.Port, dataDir, "127.0.0.1:0", publicYAML)

	runCtx, stopRun := context.WithCancel(context.Background())
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var listener net.ListenConfig
	go func() {
		done <- RunWithStartup(runCtx, settings.startup, listener.Listen, func(addr net.Addr) { ready <- addr })
	}()
	joined := false
	t.Cleanup(func() {
		stopRun()
		if joined {
			return
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error("default Run shutdown:", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("default Run did not drain")
		}
	})
	var address net.Addr
	select {
	case address = <-ready:
	case err := <-done:
		joined = true
		t.Fatal("default Run exited before TLS ready:", err)
	case <-time.After(10 * time.Second):
		t.Fatal("default Run did not become ready")
	}

	operatorTransport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", filepath.Join(dataDir, operatorSocketName))
	}}
	operatorHTTP := &http.Client{Timeout: 10 * time.Second, Transport: operatorTransport}
	t.Cleanup(operatorTransport.CloseIdleConnections)
	operator := operatorv1connect.NewOperatorAccountServiceClient(operatorHTTP, "http://operator")
	publicHTTP := nha181Client(roots)
	publicHTTP.Timeout = 10 * time.Second
	publicTransport := publicHTTP.Transport.(*http.Transport)
	publicTransport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", address.String())
	}
	t.Cleanup(publicHTTP.CloseIdleConnections)
	auth := apiv2connect.NewAuthServiceClient(publicHTTP, origin)
	accounts := apiv2connect.NewAccountServiceClient(publicHTTP, origin)
	callCtx, stopCalls := context.WithTimeout(context.Background(), 70*time.Second)
	defer stopCalls()

	bootstrap, err := operator.BootstrapAdmin(callCtx, connect.NewRequest(&authv1.BootstrapAdminRequest{
		LoginName: adminName, DisplayName: adminName, Password: adminPassword,
	}))
	if err != nil || bootstrap == nil || bootstrap.Msg.Account == nil || bootstrap.Msg.Account.Id == nil {
		t.Fatal("Unix operator bootstrap failed:", err)
	}
	adminID := bootstrap.Msg.Account.Id
	login := func(name, password string) *api.LoginDecision {
		t.Helper()
		response, err := auth.StartLogin(callCtx, connect.NewRequest(&api.StartLoginRequest{LoginName: name, Password: password}))
		if err != nil || response == nil || response.Msg.Decision == nil {
			t.Fatal("TLS password login failed:", err)
		}
		return response.Msg.Decision
	}
	plain := login(adminName, adminPassword).GetAuthenticated()
	if plain == nil {
		t.Fatal("default false policy did not admit password login")
	}
	adminAccount := nha203SessionAccount(t, plain.Session)
	policyName := adminAccount.Home().String() + "/secondFactorPolicy"
	policy, err := accounts.GetSecondFactorPolicy(callCtx, nha142RunBearer(&api.GetSecondFactorPolicyRequest{Name: policyName}, plain.AccessToken))
	if err != nil || policy == nil || policy.Msg == nil || policy.Msg.Etag == "" || policy.Msg.RequireAdminFactor {
		t.Fatal("default Run did not bootstrap a disabled, versioned MFA policy:", err)
	}

	authenticator := nha142NewAuthenticator(t)
	begun, err := auth.BeginFactorEnrollment(callCtx, nha142RunBearer(&api.BeginFactorEnrollmentRequest{Parent: adminAccount.String(), Kind: api.FactorKind_FACTOR_KIND_FIDO2}, plain.AccessToken))
	if err != nil || begun == nil || begun.Msg.Challenge == nil || begun.Msg.Challenge.Fido2 == nil {
		t.Fatal("TLS bearer enrollment did not yield FIDO2 options:", err)
	}
	if begun.Msg.Challenge.Fido2.Origin != origin {
		t.Fatal("enrollment did not use the configured canonical HTTPS origin")
	}
	activated, err := auth.CompleteFactorEnrollment(callCtx, nha142RunBearer(&api.CompleteFactorEnrollmentRequest{Parent: adminAccount.String(),
		ChallengeId: begun.Msg.Challenge.ChallengeId,
		Attestation: authenticator.attestation(t, begun.Msg.Challenge),
	}, plain.AccessToken))
	if err != nil || activated == nil || activated.Msg.FactorCredential == nil || len(activated.Msg.RecoveryCodes) != authmfa.RecoveryCodeCount {
		t.Fatal("TLS enrollment did not activate a credential and one-time recovery batch:", err)
	}
	listWith := func(token string) error {
		_, err := auth.ListSessions(callCtx, nha142RunBearer(&api.ListSessionsRequest{Parent: adminAccount.String()}, token))
		return err
	}
	if _, err := auth.ListSessions(callCtx, connect.NewRequest(&api.ListSessionsRequest{Parent: adminAccount.String()})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("protected session route admitted an anonymous caller:", connect.CodeOf(err))
	}
	if code := connect.CodeOf(listWith(plain.AccessToken)); code != connect.CodeUnauthenticated {
		t.Fatal("old password access survived enrollment:", code)
	}
	if _, err := auth.RefreshSession(callCtx, connect.NewRequest(&api.RefreshSessionRequest{RefreshToken: plain.RefreshToken, RenewalKey: []byte("nha198-saved-operation-secret-001")})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("old password refresh survived enrollment:", connect.CodeOf(err))
	}

	challenge := login(adminName, adminPassword).GetChallenge()
	if challenge == nil {
		t.Fatal("enrolled account did not receive a challenge over TLS")
	}
	withoutUV := authenticator.assertion(t, challenge, 0x01)
	if _, err := auth.CompleteLogin(callCtx, connect.NewRequest(&api.CompleteLoginRequest{ChallengeId: challenge.ChallengeId, Proof: withoutUV})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("presence without user verification was admitted:", connect.CodeOf(err))
	}
	withUV := authenticator.assertion(t, challenge, 0x05)
	completed, err := auth.CompleteLogin(callCtx, connect.NewRequest(&api.CompleteLoginRequest{ChallengeId: challenge.ChallengeId, Proof: withUV}))
	if err != nil || completed == nil || completed.Msg.Decision.GetAuthenticated() == nil {
		t.Fatal("valid signed assertion failed on the same challenge:", err)
	}
	mfaTokens := completed.Msg.Decision.GetAuthenticated()
	if mfaTokens.Session == nil || mfaTokens.Session.AuthenticationSatisfaction != api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_FIDO2 {
		t.Fatal("signed assertion did not issue FIDO2 provenance")
	}
	if _, err := auth.CompleteLogin(callCtx, connect.NewRequest(&api.CompleteLoginRequest{ChallengeId: challenge.ChallengeId, Proof: withUV})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("consumed assertion challenge replayed:", connect.CodeOf(err))
	}
	listed, err := auth.ListFactorCredentials(callCtx, nha142RunBearer(&api.ListFactorCredentialsRequest{Parent: adminAccount.String()}, mfaTokens.AccessToken))
	if err != nil || listed == nil || len(listed.Msg.FactorCredentials) != 1 || listed.Msg.FactorCredentials[0].Name != activated.Msg.FactorCredential.Name || listed.Msg.FactorCredentials[0].Kind != api.FactorKind_FACTOR_KIND_FIDO2 {
		t.Fatal("TLS ListFactorCredentials lost the activated credential metadata:", err)
	}

	second, err := accounts.CreateAccount(callCtx, nha142RunBearer(&api.CreateAccountRequest{Parent: adminAccount.Home().String(), Account: &api.Account{
		LoginName: secondName, DisplayName: secondName, Role: api.AccountRole_ACCOUNT_ROLE_ADMIN, InitialPassword: secondPassword,
	}}, mfaTokens.AccessToken))
	if err != nil || second == nil || second.Msg == nil || second.Msg.Name == "" {
		t.Fatal("could not prepare an unenrolled admin for policy admission:", err)
	}
	current, err := accounts.GetSecondFactorPolicy(callCtx, nha142RunBearer(&api.GetSecondFactorPolicyRequest{Name: policyName}, mfaTokens.AccessToken))
	if err != nil || current == nil || current.Msg == nil || current.Msg.Etag == "" {
		t.Fatal("could not read current MFA policy for CAS:", err)
	}
	updated, err := accounts.ChangeSecondFactorPolicy(callCtx, nha142RunBearer(&api.ChangeSecondFactorPolicyRequest{
		Name: current.Msg.Name, Etag: current.Msg.Etag, RequireAdminFactor: true,
	}, mfaTokens.AccessToken))
	if err != nil || updated == nil || updated.Msg.Policy == nil || !updated.Msg.Policy.RequireAdminFactor {
		t.Fatal("TLS policy activation failed:", err)
	}
	grant := login(secondName, secondPassword).GetEnrollmentRequired()
	if grant == nil || grant.Account == "" || grant.Account != second.Msg.Name {
		t.Fatal("unenrolled required admin received privileged access instead of enrollment grant")
	}
	if code := connect.CodeOf(listWith(grant.EnrollmentToken)); code != connect.CodeUnauthenticated {
		t.Fatal("enrollment grant crossed protected access admission:", code)
	}
	if _, err := accounts.ListAccounts(callCtx, nha142RunBearer(&api.ListAccountsRequest{Parent: adminAccount.Home().String()}, grant.EnrollmentToken)); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("enrollment grant crossed privileged account admission:", connect.CodeOf(err))
	}

	replacement, err := auth.ReplaceRecoveryCodes(callCtx, nha142RunBearer(&api.ReplaceRecoveryCodesRequest{Name: adminAccount.String()}, mfaTokens.AccessToken))
	if err != nil || replacement == nil || len(replacement.Msg.RecoveryCodes) != authmfa.RecoveryCodeCount {
		t.Fatal("recent FIDO2 authentication could not rotate recovery codes:", err)
	}
	if code := connect.CodeOf(listWith(mfaTokens.AccessToken)); code != connect.CodeUnauthenticated {
		t.Fatal("recovery rotation did not revoke the caller session:", code)
	}

	// The rotated recovery batch invalidated the caller. Establish a new
	// session through the actual TLS password and FIDO2 assertion flow before
	// probing the public operator route.
	freshChallenge := login(adminName, adminPassword).GetChallenge()
	if freshChallenge == nil || freshChallenge.ChallengeId == "" {
		t.Fatal("enrolled admin did not receive a fresh factor challenge")
	}
	freshCompleted, err := auth.CompleteLogin(callCtx, connect.NewRequest(&api.CompleteLoginRequest{
		ChallengeId: freshChallenge.ChallengeId,
		Proof:       authenticator.assertion(t, freshChallenge, 0x05),
	}))
	if err != nil || freshCompleted == nil || freshCompleted.Msg == nil || freshCompleted.Msg.Decision == nil {
		t.Fatal("fresh FIDO2 login after code rotation failed:", err)
	}
	freshTokens := freshCompleted.Msg.Decision.GetAuthenticated()
	if freshTokens == nil || freshTokens.Session == nil ||
		freshTokens.Session.AuthenticationSatisfaction != api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_FIDO2 {
		t.Fatal("fresh signed assertion did not issue FIDO2 provenance")
	}

	publicOperator := operatorv1connect.NewOperatorAccountServiceClient(publicHTTP, origin)
	if _, err := publicOperator.ResetSecondFactor(callCtx, connect.NewRequest(&authv1.ResetSecondFactorRequest{
		AccountId: adminID,
	})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatal("public TLS routed Unix-only factor reset:", connect.CodeOf(err))
	}
	still, err := auth.ListFactorCredentials(callCtx, nha142RunBearer(&api.ListFactorCredentialsRequest{
		Parent: adminAccount.String(),
	}, freshTokens.AccessToken))
	if err != nil || still == nil || still.Msg == nil || len(still.Msg.FactorCredentials) != 1 ||
		still.Msg.FactorCredentials[0].Name == "" || still.Msg.FactorCredentials[0].Name != activated.Msg.FactorCredential.Name {
		t.Fatal("public reset probe changed factor state or invalidated the caller:", err)
	}

	reset, err := operator.ResetSecondFactor(callCtx, connect.NewRequest(&authv1.ResetSecondFactorRequest{
		AccountId: adminID,
	}))
	if err != nil || reset == nil || reset.Msg == nil || reset.Msg.Account == nil ||
		reset.Msg.Account.Id == nil || reset.Msg.Account.Id.Value != adminID.Value ||
		reset.Msg.Account.SecondFactorEnabled {
		t.Fatal("Unix operator reset did not disable the target account factor:", err)
	}
	if code := connect.CodeOf(listWith(freshTokens.AccessToken)); code != connect.CodeUnauthenticated {
		t.Fatal("Unix factor reset did not revoke the FIDO2 access:", code)
	}
	if _, err := auth.RefreshSession(callCtx, connect.NewRequest(&api.RefreshSessionRequest{
		RefreshToken: freshTokens.RefreshToken, RenewalKey: []byte("nha198-saved-operation-secret-001"),
	})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("Unix factor reset did not revoke the FIDO2 refresh:", connect.CodeOf(err))
	}
	required := login(adminName, adminPassword).GetEnrollmentRequired()
	if required == nil || required.Account == "" || required.Account != adminAccount.String() ||
		required.EnrollmentToken == "" {
		t.Fatal("required admin did not return to restricted enrollment after Unix reset")
	}
}
