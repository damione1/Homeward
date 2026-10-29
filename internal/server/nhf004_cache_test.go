package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	dashboardv1 "github.com/damione1/Homeward/gen/home/dashboard/v1"
	"github.com/damione1/Homeward/gen/home/dashboard/v1/dashboardv1connect"
	statev1 "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/gen/home/state/v1/statev1connect"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
)

type nhf004Action string

const (
	nhf004Advance  nhf004Action = "advance"
	nhf004Load     nhf004Action = "load"
	nhf004Burst    nhf004Action = "burst"
	nhf004Outage   nhf004Action = "outage"
	nhf004Restore  nhf004Action = "restore"
	nhf004Deny     nhf004Action = "deny"
	nhf004Allow    nhf004Action = "allow"
	nhf004Withdraw nhf004Action = "withdraw"
	nhf004Restart  nhf004Action = "restart"
)

func (a *nhf004Action) UnmarshalJSON(raw []byte) error {
	var input string
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	value := nhf004Action(input)
	switch value {
	case nhf004Advance, nhf004Load, nhf004Burst, nhf004Outage, nhf004Restore, nhf004Deny, nhf004Allow, nhf004Withdraw, nhf004Restart:
		*a = value
		return nil
	default:
		return fmt.Errorf("unknown synthetic cache action %q", input)
	}
}

type nhf004Command struct {
	Sequence uint64       `json:"sequence"`
	Action   nhf004Action `json:"action"`
}

