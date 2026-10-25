package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/gen/home/actions/v1/actionsv1connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	dashboardwire "github.com/damione1/Homeward/gen/home/dashboard/v1"
	"github.com/damione1/Homeward/gen/home/dashboard/v1/dashboardv1connect"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/gen/home/state/v1/statev1connect"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
	"google.golang.org/protobuf/proto"
)

type nhf006Scenario string

const (
	nhf006Lost     nhf006Scenario = "lost-submit"
	nhf006Ack      nhf006Scenario = "ack-only"
	nhf006Expire   nhf006Scenario = "expire"
	nhf006Refusal  nhf006Scenario = "refusal"
	nhf006Withdraw nhf006Scenario = "withdrawn"
)

func nhf006ParseScenario(input string) (nhf006Scenario, error) {
	value := nhf006Scenario(input)
	switch value {
	case nhf006Lost, nhf006Ack, nhf006Expire, nhf006Refusal, nhf006Withdraw:
		return value, nil
	default:
		return "", errors.New("unknown synthetic action scenario")
	}
}

// No adapter, socket, broker, physical credential or device I/O exists here.
// The actual Host/ledger/authority are exercised through this final fake driver.
type nhf006Driver struct {
	count atomic.Uint64
	mu    sync.Mutex
	last  *integrations.AdmittedCommand
}

func (d *nhf006Driver) Dispatch(_ context.Context, command integrations.AdmittedCommand) error {
	if !command.ReadyForTransport() {
		return integrations.ErrCommandAuthorizationDenied
	}
	d.count.Add(1)
	d.mu.Lock()
	d.last = &command
	d.mu.Unlock()
	return command.Acknowledge()
}

type nhf006Calls struct {
	mu                     sync.Mutex
	submit, get, watch     []string
	lost                   atomic.Uint64
	logins, views, revokes atomic.Uint64
	refusalFaults          atomic.Uint64
	network                *nhf002Network
}

func (c *nhf006Calls) observe(path string, body []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.submit) >= 32 || len(c.get) >= 256 || len(c.watch) >= 256 {
		return false
	}
	// Server-streaming Connect requests use an envelope; unary messages do not.
	if len(body) >= 5 && body[0] == 0 && strings.Contains(path, "WatchAction") {
		body = body[5:]
	}
	switch path {
	case actionsv1connect.ActionServiceSubmitActionProcedure:
		value := new(actions.SubmitActionRequest)
		if proto.Unmarshal(body, value) != nil || value.Command == nil {
			return false
		}
		c.submit = append(c.submit, value.Command.CommandId)
	case actionsv1connect.ActionServiceGetActionProcedure:
		value := new(actions.GetActionRequest)
		if proto.Unmarshal(body, value) != nil {
			return false
		}
		c.get = append(c.get, value.CommandId)
	case actionsv1connect.ActionServiceWatchActionProcedure:
		value := new(actions.WatchActionRequest)
		if proto.Unmarshal(body, value) != nil {
			return false
		}
		c.watch = append(c.watch, value.CommandId)
	default:
		return false
	}
	return true
}
func (c *nhf006Calls) proxy(t *testing.T, upstream string, transport *http.Transport, pair tls.Certificate, scenario nhf006Scenario, native bool) *httptest.Server {
	t.Helper()
	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	lost := errors.New("synthetic Submit response lost after commit")
	proxy.ModifyResponse = func(response *http.Response) error {
		if scenario == nhf006Lost && response.Request.URL.Path == actionsv1connect.ActionServiceSubmitActionProcedure && response.StatusCode == http.StatusOK && c.lost.CompareAndSwap(0, 1) {
			data, err := io.ReadAll(io.LimitReader(response.Body, 65537))
			_ = response.Body.Close()
			clear(data)
			if err != nil {
				return err
			}
			return lost
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if errors.Is(err, lost) {
			panic(http.ErrAbortHandler)
		}
		http.Error(w, "synthetic upstream unavailable", http.StatusBadGateway)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case authv1connect.AuthServiceStartLoginProcedure:
			c.logins.Add(1)
		case authv1connect.AuthServiceRevokeSessionProcedure:
			c.revokes.Add(1)
		case statev1connect.StateServiceOpenViewProcedure:
			c.views.Add(1)
		}
		if strings.HasPrefix(r.URL.Path, "/"+actionsv1connect.ActionServiceName+"/") {
			data, err := io.ReadAll(io.LimitReader(r.Body, 65537))
			_ = r.Body.Close()
			if err != nil || len(data) > 65536 || !c.observe(r.URL.Path, data) {
				http.Error(w, "invalid bounded synthetic action", http.StatusBadRequest)
				return
			}
			// A wire fault creates a deterministic configuration race for the
			// native refusal case. Admission/typed refusal still run in the real
			// server; no handler invents a successful or failed CommandResponse.
			if native && scenario == nhf006Refusal && r.URL.Path == actionsv1connect.ActionServiceSubmitActionProcedure && c.refusalFaults.CompareAndSwap(0, 1) {
				request := new(actions.SubmitActionRequest)
				if err := proto.Unmarshal(data, request); err != nil || request.Command == nil {
					http.Error(w, "invalid synthetic refusal request", http.StatusBadRequest)
					return
				}
				request.Command.ConfigRevision++
				data, err = proto.Marshal(request)
				if err != nil {
					panic(err)
				}
				r.ContentLength = int64(len(data))
			}
			r.Body = io.NopCloser(bytes.NewReader(data))
		}
		proxy.ServeHTTP(w, r)
	}))
	c.network = &nhf002Network{Listener: server.Listener, connections: make(map[*nhf002Connection]struct{})}
	server.Listener = c.network
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, NextProtos: []string{"http/1.1"}}
	server.StartTLS()
	return server
}

