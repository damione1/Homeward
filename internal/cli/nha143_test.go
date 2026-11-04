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

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"golang.org/x/sys/unix"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const nha143Password = "NHA143 synthetic password should never appear in captured output"
const nha143Access = "NHA143-opaque-access-secret"
const nha143Refresh = "NHA143-opaque-refresh-secret"
const nha143Invitation = "NHA143-opaque-invitation-secret"
const nha143Home = "homes/home-1"
const nha143Type = "homes/home-1/integrationTypes/t-bqxhi5ba"
const nha143Instance = "homes/home-1/integrationInstances/i-isolated"
const nha143Document = "homes/home-1/configurationDocuments/d-operational"
const nha143Operation = "homes/home-1/operations/config-0123456789abcdef0123456789abcdef"

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
	for _, command := range [][]string{{}, {"bogus"}, {"login"}, {"login", "admin", "--password", nha143Password}, {"login", "admin", nha143Password}, {"accounts", "create", "u", "--display-name", "u", "--role", "root"}, {"integrations", "delete", nha143Instance}} {
		nha143Run(t, dir, terminal, ExitUsage, command...)
	}
	if terminal.calls != 0 {
		t.Fatal("parser touched terminal")
	}
}

type nha143MutationRecord struct {
	etag      string
	requestID string
	paths     []string
}

