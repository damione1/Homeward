package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	cfg "github.com/damione1/Homeward/gen/home/configuration/v1"
	wire "github.com/damione1/Homeward/gen/home/integrationadmin/v1"
	"github.com/damione1/Homeward/gen/home/integrationadmin/v1/integrationadminv1connect"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const nha143Password = "NHA143 synthetic password should never appear in captured output"
const nha143Access = "NHA143-opaque-access-secret"
const nha143Refresh = "NHA143-opaque-refresh-secret"
const nha143Invitation = "NHA143-opaque-invitation-secret"

type nha143Terminal struct {
	calls  int
	values []string
}

func (f *nha143Terminal) ReadPassword(string) ([]byte, error) {
	f.calls++
	if len(f.values) == 0 {
		return nil, errors.New("no terminal input")
	}
	value := f.values[0]
	f.values = f.values[1:]
	return []byte(value), nil
}
func nha143Private(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
func nha143Run(t *testing.T, dir string, terminal Terminal, want int, args ...string) string {
	t.Helper()
	var out, diagnostic bytes.Buffer
	argv := append([]string{"--config-dir", dir}, args...)
	code := Run(context.Background(), argv, &out, &diagnostic, terminal)
	captured := out.String() + diagnostic.String()
	for _, secret := range []string{nha143Password, nha143Access, nha143Refresh, nha143Invitation} {
		if strings.Contains(captured, secret) {
			t.Fatal("credential leaked in CLI output")
		}
	}
	if code != want {
		t.Fatalf("argv %v code=%d want=%d output=%s", args, code, want, captured)
	}
	return captured
}

func TestNHA143KongArgvHelpAndUsage(t *testing.T) {
	dir := nha143Private(t)
	terminal := &nha143Terminal{}
	for _, command := range [][]string{{"--help"}, {"login", "--help"}, {"sessions", "list", "--help"}, {"accounts", "create", "--help"}, {"accounts", "invite", "--help"}, {"accounts", "reset", "--help"}, {"bootstrap", "--help"}, {"recovery", "--help"}, {"integrations", "catalogue", "--help"}, {"integrations", "create", "--help"}, {"integrations", "update", "--help"}, {"integrations", "delete", "--help"}, {"integrations", "enable", "--help"}, {"integrations", "disable", "--help"}, {"integrations", "health", "--help"}} {
		if out := nha143Run(t, dir, terminal, ExitOK, command...); !strings.Contains(out, "Usage:") {
			t.Fatal("Kong contextual help missing")
		}
	}
	for _, command := range [][]string{{}, {"bogus"}, {"login"}, {"login", "admin", "--password", nha143Password}, {"login", "admin", nha143Password}, {"accounts", "create", "u", "--display-name", "u", "--role", "root"}, {"integrations", "delete", "id"}} {
		nha143Run(t, dir, terminal, ExitUsage, command...)
	}
	if terminal.calls != 0 {
		t.Fatal("parser touched terminal")
	}
}

type nha143API struct {
	authv1connect.UnimplementedAuthServiceHandler
	authv1connect.UnimplementedAccountServiceHandler
	integrationadminv1connect.UnimplementedIntegrationServiceHandler
	mu               sync.Mutex
	calls            []string
	challenge        bool
	deny             bool
	token            string
	preconditions    []*wire.MutationPrecondition
	refreshError     connect.Code
	revokeError      connect.Code
	activation       *cfg.ActivationResult
	mutationOverride bool
}

func (f *nha143API) record(name string, header http.Header) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	if header.Get("Authorization") != "Bearer "+f.token {
		return connect.NewError(connect.CodeUnauthenticated, errors.New(nha143Access))
	}
	if f.deny {
		e := connect.NewError(connect.CodePermissionDenied, errors.New(nha143Password))
		d, _ := connect.NewErrorDetail(&auth.AuthError{Code: auth.AuthErrorCode_AUTH_ERROR_CODE_PERMISSION_DENIED, Detail: &auth.AuthError_Permission{Permission: auth.PermissionFailure_PERMISSION_FAILURE_SCOPE_DENIED}})
		e.AddDetail(d)
		return e
	}
	return nil
}
func (f *nha143API) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }
func nha143Tokens(token string) *auth.SessionTokens {
	return &auth.SessionTokens{Session: &auth.Session{Id: &auth.SessionId{Value: "session-1"}, AccountId: &auth.AccountId{Value: "account-1"}}, Audience: auth.CredentialAudience_CREDENTIAL_AUDIENCE_INTERACTIVE, AccessToken: token, RefreshToken: nha143Refresh, AccessExpiresAt: timestamppb.New(time.Now().Add(time.Hour))}
}
func (f *nha143API) StartLogin(_ context.Context, r *connect.Request[auth.StartLoginRequest]) (*connect.Response[auth.StartLoginResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "login")
	if r.Msg.Password != nha143Password {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New(nha143Password))
	}
	decision := &auth.LoginDecision{Outcome: &auth.LoginDecision_Authenticated{Authenticated: nha143Tokens(f.token)}}
	if f.challenge {
		decision.Outcome = &auth.LoginDecision_Challenge{Challenge: &auth.FactorChallenge{Id: &auth.ChallengeId{Value: "factor-challenge"}}}
	}
	return connect.NewResponse(&auth.StartLoginResponse{Decision: decision}), nil
}
func (f *nha143API) RefreshSession(_ context.Context, r *connect.Request[auth.RefreshSessionRequest]) (*connect.Response[auth.RefreshSessionResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "refresh")
	if f.refreshError != 0 {
		return nil, connect.NewError(f.refreshError, errors.New(nha143Refresh))
	}
	if r.Msg.RefreshToken != nha143Refresh {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New(nha143Refresh))
	}
	return connect.NewResponse(&auth.RefreshSessionResponse{Tokens: nha143Tokens(f.token)}), nil
}
func (f *nha143API) ListSessions(_ context.Context, r *connect.Request[auth.ListSessionsRequest]) (*connect.Response[auth.ListSessionsResponse], error) {
	if err := f.record("sessions", r.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&auth.ListSessionsResponse{Sessions: []*auth.Session{nha143Tokens(f.token).Session}}), nil
}
func (f *nha143API) RevokeSession(_ context.Context, r *connect.Request[auth.RevokeSessionRequest]) (*connect.Response[auth.RevokeSessionResponse], error) {
	if err := f.record("revoke", r.Header()); err != nil {
		return nil, err
	}
	f.mu.Lock()
	code := f.revokeError
	f.mu.Unlock()
	if code != 0 {
		return nil, connect.NewError(code, errors.New(nha143Access))
	}
	return connect.NewResponse(&auth.RevokeSessionResponse{}), nil
}
func (f *nha143API) ListAccounts(_ context.Context, r *connect.Request[auth.ListAccountsRequest]) (*connect.Response[auth.ListAccountsResponse], error) {
	if err := f.record("accounts", r.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&auth.ListAccountsResponse{}), nil
}
func (f *nha143API) GetAccount(_ context.Context, r *connect.Request[auth.GetAccountRequest]) (*connect.Response[auth.GetAccountResponse], error) {
	if err := f.record("account", r.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&auth.GetAccountResponse{Account: &auth.Account{Id: r.Msg.AccountId}}), nil
}
func (f *nha143API) CreateAccount(_ context.Context, r *connect.Request[auth.CreateAccountRequest]) (*connect.Response[auth.CreateAccountResponse], error) {
	if err := f.record("create-account", r.Header()); err != nil {
		return nil, err
	}
	if r.Msg.InitialPassword != nha143Password {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("password"))
	}
	return connect.NewResponse(&auth.CreateAccountResponse{Account: &auth.Account{Id: &auth.AccountId{Value: "created"}, Role: r.Msg.Role}}), nil
}
func (f *nha143API) CreateInvitation(_ context.Context, r *connect.Request[auth.CreateInvitationRequest]) (*connect.Response[auth.CreateInvitationResponse], error) {
	if err := f.record("invite", r.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&auth.CreateInvitationResponse{Invitation: &auth.Invitation{Id: &auth.InvitationId{Value: "invitation-1"}}, InvitationToken: nha143Invitation}), nil
}
func (f *nha143API) ListIntegrationTypes(_ context.Context, r *connect.Request[wire.ListIntegrationTypesRequest]) (*connect.Response[wire.ListIntegrationTypesResponse], error) {
	if err := f.record("catalogue", r.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&wire.ListIntegrationTypesResponse{}), nil
}
func (f *nha143API) GetIntegrationType(_ context.Context, r *connect.Request[wire.GetIntegrationTypeRequest]) (*connect.Response[wire.GetIntegrationTypeResponse], error) {
	if err := f.record("schema", r.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&wire.GetIntegrationTypeResponse{Type: &wire.IntegrationType{TypeId: r.Msg.TypeId}}), nil
}
func (f *nha143API) ListInstances(_ context.Context, r *connect.Request[wire.ListInstancesRequest]) (*connect.Response[wire.ListInstancesResponse], error) {
	if err := f.record("instances", r.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&wire.ListInstancesResponse{}), nil
}
func (f *nha143API) GetInstance(_ context.Context, r *connect.Request[wire.GetInstanceRequest]) (*connect.Response[wire.GetInstanceResponse], error) {
	if err := f.record("instance", r.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&wire.GetInstanceResponse{Instance: &wire.IntegrationInstance{IntegrationInstanceId: r.Msg.IntegrationInstanceId, DesiredState: wire.DesiredState_DESIRED_STATE_DISABLED}}), nil
}
func (f *nha143API) mutate(name string, header http.Header, p *wire.MutationPrecondition) error {
	if err := f.record(name, header); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.preconditions = append(f.preconditions, p)
	return nil
}
func (f *nha143API) CreateInstance(_ context.Context, r *connect.Request[wire.CreateInstanceRequest]) (*connect.Response[wire.CreateInstanceResponse], error) {
	if err := f.mutate("create-instance", r.Header(), r.Msg.Precondition); err != nil {
		return nil, err
	}
	return connect.NewResponse(&wire.CreateInstanceResponse{Activation: f.activationResult()}), nil
}
func (f *nha143API) UpdateInstance(_ context.Context, r *connect.Request[wire.UpdateInstanceRequest]) (*connect.Response[wire.UpdateInstanceResponse], error) {
	if err := f.mutate("update-instance", r.Header(), r.Msg.Precondition); err != nil {
		return nil, err
	}
	return connect.NewResponse(&wire.UpdateInstanceResponse{Activation: f.activationResult()}), nil
}
func (f *nha143API) DeleteInstance(_ context.Context, r *connect.Request[wire.DeleteInstanceRequest]) (*connect.Response[wire.DeleteInstanceResponse], error) {
	if err := f.mutate("delete-instance", r.Header(), r.Msg.Precondition); err != nil {
		return nil, err
	}
	return connect.NewResponse(&wire.DeleteInstanceResponse{Activation: f.activationResult()}), nil
}
func (f *nha143API) SetInstanceEnabled(_ context.Context, r *connect.Request[wire.SetInstanceEnabledRequest]) (*connect.Response[wire.SetInstanceEnabledResponse], error) {
	if err := f.mutate("set-enabled", r.Header(), r.Msg.Precondition); err != nil {
		return nil, err
	}
	return connect.NewResponse(&wire.SetInstanceEnabledResponse{Activation: f.activationResult()}), nil
}
func nha143TLS(t *testing.T, token string) (*httptest.Server, *nha143API, string) {
	t.Helper()
	api := &nha143API{token: token}
	mux := http.NewServeMux()
	mux.Handle(authv1connect.NewAuthServiceHandler(api))
	mux.Handle(authv1connect.NewAccountServiceHandler(api))
	mux.Handle(integrationadminv1connect.NewIntegrationServiceHandler(api))
	certificate, key := nha143Certificate(t)
	pair, err := tls.X509KeyPair(certificate, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(mux)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13}
	server.StartTLS()
	t.Cleanup(server.Close)
	ca := filepath.Join(nha143Private(t), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	return server, api, ca
}
func nha143Login(t *testing.T, dir, profile string) {
	t.Helper()
	terminal := &nha143Terminal{values: []string{nha143Password}}
	nha143Run(t, dir, terminal, ExitOK, "--profile", profile, "login", "admin")
	if terminal.calls != 1 {
		t.Fatal("password must be read once through terminal")
	}
}

func TestNHA143TwoTLSProfilesNoFallbackAndCredentials(t *testing.T) {
	dir := nha143Private(t)
	one, first, ca := nha143TLS(t, nha143Access)
	two, second, _ := nha143TLS(t, "other-instance-access")
	terminal := &nha143Terminal{}
	nha143Run(t, dir, terminal, ExitOK, "profiles", "add", "one", "--url", one.URL, "--ca", ca)
	nha143Run(t, dir, terminal, ExitOK, "profiles", "add", "two", "--url", two.URL, "--ca", ca)
	nha143Login(t, dir, "one")
	nha143Run(t, dir, terminal, ExitOK, "--profile", "one", "sessions", "list")
	if first.count() != 2 || second.count() != 0 {
		t.Fatal("wrong server targeted")
	}
	nha143Run(t, dir, terminal, ExitUsage, "--profile", "missing", "accounts", "list")
	nha143Run(t, dir, terminal, ExitUsage, "accounts", "list")
	// Independent trust anchors: profile two must not accept server one's certificate.
	nha143Run(t, dir, &nha143Terminal{values: []string{nha143Password}}, ExitUnavailable, "--profile", "two", "login", "admin")
	if first.count() != 2 || second.count() != 0 {
		t.Fatal("TLS failure switched server or reached handler")
	}
	info, err := os.Stat(filepath.Join(dir, "one.credentials.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential permissions", err)
	}
	for _, command := range [][]string{{"bootstrap", "admin", "--display-name", "Admin"}, {"recovery", "account-1"}, {"accounts", "reset", "account-1"}} {
		nha143Run(t, dir, terminal, ExitPermission, append([]string{"--profile", "one"}, command...)...)
	}
	if terminal.calls != 0 || first.count() != 2 {
		t.Fatal("remote operator command touched terminal/network")
	}
	nha143Run(t, dir, terminal, ExitOK, "--profile", "one", "logout")
	if first.count() != 3 {
		t.Fatal("logout did not revoke remote session")
	}
	if _, err := os.Stat(filepath.Join(dir, "one.credentials.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("logout kept credential")
	}
}

func TestNHA143ConnectCommandsAndExplicitCAS(t *testing.T) {
	dir := nha143Private(t)
	server, api, ca := nha143TLS(t, nha143Access)
	terminal := &nha143Terminal{}
	nha143Run(t, dir, terminal, ExitOK, "profiles", "add", "home", "--url", server.URL, "--ca", ca)
	nha143Login(t, dir, "home")
	for _, command := range [][]string{{"accounts", "list"}, {"accounts", "get", "account-1"}, {"sessions", "list", "account-1"}, {"sessions", "revoke", "other-session"}, {"integrations", "catalogue"}, {"integrations", "schema", "mqtt"}, {"integrations", "list"}, {"integrations", "get", "isolated"}, {"integrations", "health", "isolated"}} {
		nha143Run(t, dir, terminal, ExitOK, append([]string{"--profile", "home"}, command...)...)
	}
	nha143Run(t, dir, &nha143Terminal{values: []string{nha143Password, nha143Password}}, ExitOK, "--profile", "home", "accounts", "create", "user", "--display-name", "User", "--role", "user")
	nha143Run(t, dir, terminal, ExitOK, "--profile", "home", "accounts", "invite", "guest", "--role", "user", "--expires", time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "--save-token", "guest")
	secret, err := os.ReadFile(filepath.Join(dir, "home.invitation-guest.json"))
	if err != nil || !bytes.Contains(secret, []byte(nha143Invitation)) {
		t.Fatal("invitation token not retained privately")
	}
	clear(secret)
	config := filepath.Join(dir, "input.json")
	if err := os.WriteFile(config, []byte(`{"schema_version":"1","yaml_source":"e30="}`), 0600); err != nil {
		t.Fatal(err)
	}
	cas := []string{"--revision", "7", "--source-sha256", strings.Repeat("a", 64), "--workspace-sha256", strings.Repeat("b", 64)}
	for _, command := range [][]string{{"integrations", "create", "mqtt", "isolated", "--config", config}, {"integrations", "update", "mqtt", "isolated", "--config", config}, {"integrations", "delete", "isolated"}, {"integrations", "enable", "isolated"}, {"integrations", "disable", "isolated"}} {
		argv := append([]string{"--profile", "home"}, command...)
		argv = append(argv, cas...)
		nha143Run(t, dir, terminal, ExitOK, argv...)
	}
	api.mu.Lock()
	if len(api.preconditions) != 5 {
		t.Fatal("missing Connect mutation")
	}
	for _, p := range api.preconditions {
		if p.Expected.Revision != 7 || !bytes.Equal(p.Expected.SourceSha256, bytes.Repeat([]byte{0xaa}, 32)) || !bytes.Equal(p.ExpectedWorkspaceSha256, bytes.Repeat([]byte{0xbb}, 32)) {
			t.Fatal("CAS changed at CLI boundary")
		}
	}
	api.deny = true
	api.mu.Unlock()
	out := nha143Run(t, dir, terminal, ExitPermission, "--profile", "home", "accounts", "list")
	if !strings.Contains(out, "AUTH_ERROR_CODE_PERMISSION_DENIED") {
		t.Fatal("typed server error lost")
	}
}

func TestNHA143ChallengeAndCredentialFileRejection(t *testing.T) {
	dir := nha143Private(t)
	server, api, ca := nha143TLS(t, nha143Access)
	terminal := &nha143Terminal{}
	nha143Run(t, dir, terminal, ExitOK, "profiles", "add", "home", "--url", server.URL, "--ca", ca)
	api.mu.Lock()
	api.challenge = true
	api.mu.Unlock()
	nha143Run(t, dir, &nha143Terminal{values: []string{nha143Password}}, ExitChallenge, "--profile", "home", "login", "admin")
	if _, err := os.Stat(filepath.Join(dir, "home.credentials.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("challenge saved credential")
	}
	api.mu.Lock()
	api.challenge = false
	api.mu.Unlock()
	nha143Login(t, dir, "home")
	path := filepath.Join(dir, "home.credentials.json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(original)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	nha143Run(t, dir, terminal, ExitLocal, "--profile", "home", "accounts", "list")
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path+".real", path); err != nil {
		t.Fatal(err)
	}
	nha143Run(t, dir, terminal, ExitLocal, "--profile", "home", "accounts", "list")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	var c credentials
	if err := json.Unmarshal(original, &c); err != nil {
		t.Fatal(err)
	}
	c.Expires = time.Now().Add(-time.Minute)
	encoded, _ := json.Marshal(c)
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	clear(encoded)
	nha143Run(t, dir, terminal, ExitOK, "--profile", "home", "accounts", "list")
	api.mu.Lock()
	calls := append([]string(nil), api.calls...)
	api.mu.Unlock()
	if len(calls) < 2 || calls[len(calls)-2] != "refresh" || calls[len(calls)-1] != "accounts" {
		t.Fatal("expired access was not refreshed before business request")
	}
	linked := filepath.Join(nha143Private(t), "link")
	if err := os.Symlink(dir, linked); err != nil {
		t.Fatal(err)
	}
	nha143Run(t, linked, terminal, ExitLocal, "profiles", "list")
}

func TestNHA143ProfileInputAdmission(t *testing.T) {
	dir := nha143Private(t)
	terminal := &nha143Terminal{}
	for _, origin := range []string{"http://localhost", "https://user:password@host", "https://host/path", "https://host?x=1", "https://host#x", "https://host:0", "https://host:99999"} {
		nha143Run(t, dir, terminal, ExitUsage, "profiles", "add", "invalid", "--url", origin)
	}
	nha143Run(t, dir, terminal, ExitOK, "profiles", "add", "canonical", "--url", "https://EXAMPLE.com:443/")
	if out := nha143Run(t, dir, terminal, ExitOK, "profiles", "list"); !strings.Contains(out, "https://example.com") {
		t.Fatal("URL was not canonicalized")
	}
}

func nha143Certificate(t *testing.T) ([]byte, []byte) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
}

func (f *nha143API) activationResult() *cfg.ActivationResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mutationOverride {
		if f.activation == nil {
			return nil
		}
		return proto.Clone(f.activation).(*cfg.ActivationResult)
	}
	revision := int64(1)
	return &cfg.ActivationResult{Outcome: cfg.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED, Transaction: cfg.TransactionOutcome_TRANSACTION_OUTCOME_COMMITTED, GenerationRevision: &revision}
}

func TestNHA143RejectsPrivateKeysAndNeverPrintsPEM(t *testing.T) {
	dir := nha143Private(t)
	terminal := &nha143Terminal{}
	certificate, key := nha143Certificate(t)
	defer clear(key)
	bundle := append(append([]byte{}, certificate...), key...)
	defer clear(bundle)
	input := filepath.Join(dir, "bundle.pem")
	if err := os.WriteFile(input, bundle, 0600); err != nil {
		t.Fatal(err)
	}
	captured := nha143Run(t, dir, terminal, ExitUsage, "profiles", "add", "bad", "--url", "https://localhost", "--ca", input)
	if strings.Contains(captured, "ca_pem") || bytes.Contains([]byte(captured), key) {
		t.Fatal("PEM leaked")
	}
	if _, err := os.Stat(filepath.Join(dir, "bad.profile.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("private-key bundle persisted")
	}
	if err := os.WriteFile(input, certificate, 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{{"profiles", "add", "good", "--url", "https://localhost", "--ca", input}, {"profiles", "list"}} {
		output := nha143Run(t, dir, terminal, ExitOK, command...)
		var parsed map[string]any
		if err := json.Unmarshal([]byte(output), &parsed); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(parsed)
		if bytes.Contains(encoded, []byte("ca_pem")) || bytes.Contains(encoded, []byte("PRIVATE KEY")) {
			t.Fatal("public profile contains PEM field")
		}
	}
	stored, err := os.ReadFile(filepath.Join(dir, "good.profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted profile
	if err := json.Unmarshal(stored, &persisted); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(persisted.CA, certificate) {
		t.Fatal("certificate-only canonical persistence changed trust")
	}
	persisted.CA = bundle
	corrupt, _ := json.Marshal(persisted)
	defer clear(corrupt)
	if err := os.WriteFile(filepath.Join(dir, "good.profile.json"), corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	output := nha143Run(t, dir, terminal, ExitUsage, "profiles", "list")
	if strings.Contains(output, "ca_pem") || bytes.Contains([]byte(output), key) {
		t.Fatal("historic private-key bundle leaked")
	}
	nha143Run(t, dir, terminal, ExitUsage, "--profile", "good", "login", "admin")
	if terminal.calls != 0 {
		t.Fatal("invalid historic CA reached password prompt")
	}
}

func TestNHA143InputFilesRejectFIFOWithoutBlocking(t *testing.T) {
	dir := nha143Private(t)
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := readRegularInput(fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO admitted")
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO open blocked")
	}
	nha143Run(t, dir, &nha143Terminal{}, ExitUsage, "profiles", "add", "fifo", "--url", "https://localhost", "--ca", fifo)
	if _, err := readConfiguration(fifo); err == nil {
		t.Fatal("FIFO configuration admitted")
	}
}

func TestNHA143DefinitiveInvalidSessionsForgetAndNetworkFailuresRetain(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprint("expired-", expired), func(t *testing.T) {
			dir := nha143Private(t)
			server, api, ca := nha143TLS(t, nha143Access)
			terminal := &nha143Terminal{}
			nha143Run(t, dir, terminal, ExitOK, "profiles", "add", "home", "--url", server.URL, "--ca", ca)
			nha143Login(t, dir, "home")
			path := filepath.Join(dir, "home.credentials.json")
			if expired {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var c credentials
				if json.Unmarshal(data, &c) != nil {
					t.Fatal("credential JSON")
				}
				clear(data)
				c.Expires = time.Now().Add(-time.Minute)
				data, _ = json.Marshal(c)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				clear(data)
			}
			api.mu.Lock()
			api.revokeError = connect.CodeUnauthenticated
			api.refreshError = connect.CodeUnauthenticated
			api.mu.Unlock()
			output := nha143Run(t, dir, terminal, ExitAuthentication, "--profile", "home", "logout")
			if strings.Contains(output, `"revoked":true`) {
				t.Fatal("local forgetting claimed remote revocation")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("definitively invalid session retained")
			}
			api.mu.Lock()
			api.revokeError = 0
			api.refreshError = 0
			api.mu.Unlock()
			nha143Login(t, dir, "home")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer clear(before)
			api.mu.Lock()
			api.revokeError = connect.CodeUnavailable
			api.mu.Unlock()
			nha143Run(t, dir, terminal, ExitUnavailable, "--profile", "home", "logout")
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("ambiguous failure discarded credentials", err)
			}
			clear(after)
			var c credentials
			if err := json.Unmarshal(before, &c); err != nil {
				t.Fatal(err)
			}
			c.Expires = time.Now().Add(-time.Minute)
			expiredData, _ := json.Marshal(c)
			defer clear(expiredData)
			if err := os.WriteFile(path, expiredData, 0600); err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			api.refreshError = connect.CodeUnavailable
			api.mu.Unlock()
			nha143Run(t, dir, terminal, ExitUnavailable, "--profile", "home", "accounts", "list")
			after, err = os.ReadFile(path)
			if err != nil || !bytes.Equal(expiredData, after) {
				t.Fatal("ambiguous refresh failure discarded credentials", err)
			}
			clear(after)
			nha143Run(t, dir, terminal, ExitOK, "--profile", "home", "logout", "--forget-local")
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("explicit local forgetting failed")
			}
		})
	}
}

func TestNHA143MutationOutcomesAreTypedAndNeverRetried(t *testing.T) {
	dir := nha143Private(t)
	server, api, ca := nha143TLS(t, nha143Access)
	terminal := &nha143Terminal{}
	nha143Run(t, dir, terminal, ExitOK, "profiles", "add", "home", "--url", server.URL, "--ca", ca)
	nha143Login(t, dir, "home")
	revision := int64(2)
	cases := []struct {
		name       string
		activation *cfg.ActivationResult
		exit       int
	}{
		{"published", &cfg.ActivationResult{Outcome: cfg.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED, Transaction: cfg.TransactionOutcome_TRANSACTION_OUTCOME_COMMITTED, GenerationRevision: &revision}, ExitOK},
		{"published-not-committed", &cfg.ActivationResult{Outcome: cfg.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED, Transaction: cfg.TransactionOutcome_TRANSACTION_OUTCOME_NOT_COMMITTED, GenerationRevision: &revision}, ExitMutationFailed},
		{"published-unknown", &cfg.ActivationResult{Outcome: cfg.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED, Transaction: cfg.TransactionOutcome_TRANSACTION_OUTCOME_COMMIT_UNKNOWN, GenerationRevision: &revision}, ExitMutationUnknown},
		{"rejected", &cfg.ActivationResult{Outcome: cfg.ActivationOutcome_ACTIVATION_OUTCOME_REJECTED_BEFORE_COMMIT, Transaction: cfg.TransactionOutcome_TRANSACTION_OUTCOME_NO_TRANSACTION}, ExitMutationFailed},
		{"not-committed", &cfg.ActivationResult{Outcome: cfg.ActivationOutcome_ACTIVATION_OUTCOME_NOT_COMMITTED, Transaction: cfg.TransactionOutcome_TRANSACTION_OUTCOME_NOT_COMMITTED}, ExitMutationFailed},
		{"unknown", &cfg.ActivationResult{Outcome: cfg.ActivationOutcome_ACTIVATION_OUTCOME_COMMIT_UNKNOWN, Transaction: cfg.TransactionOutcome_TRANSACTION_OUTCOME_COMMIT_UNKNOWN}, ExitMutationUnknown},
		{"reconciling", &cfg.ActivationResult{Outcome: cfg.ActivationOutcome_ACTIVATION_OUTCOME_COMMITTED_RECONCILING, Transaction: cfg.TransactionOutcome_TRANSACTION_OUTCOME_COMMITTED, GenerationRevision: &revision}, ExitMutationPending},
		{"restart", &cfg.ActivationResult{Outcome: cfg.ActivationOutcome_ACTIVATION_OUTCOME_RESTART_REQUIRED, Transaction: cfg.TransactionOutcome_TRANSACTION_OUTCOME_NO_TRANSACTION}, ExitMutationPending},
		{"nil", nil, ExitLocal}, {"zero", &cfg.ActivationResult{}, ExitLocal}, {"future", &cfg.ActivationResult{Outcome: cfg.ActivationOutcome(999), Transaction: cfg.TransactionOutcome_TRANSACTION_OUTCOME_COMMITTED}, ExitLocal},
		{"unknown-transaction", &cfg.ActivationResult{Outcome: cfg.ActivationOutcome_ACTIVATION_OUTCOME_NOT_COMMITTED, Transaction: cfg.TransactionOutcome(999)}, ExitLocal},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			api.mu.Lock()
			api.mutationOverride = true
			api.activation = test.activation
			before := len(api.calls)
			api.mu.Unlock()
			output := nha143Run(t, dir, terminal, test.exit, "--profile", "home", "integrations", "delete", "isolated", "--revision", "7", "--source-sha256", strings.Repeat("a", 64), "--workspace-sha256", strings.Repeat("b", 64))
			api.mu.Lock()
			calls := len(api.calls) - before
			api.mu.Unlock()
			if calls != 1 {
				t.Fatal("mutation implicitly retried")
			}
			if test.exit != ExitLocal && !strings.Contains(output, `"activation"`) {
				t.Fatal("typed mutation state lost")
			}
			if test.exit == ExitMutationUnknown && !strings.Contains(output, "mutation_commit_unknown_do_not_retry") {
				t.Fatal("commit ambiguity lost its do-not-retry signal")
			}
			if test.name == "published-not-committed" || test.name == "published-unknown" {
				if !strings.Contains(output, test.activation.Outcome.String()) || !strings.Contains(output, test.activation.Transaction.String()) {
					t.Fatal("publication and requesting transaction axes not preserved")
				}
			}
		})
	}
}