// This executable is opt-in, loopback-only and unrelated to homed serving modes.
func TestNHF006ActionServer(t *testing.T) {
	out := os.Getenv("HOMEWARD_NHF006_FIXTURE")
	if out == "" {
		t.Skip("selected only by isolated action qualification")
	}
	if !filepath.IsAbs(out) {
		t.Fatal("fresh absolute fixture directory required")
	}
	scenario, err := nhf006ParseScenario(os.Getenv("HOMEWARD_NHF006_SCENARIO"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	settings := nha049Config(t, cluster.Port, data)
	ctx, signals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer signals()
	ctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	instance, _ := domain.ParseIntegrationInstanceID("nhf006-fake-only")
	devices, err := nhf006Devices()
	if err != nil {
		t.Fatal(err)
	}
	dashboard, err := nhf005DashboardFixture()
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) > 1 && dashboard == nil {
		t.Fatal("native four-family fixture requires explicit server dashboard source")
	}
	var host *integrations.Manager
	factory := RuntimeFactory(func(factoryCtx context.Context, store *storage.Store) (RuntimePlan, error) {
		manifest, err := registry.LoadManifest(scope.BaselineV1())
		if err != nil {
			return RuntimePlan{}, err
		}
		repository, err := registry.NewRepository(store, manifest)
		if err != nil {
			return RuntimePlan{}, err
		}
		for _, device := range devices {
			alias, err := device.alias.Value()
			if err != nil {
				return RuntimePlan{}, err
			}
			if _, err := repository.ConfigureBinding(factoryCtx, instance, device.key, alias); err != nil {
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
			}, ConfigureActivation: func(_ context.Context, owner *configactivation.Owner) error {
				if owner == nil {
					return errors.New("actual command activation owner required")
				}
				if dashboard != nil {
					dashboard.activation = owner
				}
				return nil
			}, Configure: func(_ context.Context, actual *integrations.Manager, _ *storage.Store) error {
				host = actual
				return nil
			}}, nil
	})
	driver := new(nhf006Driver)
	calls := new(nhf006Calls)
	var source *integrations.Session
	var bindings []integrations.Binding
	var done chan error
	var serverCancel context.CancelFunc
	address := "127.0.0.1:0"
	started, joined := 0, 0
	var entries uint64
	stopServer := func() {
		t.Helper()
		if serverCancel == nil {
			return
		}
		serverCancel()
		select {
		case err := <-done:
			joined++
			if err != nil {
				t.Error(err)
			}
		case <-time.After(15 * time.Second):
			t.Error("action server failed to join")
		}
		if host != nil {
			entries += host.CommandDriverEntries()
		}
		serverCancel = nil
	}
	defer func() {
		stopServer()
		calls.mu.Lock()
		defer calls.mu.Unlock()
		result := map[string]any{"server_joined": started == joined, "servers_started": started, "servers_joined": joined, "fake_driver_calls": driver.count.Load(), "command_driver_entries": entries, "submit_ids": calls.submit, "get_ids": calls.get, "watch_ids": calls.watch, "lost_responses": calls.lost.Load(), "refusal_faults": calls.refusalFaults.Load(), "scenario": scenario, "native_login_calls": calls.logins.Load(), "native_open_views": calls.views.Load(), "native_revokes": calls.revokes.Load(), "entity_count": len(devices)}
		if dashboard != nil {
			result["dashboard_get_calls"] = dashboard.gets.Load()
			result["dashboard_watch_calls"] = dashboard.watches.Load()
		}
		nhf003WritePrivate(t, out, "result.json", result)
	}()
	candidates := make([]integrations.Candidate, 0, len(devices))
	for _, device := range devices {
		candidate, err := integrations.NewCandidate(device.key, []*registrywire.CapabilityDescription{device.capability()})
		if err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, candidate)
	}
	startServer := func() {
		t.Helper()
		var serverCtx context.Context
		serverCtx, serverCancel = context.WithCancel(ctx)
		done = make(chan error, 1)
		ready := make(chan net.Addr, 1)
		go func() {
			done <- RunWithStartupFactory(serverCtx, settings.startup, func(listenCtx context.Context, kind, _ string) (net.Listener, error) {
				return (&net.ListenConfig{}).Listen(listenCtx, kind, address)
			}, func(a net.Addr) { ready <- a }, factory)
		}()
		select {
		case actual := <-ready:
			address = actual.String()
			started++
		case err := <-done:
			serverCancel = nil
			t.Fatal("startup failed", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		var err error
		source, err = host.OpenSession(instance)
		if err != nil {
			t.Fatal(err)
		}
		bindings, err = source.Discover(ctx, candidates)
		if err != nil || len(bindings) != len(devices) {
			t.Fatal("fake discovery", err)
		}
		if err := host.AttachCommandDriver(source, driver); err != nil {
			t.Fatal(err)
		}
		for index, device := range devices {
			observation, err := device.observation()
			if err != nil {
				t.Fatal(err)
			}
			if err := source.Publish(ctx, bindings[index], observation); err != nil {
				t.Fatal(err)
			}
		}
	}
	startServer()
	resolved := make(map[string]domain.EntityID)
	for index, device := range devices {
		_, entity, err := bindings[index].IDs()
		if err != nil {
			t.Fatal(err)
		}
		alias, err := device.alias.Value()
		if err != nil {
			t.Fatal(err)
		}
		resolved[alias] = entity
	}
	const login, password = "NHF006 isolated account", "NHF006 synthetic local password"
	account := nha162RunBootstrap(t, ctx, data, login, password)
	if dashboard != nil {
		if err := dashboard.install(account.Value, resolved); err != nil {
			t.Fatal(err)
		}
	}
	client := nha181Client(roots)
	defer client.CloseIdleConnections()
	base := "https://" + address
	tokens := nha162RunLogin(t, ctx, authv1connect.NewAuthServiceClient(client, base), login, password)
	policies := authv1connect.NewAuthorizationPolicyServiceClient(client, base)
	setPolicy := func(write bool) {
		t.Helper()
		current, err := policies.GetPolicy(ctx, nha141BearerRequest(&auth.GetPolicyRequest{AccountId: account}, tokens.AccessToken))
		if err != nil || current.Msg.GetPresent() == nil {
			t.Fatal("policy read", err)
		}
		policy := fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n", account.Value)
		permissions := []struct{ family, operation string }{{"state_registry", "read"}, {"actions", "read"}}
		if write {
			permissions = append(permissions, struct{ family, operation string }{"actions", "write"})
		}
		for _, permission := range permissions {
			policy += fmt.Sprintf("  - scope: {family: %s, operation: %s}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n", permission.family, permission.operation)
		}
		if dashboard != nil {
			policy += "  - scope: {family: dashboards, operation: read}\n    objects: {dashboards: {own: {}}}\n"
		}
		if _, err := policies.ReplacePolicy(ctx, nha141BearerRequest(&auth.ReplacePolicyRequest{AccountId: account, YamlSource: []byte(policy), Precondition: &auth.ReplacePolicyRequest_Expected{Expected: current.Msg.GetPresent().Version}}, tokens.AccessToken)); err != nil {
			t.Fatal("policy CAS", err)
		}
	}
	setPolicy(true)
	page, err := statev1connect.NewStateServiceClient(client, base).OpenView(ctx, nha141BearerRequest(&state.OpenViewRequest{Filter: &state.ViewFilter{}, PageSize: 16}, tokens.AccessToken))
	if err != nil || len(page.Msg.Entities) != len(devices) {
		t.Fatal("effective write permission absent", err)
	}
	for _, entity := range page.Msg.Entities {
		if len(entity.ActionAccess) != 1 || entity.ActionAccess[0].Access != state.ActionAccess_ACTION_ACCESS_ALLOWED {
			t.Fatal("effective per-slot write fact missing")
		}
	}
	if dashboard != nil {
		response, err := dashboardv1connect.NewDashboardServiceClient(client, base).GetDashboard(ctx, nha141BearerRequest(&dashboardwire.GetDashboardRequest{Id: &dashboardwire.DashboardId{Value: nhf005DashboardID}}, tokens.AccessToken))
		if err != nil || response.Msg.GetDashboard().GetNativeOverview() == nil || response.Msg.ResumeToken == "" {
			t.Fatal("authorized native dashboard is not ready", err)
		}
		if len(response.Msg.GetDashboard().GetNativeOverview().Observations) != len(devices) {
			t.Fatal("native favorites do not match the explicit synthetic fixture")
		}
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(data, nha181CertificateName), filepath.Join(data, nha181PrivateKeyName))
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}, ForceAttemptHTTP2: true}
	defer transport.CloseIdleConnections()
	proxy := calls.proxy(t, base, transport, pair, scenario, len(devices) > 1)
	defer func() {
		proxy.CloseClientConnections()
		proxy.Close()
	}()
	certificate, err := os.ReadFile(filepath.Join(data, nha181CertificateName))
	if err != nil {
		t.Fatal(err)
	}
	nhf003WritePrivate(t, out, "ready.json", map[string]any{"mode": "synthetic-only", "origin": proxy.URL, "ca_pem": string(certificate), "login_name": login, "password": password, "scenario": scenario})
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var lastControl uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(out, "stop")); err == nil {
				return
			}
			command, err := nhf006ReadControl(out, lastControl)
			if err != nil {
				t.Fatal(err)
			}
			if command == nil {
				continue
			}
			switch command.Action {
			case nhf006Cut:
				calls.network.cut()
			case nhf006Restore:
				calls.network.outage.Store(false)
			case nhf006DenyWrite:
				setPolicy(false)
			case nhf006AllowWrite:
				setPolicy(true)
			case nhf006WithdrawLight, nhf006WithdrawSwitch:
				kind := nhf006Switch
				if command.Action == nhf006WithdrawLight {
					kind = nhf006Light
				}
				index, err := nhf006DeviceIndex(devices, kind)
				if err != nil {
					t.Fatal(err)
				}
				// Re-discovery withdraws the command dimension from the same
				// exact capability. Entity retirement via Session.Withdraw is
				// permanent for this Host and is a different lifecycle case.
				description := devices[index].capability()
				switch kind {
				case nhf006Light:
					description.GetLight().BrightnessPercent = nil
				case nhf006Switch:
					description.GetSwitch().SetOn = false
				case nhf006Binary, nhf006Measurement:
					t.Fatal("read-only family cannot lose a command")
				default:
					t.Fatal("unparsed synthetic device kind")
				}
				candidate, err := integrations.NewCandidate(devices[index].key, []*registrywire.CapabilityDescription{description})
				if err != nil {
					t.Fatal(err)
				}
				changed, err := source.Discover(ctx, []integrations.Candidate{candidate})
				if err != nil || len(changed) != 1 {
					t.Fatal(err)
				}
				bindings[index] = changed[0]
			case nhf006Rediscover:
				bindings, err = source.Discover(ctx, candidates)
				if err != nil || len(bindings) != len(devices) {
					t.Fatal("rediscovery", err)
				}
				for index, device := range devices {
					observation, err := device.observation()
					if err != nil {
						t.Fatal(err)
					}
					if err := source.Publish(ctx, bindings[index], observation); err != nil {
						t.Fatal(err)
					}
				}
			case nhf006Confirm:
				if err := driver.confirm(ctx, source, bindings, devices); err != nil {
					t.Fatal(err)
				}
			case nhf006Restart:
				// A server restart loses its client TCP connections. Cut those
				// first so graceful retirement of this old test process cannot
				// masquerade as an explicit rights revocation on a live stream.
				calls.network.cut()
				stopServer()
				transport.CloseIdleConnections()
				startServer()
				calls.network.outage.Store(false)
			default:
				t.Fatal("unparsed synthetic control action")
			}
			lastControl = command.Sequence
			nhf003WritePrivate(t, out, fmt.Sprintf("ack-%02d.json", lastControl), map[string]any{"sequence": lastControl, "action": command.Action})

		}
	}
}