// Explicitly selected test executable, never a homed serving mode. SQL, TLS,
// native auth, authorization, snapshots and streams are the actual composition.
// Only the 850 observation sources are synthetic. No pilot/driver is attached.
func TestNHF004CacheServer(t *testing.T) {
	out := os.Getenv("HOMEWARD_NHF004_FIXTURE")
	if out == "" {
		t.Skip("selected only by the native cache qualification harness")
	}
	if !filepath.IsAbs(out) {
		t.Fatal("fresh absolute private directory required")
	}
	uiProfile, err := parseNHF005UIProfile(os.Getenv("HOMEWARD_FRONTEND_UI_PROFILE"))
	if err != nil {
		t.Fatal(err)
	}
	appMode := false
	if input := os.Getenv("HOMEWARD_NHF004_APP"); input != "" {
		var err error
		appMode, err = strconv.ParseBool(input)
		if err != nil {
			t.Fatal("invalid opt-in application mode:", err)
		}
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name string, value any) { nhf003WritePrivate(t, out, name, value) }
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixtureWith(t, data, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), func(cert *x509.Certificate) {
		cert.DNSNames = append(cert.DNSNames, "nhf004.example.test")
	})
	public := strings.Replace(nha181TLSYAML(), "public:\n", "public:\n  url: https://nhf004.example.test\n", 1)
	if uiProfile == nhf005HomeUI {
		name, err := uiProfile.revision().Value()
		if err != nil {
			t.Fatal(err)
		}
		public += "scope_revision: " + name + "\n"
	}
	settings := nha181Config(t, cluster.Port, data, "127.0.0.1:0", public)
	ctx, signals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer signals()
	ctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
	defer cancel()
	var aliases struct {
		Entities []struct {
			ID string `json:"entity_id"`
		} `json:"entities"`
	}
	if err := json.Unmarshal(scope.ManifestV1(), &aliases); err != nil || len(aliases.Entities) < 850 {
		t.Fatal("850 baseline identities required:", err)
	}
	if uiProfile == nhf005HomeUI {
		// The explicit, existing scope amendment adds climate.home. The 850
		// baseline manifest and the default NHF004 profile remain unchanged.
		aliases.Entities = append(aliases.Entities, struct {
			ID string `json:"entity_id"`
		}{ID: "climate.home"})
	}
	instance, _ := domain.ParseIntegrationInstanceID("nhf004-synthetic-only")
	keys := make([]integrations.SourceKey, len(aliases.Entities))
	candidates := make([]integrations.Candidate, len(keys))
	for i := range keys {
		var err error
		keys[i], err = integrations.NewSourceKey(fmt.Sprintf("fixture-%03d", i), "relay")
		if err != nil {
			t.Fatal(err)
		}
		candidates[i], err = integrations.NewCandidate(keys[i], uiProfile.capabilities(aliases.Entities[i].ID, i))
		if err != nil {
			t.Fatal(err)
		}
	}
	var host *integrations.Manager
	var activation *configactivation.Owner
	var source *integrations.Session
	var bindings []integrations.Binding
	var network *nhf002Network
	var proxy *nhf003Proxy
	var serverCancel context.CancelFunc
	var done chan error
	address := "127.0.0.1:0"
	started, joined, connections := 0, 0, 0
	var drivers uint64
	var published uint64
	dashboard, dashboardErr := nhf005DashboardFixture()
	if dashboardErr != nil {
		t.Fatal(dashboardErr)
	}
	publish := func(index int, sequence *uint64) {
		t.Helper()
		observation, err := uiProfile.observation(aliases.Entities[index].ID, sequence, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if err := source.Publish(ctx, bindings[index], observation); err != nil {
			t.Fatal("synthetic publication:", err)
		}
	}
	factory := RuntimeFactory(func(factoryCtx context.Context, store *storage.Store) (RuntimePlan, error) {
		manifest, err := registry.LoadManifest(uiProfile.revision())
		if err != nil {
			return RuntimePlan{}, err
		}
		repository, err := registry.NewRepository(store, manifest)
		if err != nil {
			return RuntimePlan{}, err
		}
		for i, key := range keys {
			if _, err := repository.ConfigureBinding(factoryCtx, instance, key, aliases.Entities[i].ID); err != nil {
				return RuntimePlan{}, err
			}
		}
		var public *PublicPlan
		if dashboard != nil {
			public, err = NewPublicPlan(nil, nil, accountTransportAudit{store: store})
			if err != nil {
				return RuntimePlan{}, err
			}
			if err := dashboard.register(public); err != nil {
				return RuntimePlan{}, err
			}
		}
		return RuntimePlan{Identity: repository, Public: public,
			ConfigureAuthorization: func(_ context.Context, composition AuthorizationComposition) error {
				if dashboard != nil {
					dashboard.authority = composition.ViewAuthorizer().(*policyViewAuthorizer).authority
				}
				return nil
			},
			Configure: func(_ context.Context, actual *integrations.Manager, _ *storage.Store) error {
				host = actual
				return nil
			},
			ConfigureActivation: func(_ context.Context, actual *configactivation.Owner) error {
				activation = actual
				if dashboard != nil {
					dashboard.activation = actual
				}
				return nil
			},
		}, nil
	})
	stopServer := func() {
		t.Helper()
		if serverCancel == nil {
			return
		}
		serverCancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("server shutdown:", err)
			} else {
				joined++
			}
		case <-time.After(15 * time.Second):
			t.Error("server failed to join")
		}
		if network != nil {
			network.mu.Lock()
			connections += len(network.connections)
			network.mu.Unlock()
		}
		drivers += host.CommandDriverEntries()
		serverCancel = nil
	}
	defer func() {
		if proxy != nil {
			proxy.close()
		}
		stopServer()
		result := map[string]any{"published": published, "servers_started": started, "servers_joined": joined, "server_joined": started == joined, "command_driver_entries": drivers, "connections_remaining": connections}
		if proxy != nil {
			result["start_login_calls"] = proxy.login.Load()
			result["refresh_calls"] = proxy.refresh.Load()
			result["revoke_calls"] = proxy.revoke.Load()
			result["revoke_ok"] = proxy.revokeOK.Load()
			result["state_read_calls"] = proxy.stateReads.Load()
			result["state_read_ok"] = proxy.stateReadOK.Load()
			result["state_release_calls"] = proxy.stateReleases.Load()
			result["state_release_ok"] = proxy.stateReleaseOK.Load()
			result["action_rpc_calls"] = proxy.actions.Load()
			result["login_observation"] = proxy.loginObservation()
		}
		if dashboard != nil {
			result["dashboard_list_calls"] = dashboard.lists.Load()
			result["dashboard_get_calls"] = dashboard.gets.Load()
			result["dashboard_watch_calls"] = dashboard.watches.Load()
		}
		write("result.json", result)
		if started != joined || connections != 0 || drivers != 0 {
			t.Error("server ownership or physical-driver invariant failed")
		}
	}()
	startServer := func() {
		t.Helper()
		var serverCtx context.Context
		serverCtx, serverCancel = context.WithCancel(ctx)
		done = make(chan error, 1)
		ready := make(chan net.Addr, 1)
		go func() {
			done <- RunWithStartupFactory(serverCtx, settings.startup, func(listenCtx context.Context, kind, _ string) (net.Listener, error) {
				listener, err := (&net.ListenConfig{}).Listen(listenCtx, kind, address)
				if err != nil {
					return nil, err
				}
				network = &nhf002Network{Listener: listener, connections: make(map[*nhf002Connection]struct{})}
				return network, nil
			}, func(a net.Addr) { ready <- a }, factory)
		}()
		select {
		case actual := <-ready:
			address = actual.String()
			started++
		case err := <-done:
			serverCancel = nil // The failed server has already been joined here.
			t.Fatal("server startup:", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if host == nil || activation == nil {
			t.Fatal("real host/activation not composed")
		}
		var err error
		source, err = host.OpenSession(instance)
		if err != nil {
			t.Fatal(err)
		}
		bindings = nil
		for first := 0; first < len(keys); first += 250 {
			batch, err := source.Discover(ctx, candidates[first:min(first+250, len(keys))])
			if err != nil {
				t.Fatal("discovery:", err)
			}
			bindings = append(bindings, batch...)
		}
		for i := range bindings {
			publish(i, nil)
		}
	}
	startServer()
	const login, password = "NHF004 isolated account", "NHF004 synthetic local password"
	account := nha162RunBootstrap(t, ctx, data, login, password)
	if dashboard != nil {
		resolved := make(map[string]domain.EntityID)
		for i, binding := range bindings {
			_, entity, err := binding.IDs()
			if err != nil {
				t.Fatal(err)
			}
			resolved[aliases.Entities[i].ID] = entity
		}
		if err := dashboard.install(account.Value, resolved); err != nil {
			t.Fatal(err)
		}
	}
	client := nha181Client(roots)
	defer client.CloseIdleConnections()
	base := "https://" + address
	auth := authv1connect.NewAuthServiceClient(client, base)
	tokens := nha162RunLogin(t, ctx, auth, login, password)
	policies := authv1connect.NewAuthorizationPolicyServiceClient(client, base)
	setPolicy := func(allow bool) {
		t.Helper()
		current, err := policies.GetPolicy(ctx, nha141BearerRequest(&authv1.GetPolicyRequest{AccountId: account}, tokens.AccessToken))
		if err != nil || current.Msg.GetPresent() == nil {
			t.Fatal("policy read:", err)
		}
		policy := fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants: []\n", account.Value)
		if allow {
			policy = fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n  - scope: {family: state_registry, operation: read}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n", account.Value)
		}
		if allow && dashboard != nil {
			policy += "  - scope: {family: dashboards, operation: read}\n    objects: {dashboards: {own: {}}}\n"
		}
		if _, err := policies.ReplacePolicy(ctx, nha141BearerRequest(&authv1.ReplacePolicyRequest{AccountId: account, YamlSource: []byte(policy), Precondition: &authv1.ReplacePolicyRequest_Expected{Expected: current.Msg.GetPresent().Version}}, tokens.AccessToken)); err != nil {
			t.Fatal("policy CAS:", err)
		}
	}
	setPolicy(true)
	if dashboard != nil {
		api := dashboardv1connect.NewDashboardServiceClient(client, base)
		response, err := api.GetDashboard(ctx, nha141BearerRequest(&dashboardv1.GetDashboardRequest{Id: &dashboardv1.DashboardId{Value: nhf005DashboardID}}, tokens.AccessToken))
		if err != nil || response.Msg.GetDashboard().GetNativeOverview() == nil || response.Msg.ResumeToken == "" {
			t.Fatal("synthetic dashboard TLS readiness:", err)
		}
	}
	api := statev1connect.NewStateServiceClient(client, base)
	check, err := api.OpenView(ctx, nha141BearerRequest(&statev1.OpenViewRequest{Filter: &statev1.ViewFilter{}, PageSize: 256}, tokens.AccessToken))
	if err != nil || len(check.Msg.Entities) != 256 || check.Msg.NextPageToken == "" {
		t.Fatal("native paginated readiness:", err)
	}
	certificate, err := os.ReadFile(filepath.Join(data, nha181CertificateName))
	if err != nil {
		t.Fatal(err)
	}
	_, firstID, err := bindings[0].IDs()
	if err != nil {
		t.Fatal(err)
	}
	first, _ := firstID.Value()
	clientOrigin := base
	if appMode {
		pair, err := tls.LoadX509KeyPair(filepath.Join(data, nha181CertificateName), filepath.Join(data, nha181PrivateKeyName))
		if err != nil {
			t.Fatal(err)
		}
		// Opt-in pass-through TLS observer used only to count actual native app
		// login/read/revoke calls. It changes no auth or StateService response.
		proxy = nhf003StartProxy(t, base, roots, pair, out, nhf003LoginExpectation{name: login, password: password})
		clientOrigin = proxy.server.URL
	}
	readyData := map[string]any{"mode": "synthetic-only", "origin": clientOrigin, "ca_pem": string(certificate), "login_name": login, "password": password, "first_entity": first, "entity_count": len(keys)}
	if appMode {
		readyData["ui_profile"] = uiProfile.label()
		// Derive UI assertions from the real paginated view, not by reproducing
		// the server's ordering or zipping identities with separate arrays.
		page := check.Msg
		readyData["expected_first_alias"] = page.Entities[0].CurrentAlias
		count := len(page.Entities)
		for page.NextPageToken != "" {
			response, err := api.ReadPage(ctx, nha141BearerRequest(&statev1.ReadPageRequest{SnapshotToken: page.SnapshotToken, PageToken: page.NextPageToken}, tokens.AccessToken))
			if err != nil || response.Msg.GetPage() == nil || len(response.Msg.Page.Entities) == 0 {
				t.Fatal("native UI expected-row capture:", err)
			}
			page = response.Msg.Page
			count += len(page.Entities)
		}
		if count != len(keys) {
			t.Fatal("native UI expected-row capture incomplete")
		}
		readyData["expected_last_alias"] = page.Entities[len(page.Entities)-1].CurrentAlias
	}
	write("ready.json", readyData)
	control := time.NewTicker(5 * time.Millisecond)
	defer control.Stop()
	load := time.NewTicker(50 * time.Millisecond)
	defer load.Stop()
	var lastCommand uint64
	remaining := 0
	var loadStarted time.Time
	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				t.Error("synthetic control deadline exceeded")
			}
			return
		case <-load.C:
			if remaining > 0 {
				published++
				publish(int((published-1)%850), &published)
				remaining--
				if remaining == 0 {
					write("load-done.json", map[string]any{"updates": 1000, "elapsed_seconds": time.Since(loadStarted).Seconds(), "last_sequence": published})
				}
			}
		case <-control.C:
			if _, err := os.Stat(filepath.Join(out, "stop")); err == nil {
				return
			}
			raw, err := os.ReadFile(filepath.Join(out, "command.json"))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			var command nhf004Command
			if err := json.Unmarshal(raw, &command); err != nil || command.Sequence == 0 || command.Sequence > 64 || command.Action == "" {
				t.Fatal("invalid synthetic command:", err)
			}
			if command.Sequence == lastCommand {
				continue
			}
			if command.Sequence != lastCommand+1 {
				t.Fatal("out of order synthetic control")
			}
			switch command.Action {
			case nhf004Advance:
				published++
				publish(0, &published)
			case nhf004Load:
				if remaining != 0 || !loadStarted.IsZero() {
					t.Fatal("load can be started only once")
				}
				remaining, loadStarted = 1000, time.Now()
			case nhf004Burst:
				for range 17000 {
					published++
					publish(0, &published)
				}
			case nhf004Outage:
				network.cut()
			case nhf004Restore:
				network.outage.Store(false)
			case nhf004Deny:
				setPolicy(false)
			case nhf004Allow:
				setPolicy(true)
				if dashboard != nil {
					api := dashboardv1connect.NewDashboardServiceClient(client, base)
					response, err := api.GetDashboard(ctx, nha141BearerRequest(&dashboardv1.GetDashboardRequest{Id: &dashboardv1.DashboardId{Value: nhf005DashboardID}}, tokens.AccessToken))
					if err != nil || response.Msg.GetDashboard().GetNativeOverview() == nil || response.Msg.ResumeToken == "" {
						t.Fatal("synthetic dashboard TLS readiness:", err)
					}
				}
			case nhf004Withdraw:
				if err := source.Withdraw(ctx, bindings[0]); err != nil {
					t.Fatal("synthetic capability withdrawal:", err)
				}
			case nhf004Restart:
				stopServer()
				startServer()
			}
			lastCommand = command.Sequence
			write(fmt.Sprintf("ack-%02d.json", lastCommand), map[string]any{"sequence": lastCommand, "published": published, "action": command.Action})
		}
	}
}
