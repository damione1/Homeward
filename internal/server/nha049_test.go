package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nha049Identity struct {
	mu    sync.RWMutex
	admin string
	user  string
}

func (a *nha049Identity) set(admin, user string) {
	a.mu.Lock()
	a.admin, a.user = admin, user
	a.mu.Unlock()
}

func (a *nha049Identity) Authenticate(_ context.Context, request *http.Request) (domain.ActorID, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	switch request.Header.Get("Authorization") {
	case "Bearer fixture-admin":
		if a.admin != "" {
			return domain.ParseActorID(a.admin)
		}
	case "Bearer fixture-user":
		if a.user != "" {
			return domain.ParseActorID(a.user)
		}
	}
	return domain.ActorID{}, errors.New("fixture identity denied")
}

type nha049AllowRoute struct{}

func (nha049AllowRoute) Authorize(context.Context, domain.ActorID, RouteKey) error { return nil }

func nha049Config(t *testing.T, port int, dataDir string) config.Config {
	t.Helper()
	yaml := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\n%spostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n", dataDir, nha181TLSYAML(), port))
	value, err := config.ParseWithEnv(yaml, func(name string) (string, bool) {
		return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD"
	})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func nha049Request[T any](message *T, credential string) *connect.Request[T] {
	request := connect.NewRequest(message)
	if credential != "" {
		request.Header().Set("Authorization", "Bearer fixture-"+credential)
	}
	return request
}

func nha049Code(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if connect.CodeOf(err) != want {
		t.Fatalf("Connect code=%v, want %v: %v", connect.CodeOf(err), want, err)
	}
}

func nha049NoCanaries(t *testing.T, location, content string, canaries ...string) {
	t.Helper()
	for _, canary := range canaries {
		if canary != "" && strings.Contains(content, canary) {
			t.Fatalf("%s exposed a credential canary", location)
		}
	}
}

func nha049Wire(t *testing.T, message proto.Message) string {
	t.Helper()
	encoded, err := protojson.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestNHA049RunConnectUnixAccounts(t *testing.T) {
	const passwordCanary = "NHA049 synthetic password canary 2026"
	const recoveryCanary = "NHA049 synthetic recovery canary 2026"
	cluster := testpg.Start(t)
	dataDir := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, dataDir)
	settings := nha049Config(t, cluster.Port, dataDir)
	identity := &nha049Identity{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var listen net.ListenConfig
	go func() {
		done <- RunWithFactory(ctx, settings, listen.Listen, func(addr net.Addr) { ready <- addr }, func(_ context.Context, store *storage.Store) (RuntimePlan, error) {
			plan, err := NewPublicPlan(identity, nha049AllowRoute{}, accountTransportAudit{store: store})
			return RuntimePlan{Identity: unconfiguredIdentity{}, Public: plan}, err
		})
	}()
	var address net.Addr
	select {
	case address = <-ready:
	case err := <-done:
		t.Fatalf("Run exited before ready: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("Run did not reach ready")
	}
	socket := filepath.Join(dataDir, operatorSocketName)
	info, err := os.Lstat(socket)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 {
		t.Fatalf("operator socket mode/type: %v %v", info, err)
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		t.Fatal("operator socket is not owned by effective UID")
	}
	operatorHTTP := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dial net.Dialer
		return dial.DialContext(ctx, "unix", socket)
	}}}
	operator := authv1connect.NewOperatorAccountServiceClient(operatorHTTP, "http://operator")
	publicHTTP := nha181Client(roots)
	defer publicHTTP.CloseIdleConnections()
	publicURL := "https://" + address.String()
	accountClient := authv1connect.NewAccountServiceClient(publicHTTP, publicURL)
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run shutdown: %v", err)
			}
		case <-time.After(8 * time.Second):
			t.Error("Run did not drain")
		}
		if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("operator socket remained after shutdown: %v", err)
		}
	}()

	// The production public plan has no trusted header identity. This fixture
	// supplies a test authenticator; a missing credential still fails closed.
	_, err = accountClient.CreateAccount(ctx, nha049Request(&authv1.CreateAccountRequest{LoginName: "prebootstrap", DisplayName: "Prebootstrap", Role: authv1.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: "NHA049 prebootstrap password"}, ""))
	nha049Code(t, err, connect.CodeUnauthenticated)
	bootstrap := []*connect.Request[authv1.BootstrapAdminRequest]{
		nha049Request(&authv1.BootstrapAdminRequest{LoginName: "Admin One", DisplayName: "Admin One", Password: "NHA049 admin one password"}, ""),
		nha049Request(&authv1.BootstrapAdminRequest{LoginName: "Admin Two", DisplayName: "Admin Two", Password: "NHA049 admin two password"}, ""),
	}
	var wait sync.WaitGroup
	responses := make([]*connect.Response[authv1.BootstrapAdminResponse], 2)
	errorsSeen := make([]error, 2)
	for i := range bootstrap {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			responses[i], errorsSeen[i] = operator.BootstrapAdmin(ctx, bootstrap[i])
		}(i)
	}
	wait.Wait()
	winners, denied := 0, 0
	var first *authv1.Account
	for i, err := range errorsSeen {
		if err == nil {
			winners++
			first = responses[i].Msg.Account
		} else if connect.CodeOf(err) == connect.CodeFailedPrecondition {
			denied++
		} else {
			t.Fatalf("bootstrap %d: %v", i, err)
		}
	}
	if winners != 1 || denied != 1 || first == nil || first.Role != authv1.AccountRole_ACCOUNT_ROLE_ADMIN {
		t.Fatalf("bootstrap winners=%d denied=%d", winners, denied)
	}
	identity.set(first.Id.Value, "")
	_, err = operator.BootstrapAdmin(ctx, bootstrap[0])
	nha049Code(t, err, connect.CodeFailedPrecondition)
	_, err = accountClient.DeleteAccount(ctx, nha049Request(&authv1.DeleteAccountRequest{AccountId: first.Id}, "admin"))
	nha049Code(t, err, connect.CodeFailedPrecondition)

	user, err := accountClient.CreateAccount(ctx, nha049Request(&authv1.CreateAccountRequest{LoginName: "Ordinary User", DisplayName: "Ordinary User", Role: authv1.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: passwordCanary}, "admin"))
	if err != nil {
		t.Fatal(err)
	}
	nha049NoCanaries(t, "create-account response", nha049Wire(t, user.Msg), passwordCanary)
	_, err = accountClient.CreateAccount(ctx, nha049Request(&authv1.CreateAccountRequest{LoginName: "Ordinary User", DisplayName: "Ordinary User", Role: authv1.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: passwordCanary}, "admin"))
	nha049Code(t, err, connect.CodeAlreadyExists)
	nha049NoCanaries(t, "duplicate-account error", err.Error(), passwordCanary)
	identity.set(first.Id.Value, user.Msg.Account.Id.Value)
	elevation := authv1.AccountRole_ACCOUNT_ROLE_ADMIN
	_, err = accountClient.UpdateAccount(ctx, nha049Request(&authv1.UpdateAccountRequest{AccountId: user.Msg.Account.Id, Role: &elevation}, "user"))
	nha049Code(t, err, connect.CodePermissionDenied)
	_, err = accountClient.DeleteAccount(ctx, nha049Request(&authv1.DeleteAccountRequest{AccountId: first.Id}, "user"))
	nha049Code(t, err, connect.CodePermissionDenied)

	invitation, err := accountClient.CreateInvitation(ctx, nha049Request(&authv1.CreateInvitationRequest{LoginName: "Invited Admin", Role: authv1.AccountRole_ACCOUNT_ROLE_ADMIN, ExpiresAt: timestamppb.New(time.Now().Add(time.Hour))}, "admin"))
	if err != nil || invitation.Msg.InvitationToken == "" || invitation.Msg.Invitation.Role != authv1.AccountRole_ACCOUNT_ROLE_ADMIN {
		t.Fatalf("admin invitation: %v", err)
	}
	nha049NoCanaries(t, "create-invitation response", nha049Wire(t, invitation.Msg), passwordCanary)
	accepted, err := accountClient.AcceptInvitation(ctx, nha049Request(&authv1.AcceptInvitationRequest{InvitationToken: invitation.Msg.InvitationToken, Password: "NHA049 invited admin password", DisplayName: "Invited Admin"}, ""))
	if err != nil || accepted.Msg.Account.Role != authv1.AccountRole_ACCOUNT_ROLE_ADMIN {
		t.Fatalf("invitation acceptance: %v", err)
	}
	nha049NoCanaries(t, "accept-invitation response", nha049Wire(t, accepted.Msg), invitation.Msg.InvitationToken, passwordCanary)
	_, err = accountClient.AcceptInvitation(ctx, nha049Request(&authv1.AcceptInvitationRequest{InvitationToken: invitation.Msg.InvitationToken, Password: "NHA049 invited admin password", DisplayName: "Invited Admin"}, ""))
	nha049Code(t, err, connect.CodeFailedPrecondition)
	nha049NoCanaries(t, "reused-invitation error", err.Error(), invitation.Msg.InvitationToken, passwordCanary)
	unknown, _ := accounts.NewInvitationToken()
	unknownText, _ := unknown.Text()
	_, err = accountClient.AcceptInvitation(ctx, nha049Request(&authv1.AcceptInvitationRequest{InvitationToken: unknownText, Password: "NHA049 unknown token password", DisplayName: "Unknown"}, ""))
	nha049Code(t, err, connect.CodeNotFound)

	storeSettings, _ := settings.PostgresSettings()
	store, err := storage.Open(ctx, storeSettings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tokenValue, err := accounts.ParseInvitationToken(invitation.Msg.InvitationToken)
	if err != nil {
		t.Fatal(err)
	}
	tokenDigest, err := tokenValue.Digest()
	if err != nil {
		t.Fatal(err)
	}
	var persistedDigest []byte
	if err := store.InTransaction(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT token_digest FROM public.auth_invitations WHERE id=$1`, invitation.Msg.Invitation.Id.Value).Scan(&persistedDigest)
	}); err != nil || !bytes.Equal(persistedDigest, tokenDigest[:]) {
		t.Fatalf("invitation digest persistence: %v", err)
	}
	for _, fixture := range []struct {
		login  string
		revoke bool
	}{
		{login: "Expired Invite"},
		{login: "Revoked Invite", revoke: true},
	} {
		created, err := accountClient.CreateInvitation(ctx, nha049Request(&authv1.CreateInvitationRequest{LoginName: fixture.login, Role: authv1.AccountRole_ACCOUNT_ROLE_USER, ExpiresAt: timestamppb.New(time.Now().Add(time.Hour))}, "admin"))
		if err != nil {
			t.Fatal(err)
		}
		if fixture.revoke {
			_, err = accountClient.RevokeInvitation(ctx, nha049Request(&authv1.RevokeInvitationRequest{InvitationId: created.Msg.Invitation.Id}, "admin"))
		} else {
			err = store.InTransaction(ctx, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE public.auth_invitations SET expires_at=created_at+interval '1 microsecond' WHERE id=$1`, created.Msg.Invitation.Id.Value)
				return err
			})
		}
		if err != nil {
			t.Fatal(err)
		}
		_, err = accountClient.AcceptInvitation(ctx, nha049Request(&authv1.AcceptInvitationRequest{InvitationToken: created.Msg.InvitationToken, Password: "NHA049 rejected invitation password", DisplayName: fixture.login}, ""))
		nha049Code(t, err, connect.CodeFailedPrecondition)
	}
	listed, err := accountClient.ListInvitations(ctx, nha049Request(&authv1.ListInvitationsRequest{}, "admin"))
	if err != nil {
		t.Fatalf("invitation list disclosed token: %v", err)
	}
	nha049NoCanaries(t, "list-invitations response", nha049Wire(t, listed.Msg), invitation.Msg.InvitationToken, passwordCanary)
	accountsListed, err := accountClient.ListAccounts(ctx, nha049Request(&authv1.ListAccountsRequest{}, "admin"))
	if err != nil {
		t.Fatal(err)
	}
	nha049NoCanaries(t, "list-accounts response", nha049Wire(t, accountsListed.Msg), invitation.Msg.InvitationToken, passwordCanary)
	login, _ := accounts.ParseLoginName("Invited Admin")
	beforeRecord, err := store.CredentialByLogin(ctx, login)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := beforeRecord.Revision()
	if err := store.InTransaction(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE public.auth_accounts SET second_factor_enabled=true WHERE id=$1`, accepted.Msg.Account.Id.Value)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	recovered, err := operator.RecoverAccount(ctx, nha049Request(&authv1.RecoverAccountRequest{AccountId: accepted.Msg.Account.Id, ReplacementPassword: recoveryCanary}, ""))
	if err != nil || recovered.Msg.Account.Id.Value != accepted.Msg.Account.Id.Value || !recovered.Msg.Account.SecondFactorEnabled {
		t.Fatalf("operator recovery: %v", err)
	}
	nha049NoCanaries(t, "recovery response", nha049Wire(t, recovered.Msg), invitation.Msg.InvitationToken, passwordCanary, recoveryCanary)
	current, err := store.RevisionCurrent(ctx, before)
	if err != nil || current {
		t.Fatalf("old revision admitted: current=%v err=%v", current, err)
	}
	afterRecord, err := store.CredentialByLogin(ctx, login)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := afterRecord.Revision()
	if !after.NewerThan(before) {
		t.Fatal("recovery did not advance typed revision")
	}
	verifier, _ := afterRecord.Verifier()
	match, err := accounts.VerifyPassword(ctx, recoveryCanary, verifier)
	if err != nil || !match {
		t.Fatalf("replacement password rejected: %v", err)
	}
	match, err = accounts.VerifyPassword(ctx, "NHA049 invited admin password", verifier)
	if err != nil || match {
		t.Fatalf("old password admitted: %v", err)
	}
	var persisted string
	if err := store.InTransaction(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(string_agg(payload, E'\n'), '') FROM (
			SELECT row_to_json(a)::text AS payload FROM public.auth_accounts a
			UNION ALL SELECT row_to_json(i)::text FROM public.auth_invitations i
			UNION ALL SELECT row_to_json(c)::text FROM public.auth_login_claims c
			UNION ALL SELECT row_to_json(e)::text FROM public.auth_audit e
		) AS persisted_rows`).Scan(&persisted)
	}); err != nil {
		t.Fatal(err)
	}
	nha049NoCanaries(t, "persisted account/invitation/claim/audit rows", persisted,
		invitation.Msg.InvitationToken, passwordCanary, recoveryCanary)
	publicRecovery, err := publicHTTP.Post(publicURL+authv1connect.OperatorAccountServiceRecoverAccountProcedure, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	publicRecovery.Body.Close()
	if publicRecovery.StatusCode != http.StatusNotFound {
		t.Fatalf("operator RPC public status=%d", publicRecovery.StatusCode)
	}

	// The second active administrator may demote the first, but not itself.
	demotion := authv1.AccountRole_ACCOUNT_ROLE_USER
	_, err = accountClient.UpdateAccount(ctx, nha049Request(&authv1.UpdateAccountRequest{AccountId: first.Id, Role: &demotion}, "admin"))
	if err != nil {
		t.Fatalf("demote first administrator: %v", err)
	}
	identity.set(accepted.Msg.Account.Id.Value, user.Msg.Account.Id.Value)
	_, err = accountClient.UpdateAccount(ctx, nha049Request(&authv1.UpdateAccountRequest{AccountId: accepted.Msg.Account.Id, Role: &demotion}, "admin"))
	nha049Code(t, err, connect.CodeFailedPrecondition)
	_, err = accountClient.GetAccount(ctx, nha049Request(&authv1.GetAccountRequest{AccountId: first.Id}, "user"))
	nha049Code(t, err, connect.CodePermissionDenied)
	if _, err := contracts.ParseAccountID(accepted.Msg.Account.Id.Value); err != nil {
		t.Fatal(err)
	}
	var auditDenied, audited int
	if err := store.InTransaction(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE outcome=2) FROM public.auth_audit`).Scan(&audited, &auditDenied)
	}); err != nil || auditDenied < 5 || audited < 10 {
		t.Fatalf("administrative audit missing: all=%d denied=%d err=%v", audited, auditDenied, err)
	}
}

func TestNHA049OperatorUIDAdmission(t *testing.T) {
	if !admitOperatorUID(501, 501, nil) || admitOperatorUID(502, 501, nil) || admitOperatorUID(501, 501, errors.New("credential unavailable")) {
		t.Fatal("operator UID admission did not fail closed")
	}
}

func TestNHA049DefaultRunSocketRestartAndFailClosed(t *testing.T) {
	cluster := testpg.Start(t)
	dataDir := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, dataDir)
	settings := nha049Config(t, cluster.Port, dataDir)
	socket := filepath.Join(dataDir, operatorSocketName)
	var listen net.ListenConfig
	start := func() (context.CancelFunc, <-chan error, net.Addr) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		ready := make(chan net.Addr, 1)
		done := make(chan error, 1)
		go func() { done <- Run(ctx, settings, listen.Listen, func(addr net.Addr) { ready <- addr }) }()
		select {
		case addr := <-ready:
			return cancel, done, addr
		case err := <-done:
			cancel()
			t.Fatalf("default Run before ready: %v", err)
		case <-time.After(8 * time.Second):
			cancel()
			t.Fatal("default Run did not become ready")
		}
		return nil, nil, nil
	}
	stop := func(cancel context.CancelFunc, done <-chan error) {
		t.Helper()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("default Run shutdown: %v", err)
			}
		case <-time.After(8 * time.Second):
			t.Fatal("default Run did not drain")
		}
		if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("socket remained after drain: %v", err)
		}
	}
	cancel, done, address := start()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	uid, err := operatorPeerUID(conn)
	conn.Close()
	if err != nil || uid != uint32(os.Geteuid()) {
		t.Fatalf("actual Unix peer UID=%d: %v", uid, err)
	}
	publicHTTP := nha181Client(roots)
	defer publicHTTP.CloseIdleConnections()
	public := authv1connect.NewAccountServiceClient(publicHTTP, "https://"+address.String())
	_, err = public.CreateAccount(context.Background(), nha049Request(&authv1.CreateAccountRequest{LoginName: "Closed Route", DisplayName: "Closed Route", Role: authv1.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: "NHA049 closed route password"}, "admin"))
	nha049Code(t, err, connect.CodeUnauthenticated)
	storeSettings, _ := settings.PostgresSettings()
	store, err := storage.Open(context.Background(), storeSettings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	coordinator, err := newSessionCoordinator(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := startOperatorListener(context.Background(), dataDir, store, coordinator); !errors.Is(err, ErrOperatorSocketInUse) {
		t.Fatalf("second server stole active socket: %v", err)
	}
	operatorHTTP := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dial net.Dialer
		return dial.DialContext(ctx, "unix", socket)
	}}}
	operator := authv1connect.NewOperatorAccountServiceClient(operatorHTTP, "http://operator")
	if _, err := operator.BootstrapAdmin(context.Background(), nha049Request(&authv1.BootstrapAdminRequest{LoginName: "Persistent Admin", DisplayName: "Persistent Admin", Password: "NHA049 persistent admin password"}, "")); err != nil {
		t.Fatal(err)
	}
	stop(cancel, done)
	if err := os.Symlink(filepath.Join(dataDir, "other"), socket); err != nil {
		t.Fatal(err)
	}
	unsafeCtx, unsafeCancel := context.WithTimeout(context.Background(), 3*time.Second)
	err = Run(unsafeCtx, settings, listen.Listen, nil)
	unsafeCancel()
	if !errors.Is(err, ErrOperatorSocketUnsafe) {
		t.Fatalf("socket symlink accepted: %v", err)
	}
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	cancel, done, _ = start()
	_, err = operator.BootstrapAdmin(context.Background(), nha049Request(&authv1.BootstrapAdminRequest{LoginName: "Another Admin", DisplayName: "Another Admin", Password: "NHA049 another admin password"}, ""))
	nha049Code(t, err, connect.CodeFailedPrecondition)
	stop(cancel, done)
	longDir := filepath.Join(dataDir, strings.Repeat("x", operatorSocketPathLimit))
	if _, err := startOperatorListener(context.Background(), longDir, store, coordinator); !errors.Is(err, ErrOperatorSocketPathTooLong) {
		t.Fatalf("long socket path was not typed: %v", err)
	}
}
