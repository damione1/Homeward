package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestNHA145RealTLSKeysIsolationRevocationRightsAndExpiry(t *testing.T) {
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	settings := nha049Config(t, cluster.Port, data)
	clock := newNHA141Clock(time.Now().UTC().Truncate(time.Microsecond))
	canceled := make(chan string, 16)
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var store *storage.Store
	go func() {
		done <- RunWithStartupFactory(ctx, settings.startup, (&net.ListenConfig{}).Listen, func(address net.Addr) { ready <- address }, func(_ context.Context, s *storage.Store) (RuntimePlan, error) {
			store = s
			plan, err := NewPublicPlan(nil, nil, accountTransportAudit{store: s})
			if err != nil {
				return RuntimePlan{}, err
			}
			plan.sessionClock = clock
			permission, err := authorization.NewPermission(authv1.ScopeFamily_SCOPE_FAMILY_ACCOUNTS, authv1.ScopeOperation_SCOPE_OPERATION_READ)
			if err != nil {
				return RuntimePlan{}, err
			}
			for _, path := range []string{"/nha145/read", "/nha145/stream"} {
				flow := RouteFlowUnary
				if strings.HasSuffix(path, "stream") {
					flow = RouteFlowServerStream
				}
				if err := plan.Register(RouteKindMCP, RouteMethodGet, path, RouteAccessProtected, flow, RouteTransportTLSRequired, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
					if strings.HasSuffix(r.URL.Path, "stream") {
						w.(http.Flusher).Flush()
						<-r.Context().Done()
						canceled <- r.Header.Get("X-Fixture-Key")
					} else {
						_, _ = io.WriteString(w, "fixture-only")
					}
				})); err != nil {
					return RuntimePlan{}, err
				}
				if err := plan.BindPermission(RouteKindMCP, RouteMethodGet, path, permission); err != nil {
					return RuntimePlan{}, err
				}
			}
			return RuntimePlan{Identity: unconfiguredIdentity{}, Public: plan}, nil
		})
	}()
	var address net.Addr
	select {
	case address = <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(15 * time.Second):
		t.Fatal("Run readiness timeout")
	}
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Run shutdown timeout")
		}
	}()
	operatorHTTP := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(data, operatorSocketName))
	}}}
	defer operatorHTTP.CloseIdleConnections()
	operator := authv1connect.NewOperatorAccountServiceClient(operatorHTTP, "http://operator")
	const password = "NHA145 synthetic test password never used on devices"
	admin, err := operator.BootstrapAdmin(ctx, connect.NewRequest(&authv1.BootstrapAdminRequest{LoginName: "NHA145 Admin", DisplayName: "Admin", Password: password}))
	if err != nil {
		t.Fatal(err)
	}
	client := nha181Client(roots)
	defer client.CloseIdleConnections()
	base := "https://" + address.String()
	auth := authv1connect.NewAuthServiceClient(client, base)
	keys := authv1connect.NewMcpKeyServiceClient(client, base)
	accounts := authv1connect.NewAccountServiceClient(client, base)
	login := func(name string) *authv1.SessionTokens {
		t.Helper()
		r, e := auth.StartLogin(ctx, connect.NewRequest(&authv1.StartLoginRequest{LoginName: name, Password: password}))
		if e != nil || r.Msg.Decision.GetAuthenticated() == nil {
			t.Fatalf("login: %v", e)
		}
		return r.Msg.Decision.GetAuthenticated()
	}
	tokens := login("NHA145 Admin")
	scopes := []*authv1.Scope{{Family: authv1.ScopeFamily_SCOPE_FAMILY_ACCOUNTS, Operation: authv1.ScopeOperation_SCOPE_OPERATION_READ}}
	create := func(name string, lifetime *durationpb.Duration) *authv1.CreateMcpKeyResponse {
		t.Helper()
		r, e := keys.CreateMcpKey(ctx, nha141BearerRequest(&authv1.CreateMcpKeyRequest{ClientName: name, Scopes: scopes, Lifetime: lifetime}, tokens.AccessToken))
		if e != nil {
			t.Fatal(e)
		}
		return r.Msg
	}
	first := create("assistant A", nil)
	second := create("assistant B", durationpb.New(365*24*time.Hour))
	if first.Secret == second.Secret || first.Key.Id.Value == second.Key.Id.Value || first.Key.Audience != authv1.CredentialAudience_CREDENTIAL_AUDIENCE_MCP || first.Key.ExpiresAt.AsTime().Sub(clock.Now()) != 30*24*time.Hour || second.Key.ExpiresAt.AsTime().Sub(clock.Now()) != 365*24*time.Hour {
		t.Fatal("key identity/audience/lifetime")
	}
	for _, lifetime := range []time.Duration{0, -time.Second, 365*24*time.Hour + time.Nanosecond} {
		_, e := keys.CreateMcpKey(ctx, nha141BearerRequest(&authv1.CreateMcpKeyRequest{ClientName: "invalid", Scopes: scopes, Lifetime: durationpb.New(lifetime)}, tokens.AccessToken))
		nha049Code(t, e, connect.CodeInvalidArgument)
		var rpc *connect.Error
		if !errors.As(e, &rpc) || len(rpc.Details()) == 0 {
			t.Fatal("missing structured lifetime error")
		}
	}
	list, err := keys.ListMcpKeys(ctx, nha141BearerRequest(&authv1.ListMcpKeysRequest{}, tokens.AccessToken))
	if err != nil || len(list.Msg.Keys) != 2 {
		t.Fatalf("list: %v", err)
	}
	encoded, _ := json.Marshal(list.Msg)
	if strings.Contains(string(encoded), first.Secret) || strings.Contains(string(encoded), second.Secret) || strings.Contains(string(encoded), "secret") {
		t.Fatal("secret returned after issuance")
	}
	err = store.InTransaction(ctx, func(tx pgx.Tx) error {
		var data string
		if e := tx.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(k))::text,'') FROM public.auth_mcp_keys k`).Scan(&data); e != nil {
			return e
		}
		if strings.Contains(data, first.Secret) || strings.Contains(data, strings.TrimPrefix(first.Secret, "hwmcp1_")) {
			t.Fatal("raw credential persisted")
		}
		if e := tx.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(a))::text,'') FROM public.auth_audit a`).Scan(&data); e != nil {
			return e
		}
		if strings.Contains(data, first.Secret) {
			t.Fatal("secret in audit")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	request := func(path, secret, label string) *http.Response {
		t.Helper()
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if e != nil {
			t.Fatal(e)
		}
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("X-Fixture-Key", label)
		r, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	status := func(secret string) int {
		r := request("/nha145/read", secret, "")
		defer r.Body.Close()
		return r.StatusCode
	}
	if status(first.Secret) != http.StatusOK || status(tokens.AccessToken) == http.StatusOK {
		t.Fatal("MCP family admission")
	}
	_, err = auth.ListSessions(ctx, nha141BearerRequest(&authv1.ListSessionsRequest{AccountId: admin.Msg.Account.Id}, first.Secret))
	nha049Code(t, err, connect.CodeUnauthenticated)
	_, err = keys.CreateMcpKey(ctx, nha141BearerRequest(&authv1.CreateMcpKeyRequest{ClientName: "nested"}, first.Secret))
	nha049Code(t, err, connect.CodeUnauthenticated)
	streamA := request("/nha145/stream", first.Secret, "A")
	defer streamA.Body.Close()
	streamB := request("/nha145/stream", second.Secret, "B")
	defer streamB.Body.Close()
	if streamA.StatusCode != 200 || streamB.StatusCode != 200 {
		t.Fatal("stream admission")
	}
	_, err = keys.RevokeMcpKey(ctx, nha141BearerRequest(&authv1.RevokeMcpKeyRequest{KeyId: first.Key.Id}, tokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case label := <-canceled:
		if label != "A" {
			t.Fatalf("wrong key canceled %s", label)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revoked key stream remained open")
	}
	if status(first.Secret) == http.StatusOK || status(second.Secret) != http.StatusOK {
		t.Fatal("revocation affected independent key")
	}
	// Password logout is independent of a deliberately issued MCP key.
	_, err = auth.RevokeSession(ctx, nha141BearerRequest(&authv1.RevokeSessionRequest{SessionId: tokens.Session.Id}, tokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	if status(second.Secret) != 200 {
		t.Fatal("interactive logout revoked MCP credential")
	}
	tokens = login("NHA145 Admin")
	// Add another administrator so demotion remains legal for a one-house server.
	_, err = accounts.CreateAccount(ctx, nha141BearerRequest(&authv1.CreateAccountRequest{LoginName: "NHA145 other", DisplayName: "Other", InitialPassword: password, Role: authv1.AccountRole_ACCOUNT_ROLE_ADMIN}, tokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	userRole := authv1.AccountRole_ACCOUNT_ROLE_USER
	_, err = accounts.UpdateAccount(ctx, nha141BearerRequest(&authv1.UpdateAccountRequest{AccountId: admin.Msg.Account.Id, Role: &userRole}, tokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case label := <-canceled:
		if label != "B" {
			t.Fatalf("wrong rights-canceled key %s", label)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("rights loss left MCP stream open")
	}
	if status(second.Secret) == 200 {
		t.Fatal("demoted account still admitted key")
	}
	other := login("NHA145 other")
	adminRole := authv1.AccountRole_ACCOUNT_ROLE_ADMIN
	_, err = accounts.UpdateAccount(ctx, nha141BearerRequest(&authv1.UpdateAccountRequest{AccountId: admin.Msg.Account.Id, Role: &adminRole}, other.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	if status(second.Secret) == 200 {
		t.Fatal("regrant revived preexisting credential")
	}
	tokens = login("NHA145 Admin")
	// Scoped administration is refused for a normal user at issuance; a later
	// promotion cannot enlarge even an empty credential's ceiling.
	user, err := accounts.CreateAccount(ctx, nha141BearerRequest(&authv1.CreateAccountRequest{LoginName: "NHA145 user", DisplayName: "User", InitialPassword: password, Role: userRole}, tokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	userTokens := login("NHA145 user")
	_, err = keys.CreateMcpKey(ctx, nha141BearerRequest(&authv1.CreateMcpKeyRequest{ClientName: "forbidden", Scopes: scopes}, userTokens.AccessToken))
	nha049Code(t, err, connect.CodePermissionDenied)
	empty, err := keys.CreateMcpKey(ctx, nha141BearerRequest(&authv1.CreateMcpKeyRequest{ClientName: "empty ceiling"}, userTokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	if status(empty.Msg.Secret) == 200 {
		t.Fatal("empty ceiling allowed administrative route")
	}
	_, err = keys.ListMcpKeys(ctx, nha141BearerRequest(&authv1.ListMcpKeysRequest{AccountId: admin.Msg.Account.Id}, userTokens.AccessToken))
	nha049Code(t, err, connect.CodePermissionDenied)
	_, err = accounts.UpdateAccount(ctx, nha141BearerRequest(&authv1.UpdateAccountRequest{AccountId: user.Msg.Account.Id, Role: &adminRole}, tokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	if status(empty.Msg.Secret) == 200 {
		t.Fatal("promotion expanded old credential ceiling")
	}
	// Policy mutation uses the existing ConfigWriter path and synchronously
	// cancels MCP leases while interactive sessions continue under new rights.
	policyKey := create("policy bound", nil)
	policyStream := request("/nha145/stream", policyKey.Secret, "policy")
	defer policyStream.Body.Close()
	policies := authv1connect.NewAuthorizationPolicyServiceClient(client, base)
	snapshot, err := policies.GetPolicy(ctx, nha141BearerRequest(&authv1.GetPolicyRequest{AccountId: admin.Msg.Account.Id}, tokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	source := []byte("schema: homeward.authorization/1\naccount_id: " + admin.Msg.Account.Id.Value + "\ngrants:\n  - scope: {family: state_registry, operation: read}\n    objects:\n      entities:\n        all: {capabilities: {all: {}}}\n")
	_, err = policies.ReplacePolicy(ctx, nha141BearerRequest(&authv1.ReplacePolicyRequest{AccountId: admin.Msg.Account.Id, YamlSource: source, Precondition: &authv1.ReplacePolicyRequest_Expected{Expected: snapshot.Msg.GetPresent().Version}}, tokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case label := <-canceled:
		if label != "policy" {
			t.Fatalf("unexpected policy cancellation %s", label)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("policy rights change left MCP stream open")
	}
	if status(policyKey.Secret) == 200 {
		t.Fatal("policy revision did not invalidate credential")
	}
	expiry := create("expiring", durationpb.New(time.Minute))
	streamExpiry := request("/nha145/stream", expiry.Secret, "expiry")
	defer streamExpiry.Body.Close()
	clock.Advance(clock.Now().Add(time.Minute))
	select {
	case label := <-canceled:
		if label != "expiry" {
			t.Fatalf("unexpected expiry %s", label)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("expiry stream stayed open")
	}
	if status(expiry.Secret) == 200 {
		t.Fatal("credential accepted at expiry boundary")
	}
}