type nha143API struct {
	apiv2connect.UnimplementedAuthServiceHandler
	apiv2connect.UnimplementedAccountServiceHandler
	apiv2connect.UnimplementedIntegrationServiceHandler
	mu               sync.Mutex
	calls            []string
	challenge        bool
	deny             bool
	token            string
	mutations        []nha143MutationRecord
	refreshError     connect.Code
	renewalKeys      [][]byte
	credentialPath   string
	revokeError      connect.Code
	operation        *longrunningpb.Operation
	operationErr     error
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
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED).Connect()
	}

	return nil
}
func (f *nha143API) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }
func nha143Tokens(token string) *api.SessionTokens {
	now := time.Now()
	return &api.SessionTokens{Session: &api.Session{Name: nha143Home + "/accounts/account-1/sessions/session-1",
		CreateTime: timestamppb.New(now.Add(-time.Minute)), ExpireTime: timestamppb.New(now.Add(7 * 24 * time.Hour)),
		AuthenticationSatisfaction: api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_NOT_RECORDED},
		Audience: api.CredentialAudience_CREDENTIAL_AUDIENCE_INTERACTIVE, AccessToken: token, RefreshToken: nha143Refresh,
		AccessExpireTime: timestamppb.New(now.Add(14 * time.Minute))}

}
func (f *nha143API) StartLogin(_ context.Context, r *connect.Request[api.StartLoginRequest]) (*connect.Response[api.StartLoginResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "login")
	if r.Msg.Password != nha143Password {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New(nha143Password))
	}
	decision := &api.LoginDecision{Outcome: &api.LoginDecision_Authenticated{Authenticated: nha143Tokens(f.token)}}
	if f.challenge {
		decision.Outcome = &api.LoginDecision_Challenge{Challenge: &api.FactorChallenge{ChallengeId: "factor-challenge"}}
	}
	return connect.NewResponse(&api.StartLoginResponse{Decision: decision}), nil
}
func (f *nha143API) RefreshSession(_ context.Context, r *connect.Request[api.RefreshSessionRequest]) (*connect.Response[api.RefreshSessionResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "refresh")
	if len(r.Msg.RenewalKey) != 32 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("renewal key required"))
	}
	f.renewalKeys = append(f.renewalKeys, append([]byte(nil), r.Msg.RenewalKey...))
	if f.credentialPath != "" {
		raw, err := os.ReadFile(f.credentialPath)
		var saved credentials
		if err != nil || json.Unmarshal(raw, &saved) != nil || !bytes.Equal(saved.RenewalKey, r.Msg.RenewalKey) {
			return nil, connect.NewError(connect.CodeInternal, errors.New("dispatch preceded durable key"))
		}
	}
	if f.refreshError == connect.CodeUnauthenticated {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_AUTHENTICATION_REQUIRED).Connect()
	}
	if f.refreshError != 0 {
		return nil, connect.NewError(f.refreshError, errors.New(nha143Refresh))
	}
	if r.Msg.RefreshToken != nha143Refresh {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New(nha143Refresh))
	}
	return connect.NewResponse(&api.RefreshSessionResponse{Tokens: nha143Tokens(f.token)}), nil
}
func (f *nha143API) ListSessions(_ context.Context, r *connect.Request[api.ListSessionsRequest]) (*connect.Response[api.ListSessionsResponse], error) {
	if err := f.record("sessions", r.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&api.ListSessionsResponse{Sessions: []*api.Session{nha143Tokens(f.token).Session}}), nil
}
func (f *nha143API) RevokeSession(_ context.Context, r *connect.Request[api.RevokeSessionRequest]) (*connect.Response[api.RevokeSessionResponse], error) {
	if err := f.record("revoke", r.Header()); err != nil {
		return nil, err
	}
	f.mu.Lock()
	code := f.revokeError
	f.mu.Unlock()
	if code != 0 {
		return nil, connect.NewError(code, errors.New(nha143Access))
	}
	return connect.NewResponse(&api.RevokeSessionResponse{}), nil
}
func (f *nha143API) ListAccounts(_ context.Context, r *connect.Request[api.ListAccountsRequest]) (*connect.Response[api.ListAccountsResponse], error) {
	if err := f.record("accounts", r.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&api.ListAccountsResponse{}), nil
}
func (f *nha143API) GetAccount(_ context.Context, r *connect.Request[api.GetAccountRequest]) (*connect.Response[api.Account], error) {
	if err := f.record("account", r.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&api.Account{Name: r.Msg.Name}), nil
}
func (f *nha143API) CreateAccount(_ context.Context, r *connect.Request[api.CreateAccountRequest]) (*connect.Response[api.Account], error) {
	if err := f.record("create-account", r.Header()); err != nil {
		return nil, err
	}
	if r.Msg.GetAccount().GetInitialPassword() != nha143Password {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("password"))
	}
	return connect.NewResponse(&api.Account{Name: nha143Home + "/accounts/created", Role: r.Msg.GetAccount().GetRole()}), nil
}
func (f *nha143API) IssueInvitation(_ context.Context, r *connect.Request[api.IssueInvitationRequest]) (*connect.Response[api.IssueInvitationResponse], error) {
	if err := f.record("invite", r.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&api.IssueInvitationResponse{Invitation: &api.Invitation{Name: nha143Home + "/invitations/invitation-1"}, InvitationToken: nha143Invitation}), nil
}
func (f *nha143API) ListIntegrationTypes(_ context.Context, r *connect.Request[api.ListIntegrationTypesRequest]) (*connect.Response[api.ListIntegrationTypesResponse], error) {
	if err := f.record("catalogue", r.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&api.ListIntegrationTypesResponse{}), nil
}
func (f *nha143API) GetIntegrationType(_ context.Context, r *connect.Request[api.GetIntegrationTypeRequest]) (*connect.Response[api.IntegrationType], error) {
	if err := f.record("schema", r.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&api.IntegrationType{Name: r.Msg.Name, TypeKey: "mqtt"}), nil
}
func (f *nha143API) ListIntegrationInstances(_ context.Context, r *connect.Request[api.ListIntegrationInstancesRequest]) (*connect.Response[api.ListIntegrationInstancesResponse], error) {
	if err := f.record("instances", r.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&api.ListIntegrationInstancesResponse{}), nil
}
func (f *nha143API) GetIntegrationInstance(_ context.Context, r *connect.Request[api.GetIntegrationInstanceRequest]) (*connect.Response[api.IntegrationInstance], error) {
	if err := f.record("instance", r.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&api.IntegrationInstance{
		Name:                  r.Msg.Name,
		InstanceKey:           "isolated",
		IntegrationType:       nha143Type,
		DesiredState:          api.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED,
		Etag:                  "W/\"" + strings.Repeat("a", 64) + "\"",
		ConfigurationDocument: nha143Document,
		Configuration: &api.IntegrationInstance_Editable{
			Editable: &api.IntegrationConfiguration{
				SchemaVersion: "1",
				YamlSource:    []byte("{}"),
			},
		},
		ObservedHealth:      &api.IntegrationInstance_NoHealthReport{NoHealthReport: &emptypb.Empty{}},
		ObservedSupervision: &api.IntegrationInstance_NotSupervised{NotSupervised: &emptypb.Empty{}},
	}), nil
}
func (f *nha143API) mutate(name string, header http.Header, rec nha143MutationRecord) error {
	if err := f.record(name, header); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mutations = append(f.mutations, rec)
	return nil
}
func (f *nha143API) CreateIntegrationInstance(_ context.Context, r *connect.Request[api.CreateIntegrationInstanceRequest]) (*connect.Response[longrunningpb.Operation], error) {
	if err := f.mutate("create-instance", r.Header(), nha143MutationRecord{requestID: r.Msg.GetRequestId()}); err != nil {
		return nil, err
	}
	return f.operationResponse(api.IntegrationMutationKind_INTEGRATION_MUTATION_KIND_CREATE)
}
func (f *nha143API) UpdateIntegrationInstance(_ context.Context, r *connect.Request[api.UpdateIntegrationInstanceRequest]) (*connect.Response[longrunningpb.Operation], error) {
	if err := f.mutate("update-instance", r.Header(), nha143MutationRecord{etag: r.Msg.GetIntegrationInstance().GetEtag(), requestID: r.Msg.GetRequestId(), paths: append([]string(nil), r.Msg.GetUpdateMask().GetPaths()...)}); err != nil {
		return nil, err
	}
	return f.operationResponse(api.IntegrationMutationKind_INTEGRATION_MUTATION_KIND_UPDATE)
}
func (f *nha143API) DeleteIntegrationInstance(_ context.Context, r *connect.Request[api.DeleteIntegrationInstanceRequest]) (*connect.Response[longrunningpb.Operation], error) {
	if err := f.mutate("delete-instance", r.Header(), nha143MutationRecord{etag: r.Msg.GetEtag(), requestID: r.Msg.GetRequestId()}); err != nil {
		return nil, err
	}
	return f.operationResponse(api.IntegrationMutationKind_INTEGRATION_MUTATION_KIND_DELETE)
}
func nha143TLS(t *testing.T, token string) (*httptest.Server, *nha143API, string) {
	t.Helper()
	apiHandler := &nha143API{token: token}
	mux := http.NewServeMux()
	mux.Handle(apiv2connect.NewAuthServiceHandler(apiHandler))
	mux.Handle(apiv2connect.NewAccountServiceHandler(apiHandler))
	mux.Handle(apiv2connect.NewIntegrationServiceHandler(apiHandler))
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
	return server, apiHandler, ca
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
	server, apiHandler, ca := nha143TLS(t, nha143Access)
	terminal := &nha143Terminal{}
	nha143Run(t, dir, terminal, ExitOK, "profiles", "add", "home", "--url", server.URL, "--ca", ca)
	nha143Login(t, dir, "home")
	for _, command := range [][]string{
		{"accounts", "list"},
		{"accounts", "get", nha143Home + "/accounts/account-1"},
		{"sessions", "list", nha143Home + "/accounts/account-1"},
		{"sessions", "revoke", nha143Home + "/accounts/account-1/sessions/other-session"},
		{"integrations", "catalogue", nha143Home},
		{"integrations", "schema", nha143Type},
		{"integrations", "list", nha143Home},
		{"integrations", "get", nha143Instance},
		{"integrations", "health", nha143Instance},
	} {
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
	etag := "W/\"" + strings.Repeat("a", 64) + "\""
	nha143Run(t, dir, terminal, ExitOK, "--profile", "home", "integrations", "create", nha143Type, "isolated", "--config", config, "--request-id", "11111111-2222-4333-8444-555555555555")
	for i, command := range [][]string{
		{"integrations", "update", nha143Instance, "--config", config},
		{"integrations", "delete", nha143Instance},
		{"integrations", "enable", nha143Instance},
		{"integrations", "disable", nha143Instance},
	} {
		reqID := fmt.Sprintf("%d2222222-2222-4333-8444-555555555555", i+2)
		argv := append([]string{"--profile", "home"}, command...)
		argv = append(argv, "--etag", etag, "--request-id", reqID)
		nha143Run(t, dir, terminal, ExitOK, argv...)
	}
	apiHandler.mu.Lock()
	if len(apiHandler.mutations) != 5 {
		t.Fatal("missing Connect mutation")
	}
	if apiHandler.mutations[0].requestID != "11111111-2222-4333-8444-555555555555" {
		t.Fatal("create request-id lost at CLI boundary")
	}
	for i, m := range apiHandler.mutations[1:] {
		wantID := fmt.Sprintf("%d2222222-2222-4333-8444-555555555555", i+2)
		if m.etag != etag || m.requestID != wantID {
			t.Fatal("CAS etag or request-id changed at CLI boundary")
		}
	}
	apiHandler.deny = true
	apiHandler.mu.Unlock()
	out := nha143Run(t, dir, terminal, ExitPermission, "--profile", "home", "accounts", "list")
	if !strings.Contains(out, "API_ERROR_REASON_ACCESS_DENIED") {
		t.Fatal("typed server error lost")
	}
}

func TestNHA143ChallengeAndCredentialFileRejection(t *testing.T) {
	dir := nha143Private(t)
	server, apiHandler, ca := nha143TLS(t, nha143Access)
	terminal := &nha143Terminal{}
	nha143Run(t, dir, terminal, ExitOK, "profiles", "add", "home", "--url", server.URL, "--ca", ca)
	apiHandler.mu.Lock()
	apiHandler.challenge = true
	apiHandler.mu.Unlock()
	nha143Run(t, dir, &nha143Terminal{values: []string{nha143Password}}, ExitChallenge, "--profile", "home", "login", "admin")
	if _, err := os.Stat(filepath.Join(dir, "home.credentials.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("challenge saved credential")
	}
	apiHandler.mu.Lock()
	apiHandler.challenge = false
	apiHandler.mu.Unlock()
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
	apiHandler.mu.Lock()
	calls := append([]string(nil), apiHandler.calls...)
	apiHandler.mu.Unlock()
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

func nha143BuildOperation(kind api.IntegrationMutationKind, phase api.IntegrationOperationPhase, done bool, response proto.Message, opStatus *statuspb.Status) *longrunningpb.Operation {
	now := timestamppb.New(time.Unix(1_700_000_000, 0).UTC())
	meta := &api.IntegrationOperationMetadata{
		Kind:       kind,
		Target:     nha143Instance,
		CreateTime: now,
		Phase:      phase,
	}
	switch phase {
	case api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_COMMITTED, api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_PUBLISHED:
		meta.CommitTime = now
		meta.Generation = nha143Home + "/configurationGenerations/g-2"
		meta.GenerationRevision = 2
		meta.ManifestSha256 = bytes.Repeat([]byte{0x44}, 32)
	case api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_NO_CHANGE:
		meta.ValidationTime = now
	}
	packedMeta, err := anypb.New(meta)
	if err != nil {
		panic(err)
	}
	op := &longrunningpb.Operation{
		Name:     nha143Operation,
		Metadata: packedMeta,
		Done:     done,
	}
	if opStatus != nil {
		op.Result = &longrunningpb.Operation_Error{Error: opStatus}
	} else if response != nil {
		packedResp, err := anypb.New(response)
		if err != nil {
			panic(err)
		}
		op.Result = &longrunningpb.Operation_Response{Response: packedResp}
	}
	return op
}

func (f *nha143API) operationResponse(kind api.IntegrationMutationKind) (*connect.Response[longrunningpb.Operation], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mutationOverride {
		if f.operationErr != nil {
			return nil, f.operationErr
		}
		if f.operation == nil {
			return connect.NewResponse((*longrunningpb.Operation)(nil)), nil
		}
		return connect.NewResponse(proto.Clone(f.operation).(*longrunningpb.Operation)), nil
	}
	var resp proto.Message = &api.IntegrationInstance{
		Name:                  nha143Instance,
		InstanceKey:           "isolated",
		IntegrationType:       nha143Type,
		DesiredState:          api.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED,
		Etag:                  "W/\"" + strings.Repeat("b", 64) + "\"",
		ConfigurationDocument: nha143Document,
		Configuration: &api.IntegrationInstance_Editable{
			Editable: &api.IntegrationConfiguration{
				SchemaVersion: "1",
				YamlSource:    []byte("{}"),
			},
		},
	}
	if kind == api.IntegrationMutationKind_INTEGRATION_MUTATION_KIND_DELETE {
		resp = &emptypb.Empty{}
	}
	return connect.NewResponse(nha143BuildOperation(kind, api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_PUBLISHED, true, resp, nil)), nil
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
			server, apiHandler, ca := nha143TLS(t, nha143Access)
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
			apiHandler.mu.Lock()
			apiHandler.revokeError = connect.CodeUnauthenticated
			apiHandler.refreshError = connect.CodeUnauthenticated
			apiHandler.mu.Unlock()
			output := nha143Run(t, dir, terminal, ExitAuthentication, "--profile", "home", "logout")
			if strings.Contains(output, `"revoked":true`) {
				t.Fatal("local forgetting claimed remote revocation")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("definitively invalid session retained")
			}
			apiHandler.mu.Lock()
			apiHandler.revokeError = 0
			apiHandler.refreshError = 0
			apiHandler.mu.Unlock()
			nha143Login(t, dir, "home")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer clear(before)
			apiHandler.mu.Lock()
			apiHandler.revokeError = connect.CodeUnavailable
			apiHandler.mu.Unlock()
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
			apiHandler.mu.Lock()
			apiHandler.refreshError = connect.CodeUnavailable
			apiHandler.mu.Unlock()
			nha143Run(t, dir, terminal, ExitUnavailable, "--profile", "home", "accounts", "list")
			after, err = os.ReadFile(path)
			var retained credentials
			if err != nil || json.Unmarshal(after, &retained) != nil || retained.Access != c.Access || retained.Refresh != c.Refresh || retained.Account != c.Account || retained.Session != c.Session || len(retained.RenewalKey) != 32 {
				t.Fatal("ambiguous refresh failure discarded credentials or durable operation", err)
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
	server, apiHandler, ca := nha143TLS(t, nha143Access)
	terminal := &nha143Terminal{}
	nha143Run(t, dir, terminal, ExitOK, "profiles", "add", "home", "--url", server.URL, "--ca", ca)
	nha143Login(t, dir, "home")
	etag := "W/\"" + strings.Repeat("a", 64) + "\""
	failedStatus := apierrors.New(api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED).Status()
	opName, err := apitypes.ParseOperationName(nha143Operation)
	if err != nil {
		t.Fatal(err)
	}
	unknownFault := apierrors.New(api.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN).WithOperation(opName).Connect()
	cases := []struct {
		name      string
		operation *longrunningpb.Operation
		opErr     error
		exit      int
	}{
		{"published", nha143BuildOperation(api.IntegrationMutationKind_INTEGRATION_MUTATION_KIND_DELETE, api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_PUBLISHED, true, &emptypb.Empty{}, nil), nil, ExitOK},
		{"accepted", nha143BuildOperation(api.IntegrationMutationKind_INTEGRATION_MUTATION_KIND_DELETE, api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_ACCEPTED, false, nil, nil), nil, ExitMutationPending},
		{"committed", nha143BuildOperation(api.IntegrationMutationKind_INTEGRATION_MUTATION_KIND_DELETE, api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_COMMITTED, false, nil, nil), nil, ExitMutationPending},
		{"failed", nha143BuildOperation(api.IntegrationMutationKind_INTEGRATION_MUTATION_KIND_DELETE, api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_FAILED, true, nil, failedStatus), nil, ExitMutationFailed},
		{"unknown", nil, unknownFault, ExitMutationUnknown},
		{"nil", nil, nil, ExitLocal},
		{"zero", &longrunningpb.Operation{}, nil, ExitLocal},
		{"future-phase", nha143BuildOperation(api.IntegrationMutationKind_INTEGRATION_MUTATION_KIND_DELETE, api.IntegrationOperationPhase(999), true, &emptypb.Empty{}, nil), nil, ExitLocal},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			apiHandler.mu.Lock()
			apiHandler.mutationOverride = true
			apiHandler.operation = test.operation
			apiHandler.operationErr = test.opErr
			before := len(apiHandler.calls)
			apiHandler.mu.Unlock()
			output := nha143Run(t, dir, terminal, test.exit, "--profile", "home", "integrations", "delete", nha143Instance, "--etag", etag)
			apiHandler.mu.Lock()
			calls := len(apiHandler.calls) - before
			apiHandler.mu.Unlock()
			if calls != 1 {
				t.Fatal("mutation implicitly retried")
			}
			if test.exit == ExitMutationUnknown {
				if !strings.Contains(output, "mutation_commit_unknown_do_not_retry") || !strings.Contains(output, nha143Operation) {
					t.Fatal("commit ambiguity lost its do-not-retry signal or operation name")
				}
			} else if test.exit != ExitLocal && !strings.Contains(output, nha143Operation) {
				t.Fatal("typed operation receipt lost")
			}
		})
	}
}

func TestNHA203CLIRenewalSavesAndReusesExactKeyAfterLostReply(t *testing.T) {
	dir := nha143Private(t)
	server, apiHandler, ca := nha143TLS(t, nha143Access)
	terminal := &nha143Terminal{}
	nha143Run(t, dir, terminal, ExitOK, "profiles", "add", "renewal", "--url", server.URL, "--ca", ca)
	nha143Login(t, dir, "renewal")
	path := filepath.Join(dir, "renewal.credentials.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved credentials
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	saved.Expires = time.Now().Add(-time.Minute)
	data, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	apiHandler.mu.Lock()
	apiHandler.credentialPath = path
	apiHandler.refreshError = connect.CodeUnavailable
	apiHandler.mu.Unlock()
	nha143Run(t, dir, terminal, ExitUnavailable, "--profile", "renewal", "accounts", "list")
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(raw, &saved) != nil || len(saved.RenewalKey) != 32 {
		t.Fatal("uncertain renewal lost saved operation")
	}
	pending := append([]byte(nil), saved.RenewalKey...)
	apiHandler.mu.Lock()
	apiHandler.refreshError = 0
	apiHandler.mu.Unlock()
	nha143Run(t, dir, terminal, ExitOK, "--profile", "renewal", "accounts", "list")
	apiHandler.mu.Lock()
	keys := apiHandler.renewalKeys
	apiHandler.mu.Unlock()
	if len(keys) != 2 || !bytes.Equal(keys[0], pending) || !bytes.Equal(keys[1], pending) {
		t.Fatal("retry changed durable renewal key")
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	saved = credentials{} // An omitted JSON key must not retain the previous decoded value.
	if json.Unmarshal(raw, &saved) != nil || len(saved.RenewalKey) != 0 {
		t.Fatal("successful renewal retained pending operation")
	}
}
