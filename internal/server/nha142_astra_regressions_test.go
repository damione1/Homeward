package server

import (
	"context"
	"crypto/x509"
	"errors"
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
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

type nha142AstraEnrollmentState struct {
	factors, batches, revokedSessions int
	revision                          int64
}

func nha142AstraEnrollmentSnapshot(t *testing.T, rig *nha142ServerRig) nha142AstraEnrollmentState {
	t.Helper()
	account, err := rig.account.Value()
	if err != nil {
		t.Fatal(err)
	}
	var state nha142AstraEnrollmentState
	err = rig.store.InTransaction(rig.ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(rig.ctx, `SELECT
			(SELECT count(*) FROM public.auth_mfa_credentials WHERE account_id=$1 AND removed_at IS NULL),
			(SELECT count(*) FROM public.auth_mfa_recovery_batches WHERE account_id=$1 AND retired_at IS NULL),
			(SELECT count(*) FROM public.auth_sessions WHERE account_id=$1 AND revoked_at IS NOT NULL),
			(SELECT revocation_revision FROM public.auth_accounts WHERE id=$1)`, account).
			Scan(&state.factors, &state.batches, &state.revokedSessions, &state.revision)
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestNHA142AstraFirstEnrollmentRequiresRecentOriginalAuthentication(t *testing.T) {
	rig := nha142NewServerRig(t)
	plain := rig.start(t).GetAuthenticated()
	if plain == nil || plain.Session == nil || plain.Session.Name == "" || plain.Session.AuthenticationTime == nil ||
		plain.Session.AuthenticationSatisfaction != api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_ONLY {
		t.Fatal("false policy did not issue typed password-only original authentication")
	}
	original := plain.Session.AuthenticationTime.AsTime()
	rig.clock.Advance(original.Add(4*time.Minute + 30*time.Second))
	key := nha142NewAuthenticator(t)
	begin := connect.NewRequest(&api.BeginFactorEnrollmentRequest{Parent: rig.accountName.String(), Kind: api.FactorKind_FACTOR_KIND_FIDO2})
	begin.Header().Set("Authorization", "Bearer "+plain.AccessToken)
	begun, err := rig.handler.BeginFactorEnrollment(rig.tlsContext(), begin)
	if err != nil || begun == nil || begun.Msg == nil || begun.Msg.Challenge == nil ||
		begun.Msg.Challenge.ChallengeId == "" || begun.Msg.Challenge.Fido2 == nil || begun.Msg.Challenge.ExpireTime == nil {
		t.Fatalf("recent password-only session could not begin first real enrollment: %v", err)
	}
	staleAt := original.Add(authmfa.RecentAuthenticationLifetime + time.Second)
	if !staleAt.Before(begun.Msg.Challenge.ExpireTime.AsTime()) {
		t.Fatal("setup expired the ceremony before original authentication became stale")
	}
	before := nha142AstraEnrollmentSnapshot(t, rig)
	if before.factors != 0 || before.batches != 0 || before.revokedSessions != 0 {
		t.Fatalf("setup has factor, recovery batch, or revoked session: %+v", before)
	}
	rig.clock.Advance(staleAt)
	complete := connect.NewRequest(&api.CompleteFactorEnrollmentRequest{Parent: rig.accountName.String(),
		ChallengeId: begun.Msg.Challenge.ChallengeId,
		Attestation: key.attestation(t, begun.Msg.Challenge),
	})
	complete.Header().Set("Authorization", "Bearer "+plain.AccessToken)
	if _, err := rig.handler.CompleteFactorEnrollment(rig.tlsContext(), complete); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("stale original password login completed first enrollment: code=%v err=%v", connect.CodeOf(err), err)
	}
	if after := nha142AstraEnrollmentSnapshot(t, rig); after != before {
		t.Errorf("refused first enrollment changed factor/batch/revocation state: before=%+v after=%+v", before, after)
	}
}

func TestNHA142AstraFirstEnrollmentStaleBeginRefused(t *testing.T) {
	rig := nha142NewServerRig(t)
	plain := rig.start(t).GetAuthenticated()
	if plain == nil || plain.Session == nil || plain.Session.AuthenticationTime == nil ||
		plain.Session.AuthenticationSatisfaction != api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_ONLY {
		t.Fatal("false policy did not issue original password-only evidence")
	}
	before := nha142AstraEnrollmentSnapshot(t, rig)
	rig.clock.Advance(plain.Session.AuthenticationTime.AsTime().Add(authmfa.RecentAuthenticationLifetime + time.Second))
	staleBegin := connect.NewRequest(&api.BeginFactorEnrollmentRequest{Parent: rig.accountName.String(), Kind: api.FactorKind_FACTOR_KIND_FIDO2})
	staleBegin.Header().Set("Authorization", "Bearer "+plain.AccessToken)
	if _, err := rig.handler.BeginFactorEnrollment(rig.tlsContext(), staleBegin); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("standalone stale original password login began first enrollment: code=%v err=%v", connect.CodeOf(err), err)
	}
	if after := nha142AstraEnrollmentSnapshot(t, rig); after != before {
		t.Errorf("standalone stale begin changed factor/batch/revocation state: before=%+v after=%+v", before, after)
	}
}

func TestNHA142AstraLegacySessionCannotAuthorizeFirstEnrollment(t *testing.T) {
	rig := nha142NewServerRig(t)
	plain := rig.start(t).GetAuthenticated()
	if plain == nil || plain.Session == nil || plain.Session.Name == "" {
		t.Fatal("false policy did not issue a password session for legacy-row fixture")
	}
	// Migration 18 intentionally admits old, still-current sessions with no
	// original-authentication evidence. Only this database row is converted to
	// that real legacy shape; the bearer and session admission stay genuine.
	nativeSession := nha203NativeSession(t, rig.ctx, rig.store, plain.Session.Name)
	sessionID, err := nativeSession.Value()
	if err != nil {
		t.Fatal(err)
	}
	err = rig.store.InTransaction(rig.ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(rig.ctx, `UPDATE public.auth_sessions SET satisfaction=0,authenticated_at=NULL
			WHERE id=$1 AND satisfaction=1 AND authenticated_at=created_at`, sessionID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var satisfaction int16
	var authenticated *time.Time
	err = rig.store.InTransaction(rig.ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(rig.ctx, `SELECT satisfaction,authenticated_at FROM public.auth_sessions WHERE id=$1`, sessionID).
			Scan(&satisfaction, &authenticated)
	})
	if err != nil || satisfaction != 0 || authenticated != nil {
		t.Fatalf("legacy fixture was not persisted: satisfaction=%d authenticated=%v err=%v", satisfaction, authenticated, err)
	}
	ctx, release := rig.session(t, plain)
	if ctx.Err() != nil {
		t.Fatal("legacy bearer is not a current native session")
	}
	release()
	before := nha142AstraEnrollmentSnapshot(t, rig)
	begin := connect.NewRequest(&api.BeginFactorEnrollmentRequest{Parent: rig.accountName.String(), Kind: api.FactorKind_FACTOR_KIND_FIDO2})
	begin.Header().Set("Authorization", "Bearer "+plain.AccessToken)
	if _, err := rig.handler.BeginFactorEnrollment(rig.tlsContext(), begin); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("legacy session without original evidence began enrollment: code=%v err=%v", connect.CodeOf(err), err)
	}
	if after := nha142AstraEnrollmentSnapshot(t, rig); after != before {
		t.Errorf("legacy enrollment changed factor/batch/revocation state: before=%+v after=%+v", before, after)
	}
}

type nha142AstraPolicyState struct {
	policy                                             *api.SecondFactorPolicy
	revision                                           int64
	sessions, revokedSessions, audits, salts, versions int
}

func nha142AstraPolicySnapshot(t *testing.T, store *storage.Store, ctx context.Context, account string) nha142AstraPolicyState {
	t.Helper()
	policy, err := store.MFAPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := (authResourceNames{nha203AuthFixtureNames(store)}).secondFactorPolicyResource(ctx, policy)
	if err != nil {
		t.Fatal(err)
	}
	state := nha142AstraPolicyState{policy: wire}
	err = store.InTransaction(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT
			(SELECT revocation_revision FROM public.auth_accounts WHERE id=$1),
			(SELECT count(*) FROM public.auth_sessions WHERE account_id=$1),
			(SELECT count(*) FROM public.auth_sessions WHERE account_id=$1 AND revoked_at IS NOT NULL),
			(SELECT count(*) FROM public.auth_audit WHERE target_account_id=$1),
			(SELECT count(*) FROM public.secret_salts),
			(SELECT count(*) FROM public.config_versions)`, account).
			Scan(&state.revision, &state.sessions, &state.revokedSessions, &state.audits, &state.salts, &state.versions)
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func nha142AstraStartRun(t *testing.T, settings config.StartupInputs) (net.Addr, func() error) {
	t.Helper()
	runCtx, cancel := context.WithCancel(context.Background())
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var listener net.ListenConfig
	go func() {
		done <- RunWithStartup(runCtx, settings, listener.Listen, func(addr net.Addr) { ready <- addr })
	}()
	joined := false
	stop := func() error {
		if joined {
			return nil
		}
		cancel()
		select {
		case err := <-done:
			joined = true
			return err
		case <-time.After(10 * time.Second):
			return errors.New("Run did not drain after cancellation")
		}
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Error("Run shutdown:", err)
		}
	})
	select {
	case address := <-ready:
		return address, stop
	case err := <-done:
		joined = true
		cancel()
		t.Fatal("Run exited before TLS ready:", err)
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not become TLS ready")
	}
	return nil, stop
}

func TestNHA142AstraPolicyEnableRequiresCurrentReadyRPAfterRestart(t *testing.T) {
	const origin = "https://nha142.example.test"
	const login = "NHA142 Astra Admin"
	const password = "NHA142 synthetic restart password"
	cluster := testpg.Start(t)
	dataDir := shortPrivateDataDir(t)
	roots := nha181CertificateFixtureWith(t, dataDir, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), func(cert *x509.Certificate) {
		cert.DNSNames = append(cert.DNSNames, "nha142.example.test")
	})
	withURL := nha181Config(t, cluster.Port, dataDir, "127.0.0.1:0",
		strings.Replace(nha181TLSYAML(), "public:\n", "public:\n  url: "+origin+"\n", 1))
	_, stopFirst := nha142AstraStartRun(t, withURL.startup)
	operatorTransport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", filepath.Join(dataDir, operatorSocketName))
	}}
	operatorHTTP := &http.Client{Timeout: 10 * time.Second, Transport: operatorTransport}
	operator := operatorv1connect.NewOperatorAccountServiceClient(operatorHTTP, "http://operator")
	callCtx, cancelCalls := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancelCalls()
	created, err := operator.BootstrapAdmin(callCtx, connect.NewRequest(&authv1.BootstrapAdminRequest{
		LoginName: login, DisplayName: login, Password: password,
	}))
	operatorTransport.CloseIdleConnections()
	if err != nil || created == nil || created.Msg == nil || created.Msg.Account == nil || created.Msg.Account.Id == nil {
		t.Fatal("first Run Unix bootstrap failed:", err)
	}
	account := created.Msg.Account.Id.Value
	if err := stopFirst(); err != nil {
		t.Fatal("first Run did not stop before restart:", err)
	}
	postgres, err := withURL.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	probe, err := storage.Open(callCtx, postgres)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(probe.Close)
	var instance, rpID, persistedOrigin string
	err = probe.InTransaction(callCtx, func(tx pgx.Tx) error {
		return tx.QueryRow(callCtx, `SELECT instance_id,rp_id,origin FROM public.auth_mfa_instance WHERE singleton`).
			Scan(&instance, &rpID, &persistedOrigin)
	})
	if err != nil || instance == "" || rpID != "nha142.example.test" || persistedOrigin != origin {
		t.Fatalf("first real Run did not persist its RP: instance=%q rp=%q origin=%q err=%v", instance, rpID, persistedOrigin, err)
	}
	withoutURL := nha181Config(t, cluster.Port, dataDir, "127.0.0.1:0", nha181TLSYAML())
	withoutURLSettings, err := withoutURL.PublicTransport()
	if err != nil {
		t.Fatal(err)
	}
	if _, present := withoutURLSettings.CanonicalURL(); present || withoutURLSettings.Transport() != config.PublicTransportTLS {
		t.Fatal("restart configuration is not actual public TLS without a canonical RP URL")
	}
	address, _ := nha142AstraStartRun(t, withoutURL.startup)
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
	started, err := auth.StartLogin(callCtx, connect.NewRequest(&api.StartLoginRequest{LoginName: login, Password: password}))
	if err != nil || started == nil || started.Msg == nil || started.Msg.Decision == nil {
		t.Fatal("password login failed after TLS restart without canonical RP:", err)
	}
	plain := started.Msg.Decision.GetAuthenticated()
	if plain == nil || plain.Session == nil || plain.Session.AuthenticationSatisfaction != api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_ONLY {
		t.Fatal("unenrolled admin lost native password admission after RP-less restart")
	}
	if _, err := auth.ListSessions(callCtx, nha142RunBearer(&api.ListSessionsRequest{Parent: nha203SessionAccount(t, plain.Session).String()}, plain.AccessToken)); err != nil {
		t.Fatal("native password bearer not admitted after RP-less restart:", err)
	}
	before := nha142AstraPolicySnapshot(t, probe, callCtx, account)
	if before.policy.RequireAdminFactor || before.policy.Etag == "" || before.policy.UpdateTime == nil || before.revokedSessions != 0 {
		t.Fatalf("unexpected policy/session state before enable: %+v", before)
	}
	currentPolicy, err := accounts.GetSecondFactorPolicy(callCtx, nha142RunBearer(&api.GetSecondFactorPolicyRequest{Name: nha203SessionAccount(t, plain.Session).Home().String() + "/secondFactorPolicy"}, plain.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	_, err = accounts.ChangeSecondFactorPolicy(callCtx, nha142RunBearer(&api.ChangeSecondFactorPolicyRequest{
		Name: currentPolicy.Msg.Name, Etag: currentPolicy.Msg.Etag, RequireAdminFactor: true,
	}, plain.AccessToken))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("policy enable without a ready RP returned code=%v err=%v, want unavailable", connect.CodeOf(err), err)
	}
	after := nha142AstraPolicySnapshot(t, probe, callCtx, account)
	beforeMetadata, afterMetadata := before, after
	beforeMetadata.policy, afterMetadata.policy = nil, nil
	if !proto.Equal(before.policy, after.policy) || beforeMetadata != afterMetadata {
		t.Errorf("refused enable changed policy/SQL state: before=%+v after=%+v", before, after)
	}
	if _, err := auth.ListSessions(callCtx, nha142RunBearer(&api.ListSessionsRequest{Parent: nha203SessionAccount(t, plain.Session).String()}, plain.AccessToken)); err != nil {
		t.Errorf("refused enable revoked still-valid password session: %v", err)
	}
}
