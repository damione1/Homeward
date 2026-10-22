package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

// Uses the real YAML ConfigWriter and activationPolicies participant. The
// reservation retains BeginMutation across Publish and our assertions, so an
// asynchronous watcher cannot pass Authority.Current until Release below.
func TestNHA145YAMLActivationCancelsBeforeMutationRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cluster := testpg.Start(t)
	settings := nha049Config(t, cluster.Port, shortPrivateDataDir(t))
	pg, err := settings.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(ctx, pg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapMFAPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	const password = "NHA145 activation synthetic password"
	admin, err := newAccountInput(ctx, "Activation Admin", "Admin", password, authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapAdmin(ctx, admin); err != nil {
		t.Fatal(err)
	}
	other, err := newAccountInput(ctx, "Activation Other", "Other", password, authv1.AccountRole_ACCOUNT_ROLE_USER)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAccount(ctx, admin.ID, other); err != nil {
		t.Fatal(err)
	}
	sourceFor := func(account contracts.AccountID, expanded bool, comment string) authorization.PolicySource {
		t.Helper()
		id, _ := account.Value()
		objects := "[{entity_id: fixture-A, capabilities: {all: {}}}]"
		if expanded {
			objects = "[{entity_id: fixture-A, capabilities: {all: {}}}, {entity_id: fixture-B, capabilities: {all: {}}}]"
		}
		source, err := authorization.AdmitPolicySource(account, []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n  - scope: {family: state_registry, operation: read}\n    objects: {entities: {explicit: %s}}\n# %s\n", id, objects, comment)))
		if err != nil {
			t.Fatal(err)
		}
		return source
	}
	expectedFor := func(account contracts.AccountID) authorization.ExpectedVersion {
		t.Helper()
		snapshot, err := store.AuthorizationPolicy(ctx, account)
		if err != nil {
			t.Fatal(err)
		}
		version, err := snapshot.Version()
		if err != nil {
			t.Fatal(err)
		}
		expected, err := authorization.ExpectVersion(version)
		if err != nil {
			t.Fatal(err)
		}
		return expected
	}
	for _, account := range []contracts.AccountID{admin.ID, other.ID} {
		if _, err := store.ReplaceAuthorizationPolicy(ctx, admin.ID, account, sourceFor(account, false, "initial"), expectedFor(account)); err != nil {
			t.Fatal(err)
		}
	}
	coordinator, err := prepareLocalAuthorization(ctx, store, realSessionClock{})
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := composeSessionsWithCoordinator(nil, store, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	type admitted struct {
		label     string
		ctx       context.Context
		principal authorization.Principal
	}
	admissions := make(chan admitted, 8)
	permission, err := authorization.NewPermission(authv1.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY, authv1.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct {
		kind RouteKind
		path string
	}{{RouteKindMCP, "/nha145/activation/mcp"}, {RouteKindMedia, "/nha145/activation/native"}} {
		if err := plan.Register(route.kind, RouteMethodGet, route.path, RouteAccessProtected, RouteFlowServerStream, RouteTransportTLSRequired, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal, e := authorization.PrincipalFromContext(r.Context())
			if e != nil {
				return
			}
			admissions <- admitted{r.Header.Get("X-Fixture-Label"), r.Context(), principal}
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		})); err != nil {
			t.Fatal(err)
		}
		if err := plan.BindPermission(route.kind, RouteMethodGet, route.path, permission); err != nil {
			t.Fatal(err)
		}
	}
	public := config.DefaultPublicSettings()
	compiled, err := compilePublicPlan(plan, public)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(compiled.handler(public, newPublicAdmissions()))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer func() { cancel(); server.Close() }()
	client := server.Client()
	defer client.CloseIdleConnections()
	auth := authv1connect.NewAuthServiceClient(client, server.URL)
	keys := authv1connect.NewMcpKeyServiceClient(client, server.URL)
	login := func(name string) *authv1.SessionTokens {
		t.Helper()
		response, err := auth.StartLogin(ctx, connect.NewRequest(&authv1.StartLoginRequest{LoginName: name, Password: password}))
		if err != nil {
			t.Fatal(err)
		}
		tokens := response.Msg.Decision.GetAuthenticated()
		if tokens == nil {
			t.Fatal("native login did not authenticate")
		}
		return tokens
	}
	adminTokens := login("Activation Admin")
	otherTokens := login("Activation Other")
	issue := func(token, name string) *authv1.CreateMcpKeyResponse {
		t.Helper()
		r, err := keys.CreateMcpKey(ctx, nha141BearerRequest(&authv1.CreateMcpKeyRequest{ClientName: name, Scopes: []*authv1.Scope{{Family: authv1.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY, Operation: authv1.ScopeOperation_SCOPE_OPERATION_READ}}}, token))
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg
	}
	first := issue(adminTokens.AccessToken, "YAML-bound")
	sibling := issue(otherTokens.AccessToken, "other-account")
	open := func(path, secret, label string) (*http.Response, admitted) {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+secret)
		request.Header.Set("X-Fixture-Label", label)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != 200 {
			response.Body.Close()
			t.Fatalf("stream status %d", response.StatusCode)
		}
		select {
		case admission := <-admissions:
			if admission.label != label {
				t.Fatal("wrong stream context")
			}
			return response, admission
		case <-ctx.Done():
			response.Body.Close()
			t.Fatal(ctx.Err())
			return nil, admitted{}
		}
	}
	firstResponse, old := open("/nha145/activation/mcp", first.Secret, "old")
	defer firstResponse.Body.Close()
	otherResponse, otherAdmission := open("/nha145/activation/mcp", sibling.Secret, "other")
	defer otherResponse.Body.Close()
	nativeResponse, nativeAdmission := open("/nha145/activation/native", adminTokens.AccessToken, "native")
	defer nativeResponse.Body.Close()
	// Native authentication itself refreshes authorization. It must leave
	// unchanged MCP bindings alive, including another account's independent key.
	if old.ctx.Err() != nil || otherAdmission.ctx.Err() != nil {
		t.Fatal("ordinary native refresh canceled an unchanged MCP key")
	}
	entityB, _ := domain.ParseEntityID("fixture-B")
	slot, _ := authorization.NewCapabilitySlot("core.light.v1", "main")
	objectB, err := authorization.EntityObject(entityB, slot)
	if err != nil {
		t.Fatal(err)
	}
	before, err := coordinator.authority.Current(old.principal)
	if err != nil {
		t.Fatal(err)
	}
	if before.Authorize(permission, objectB) == nil {
		t.Fatal("fixture-B allowed before expansion")
	}
	publish := func(expanded bool, comment string, lease admitted, changed bool) {
		t.Helper()
		source := sourceFor(admin.ID, expanded, comment)
		expected := expectedFor(admin.ID)
		change, err := configactivation.NewPolicyChange(source, expected)
		if err != nil {
			t.Fatal(err)
		}
		reservation, err := (activationPolicies{coordinator: coordinator}).ReserveActivationPolicies(ctx, []configactivation.PolicyChange{change})
		if err != nil {
			t.Fatal(err)
		}
		defer reservation.Release()
		// Hold an admission whose first Err read has already observed a live
		// context. Keeping that result until publication forces the stale first
		// check independently of scheduler timing and the watcher goroutine.
		waiting := newNHA145PausedFirstErrContext(lease.ctx)
		defer waiting.resume()
		type readResult struct {
			err       error
			callbacks int
		}
		readDone := make(chan readResult, 1)
		go func() {
			callbacks := 0
			err := coordinator.authority.WithCurrent(waiting, lease.principal, func(current authorization.Current) error {
				callbacks++
				if expanded {
					return current.Authorize(permission, objectB)
				}
				return nil
			})
			readDone <- readResult{err, callbacks}
		}()
		select {
		case initialErr := <-waiting.observed:
			if initialErr != nil {
				t.Fatal("waiter did not start with a live MCP context", initialErr)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		// This is the same real admitted YAML and ConfigTx/ConfigWriter persistence
		// consumed by the production activation participant's publication step.
		if _, err := store.ReplaceAuthorizationPolicy(ctx, admin.ID, admin.ID, source, expected); err != nil {
			t.Fatal(err)
		}
		if err := reservation.Publish(ctx); err != nil {
			t.Fatal(err)
		}
		// BeginMutation remains held here. The old watcher is blocked behind it;
		// no scheduling delay or eventual timeout can satisfy these assertions.
		if changed && lease.ctx.Err() == nil {
			t.Fatal("MCP context still alive after YAML Publish, before mutation Release")
		}
		if !changed && lease.ctx.Err() != nil {
			t.Fatal("semantic no-op YAML publication canceled MCP context")
		}
		if nativeAdmission.ctx.Err() != nil || otherAdmission.ctx.Err() != nil {
			t.Fatal("YAML edit canceled native session or another account's key")
		}
		waiting.resume()
		reservation.Release()
		select {
		case result := <-readDone:
			if changed && (!errors.Is(result.err, context.Canceled) || result.callbacks != 0) {
				t.Fatalf("pre-publication MCP waiter reached changed policy: callbacks=%d err=%v", result.callbacks, result.err)
			}
			if !changed && (result.err != nil || result.callbacks != 1) {
				t.Fatalf("semantic no-op rejected live MCP waiter: callbacks=%d err=%v", result.callbacks, result.err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	publish(true, "expansion", old, true)
	if err := coordinator.authority.WithCurrent(old.ctx, old.principal, func(current authorization.Current) error { return current.Authorize(permission, objectB) }); !errors.Is(err, context.Canceled) {
		t.Fatalf("old context accessed newly authorized fixture-B: %v", err)
	}
	rejectOld := func(secret string) {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/nha145/activation/mcp", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+secret)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("old key reconnected after YAML edit: %d", response.StatusCode)
		}
	}
	rejectOld(first.Secret)
	fresh := issue(adminTokens.AccessToken, "explicit new ceiling")
	freshResponse, freshAdmission := open("/nha145/activation/mcp", fresh.Secret, "fresh")
	defer freshResponse.Body.Close()
	current, err := coordinator.authority.Current(freshAdmission.principal)
	if err != nil || current.Authorize(permission, objectB) != nil {
		t.Fatal("newly issued key did not receive explicitly expanded policy", err)
	}
	publish(true, "different exact YAML, same effective grants", freshAdmission, false)
	publish(false, "reduction", freshAdmission, true)
	rejectOld(fresh.Secret)
	// A new explicit issue after the reduction is valid within the new ceiling.
	reduced := issue(adminTokens.AccessToken, "reduced ceiling")
	reducedResponse, reducedAdmission := open("/nha145/activation/mcp", reduced.Secret, "reduced")
	defer reducedResponse.Body.Close()
	current, err = coordinator.authority.Current(reducedAdmission.principal)
	if err != nil {
		t.Fatal(err)
	}
	if current.Authorize(permission, objectB) == nil {
		t.Fatal("reduced policy still exposes fixture-B")
	}
}
