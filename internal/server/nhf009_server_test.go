package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	imported "github.com/damione1/Homeward/compat/ha/dashboard"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
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
	"google.golang.org/protobuf/encoding/protojson"
)

// Opt-in test executable: real server composition and typed control contracts, fake final
// physical-device driver only. Never included in homed or given a provider URL.
func TestNHF009ControlServer(t *testing.T) {
	out := os.Getenv("HOMEWARD_NHF009_FIXTURE")
	if out == "" {
		t.Skip("selected only by isolated NHF009 qualification")
	}

	scenario := nhf006Ack
	if input := os.Getenv("HOMEWARD_NHF009_SCENARIO"); input != "" {
		parsed, err := nhf006ParseScenario(input)
		if err != nil {
			t.Fatal(err)
		}
		switch parsed {
		case nhf006Ack, nhf006Lost:
			scenario = parsed
		default:
			t.Fatal("unsupported NHF009 fault scenario")
		}
	}
	root := os.Getenv("HOMEWARD_NHF009_SOURCES")
	if !filepath.IsAbs(out) || !filepath.IsAbs(root) {
		t.Fatal("fresh absolute fixture and source roots required")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	devices, err := nhf009ReadDevices(filepath.Join(root, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := registry.LoadManifest(scope.BaselineV1())
	if err != nil {
		t.Fatal(err)
	}
	dashboard, err := nhf005DashboardFixture()
	if err != nil || dashboard == nil || dashboard.catalogue == nil {
		t.Fatal("explicit server dashboard source required", err)
	}
	dashboard.compilation = imported.CompilationRecipes
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	settings := nha049Config(t, cluster.Port, data)
	ctx, signals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer signals()
	ctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	instance, _ := domain.ParseIntegrationInstanceID("nhf009-fake-only")
	var host *integrations.Manager
	var identities *registry.Repository
	factory := RuntimeFactory(func(factoryCtx context.Context, store *storage.Store) (RuntimePlan, error) {
		var err error
		identities, err = registry.NewRepository(store, manifest)
		if err != nil {
			return RuntimePlan{}, err
		}
		for _, device := range devices {
			alias, _ := device.alias.Value()
			if _, err := identities.ConfigureBinding(factoryCtx, instance, device.key, alias); err != nil {
				return RuntimePlan{}, err
			}
		}
		public, err := NewPublicPlan(nil, nil, accountTransportAudit{store: store})
		if err != nil {
			return RuntimePlan{}, err
		}
		if err := dashboard.register(public); err != nil {
			return RuntimePlan{}, err
		}
		plan := RuntimePlan{Identity: identities, Public: public,
			ConfigureAuthorization: func(_ context.Context, composition AuthorizationComposition) error {
				dashboard.authority = composition.ViewAuthorizer().(*policyViewAuthorizer).authority
				return nil
			},
			ConfigureActivation: func(_ context.Context, owner *configactivation.Owner) error {
				if owner == nil {
					return errors.New("actual activation owner required")
				}
				dashboard.activation = owner
				return nil
			},
			Configure: func(_ context.Context, actual *integrations.Manager, _ *storage.Store) error {
				host = actual
				return nil
			},
		}
		return plan, nil
	})
	done, ready := make(chan error, 1), make(chan net.Addr, 1)
	serverCtx, serverCancel := context.WithCancel(ctx)
	go func() {
		done <- RunWithStartupFactory(serverCtx, settings.startup,
			func(listenCtx context.Context, kind, _ string) (net.Listener, error) {
				return (&net.ListenConfig{}).Listen(listenCtx, kind, "127.0.0.1:0")
			},
			func(address net.Addr) { ready <- address }, factory)
	}()
	var address net.Addr
	select {
	case address = <-ready:
	case err := <-done:
		serverCancel()
		t.Fatal("isolated startup", err)
	case <-ctx.Done():
		serverCancel()
		t.Fatal(ctx.Err())
	}
	driver, calls := new(nhf008Driver), new(nhf006Calls)
	var protocolConfirmations, observedEffects uint64
	defer func() {
		count := host.CommandDriverEntries()
		serverCancel()
		joined := false
		select {
		case err := <-done:
			joined = true
			if err != nil {
				t.Error(err)
			}
		case <-time.After(15 * time.Second):
			t.Error("isolated server failed to join")
		}
		calls.mu.Lock()
		defer calls.mu.Unlock()
		var deviceArguments json.RawMessage
		driver.mu.Lock()
		allArguments := make([]json.RawMessage, 0, len(driver.arguments))
		for _, arguments := range driver.arguments {
			encoded, err := protojson.Marshal(arguments)
			if err != nil {
				t.Error(err)
			} else {
				allArguments = append(allArguments, encoded)
			}
		}
		if driver.last != nil {
			request, err := driver.last.Request()
			if err != nil {
				t.Error(err)
			} else {
				encoded, err := protojson.Marshal(request.Arguments)
				if err != nil {
					t.Error(err)
				} else {
					deviceArguments = encoded
				}
			}
		}
		driver.mu.Unlock()
		nhf003WritePrivate(t, out, "result.json", map[string]any{"server_joined": joined, "fake_driver_calls": driver.count.Load(), "command_driver_entries": count,
			"submit_ids": calls.submit, "get_ids": calls.get, "watch_ids": calls.watch, "entity_count": len(devices),
			"protocol_confirmations": protocolConfirmations, "observed_effects": observedEffects, "lost_responses": calls.lost.Load(),
			"native_login_calls": calls.logins.Load(), "native_revokes": calls.revokes.Load(),
			"dashboard_get_calls": dashboard.gets.Load(), "dashboard_watch_calls": dashboard.watches.Load(), "real_device_writes": 0, "last_device_arguments": deviceArguments, "device_arguments": allArguments})
	}()
	source, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	var candidates []integrations.Candidate
	for _, device := range devices {
		candidate, err := integrations.NewCandidate(device.key, []*registrywire.CapabilityDescription{device.capability()})
		if err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, candidate)
	}
	bindings, err := source.Discover(ctx, candidates)
	if err != nil || len(bindings) != len(devices) {
		t.Fatal("synthetic discovery", err)
	}
	if err := host.AttachCommandDriver(source, driver); err != nil {
		t.Fatal(err)
	}
	publish := func(missing bool) {
		t.Helper()
		for index, device := range devices {
			observation, err := device.observation(missing)
			if err != nil {
				t.Fatalf("synthetic %s observation: %v", device.kind, err)
			}
			if err := source.Publish(ctx, bindings[index], observation); err != nil {
				t.Fatalf("synthetic %s publication: %v", device.kind, err)
			}
		}
	}
	publish(false)
	resolved := make(map[string]domain.EntityID)
	for index, device := range devices {
		_, id, err := bindings[index].IDs()
		if err != nil {
			t.Fatal(err)
		}
		alias, _ := device.alias.Value()
		resolved[alias] = id
	}
	const login, password = "NHF009 isolated account", "NHF009 synthetic local password"
	account := nha162RunBootstrap(t, ctx, data, login, password)
	if err := dashboard.install(account.Value, resolved); err != nil {
		t.Fatal(err)
	}
	client := nha181Client(roots)
	defer client.CloseIdleConnections()
	base := "https://" + address.String()
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
		policy += "  - scope: {family: dashboards, operation: read}\n    objects: {dashboards: {own: {}}}\n"
		if _, err := policies.ReplacePolicy(ctx, nha141BearerRequest(&auth.ReplacePolicyRequest{AccountId: account, YamlSource: []byte(policy), Precondition: &auth.ReplacePolicyRequest_Expected{Expected: current.Msg.GetPresent().Version}}, tokens.AccessToken)); err != nil {
			t.Fatal(err)
		}
	}
	setPolicy(true)
	deadline := time.Now().Add(15 * time.Second)
	for {
		page, err := statev1connect.NewStateServiceClient(client, base).OpenView(ctx, nha141BearerRequest(&state.OpenViewRequest{Filter: &state.ViewFilter{}, PageSize: 32}, tokens.AccessToken))
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Msg.Entities) == len(resolved) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("synthetic host did not publish all entities")
		}
		time.Sleep(20 * time.Millisecond)
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(data, nha181CertificateName), filepath.Join(data, nha181PrivateKeyName))
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}, ForceAttemptHTTP2: true}
	defer transport.CloseIdleConnections()
	proxy := calls.proxy(t, base, transport, pair, scenario, true)
	defer func() { proxy.CloseClientConnections(); proxy.Close() }()
	certificate, err := os.ReadFile(filepath.Join(data, nha181CertificateName))
	if err != nil {
		t.Fatal(err)
	}
	nhf003WritePrivate(t, out, "ready.json", map[string]any{"mode": "synthetic-only", "origin": proxy.URL, "ca_pem": string(certificate), "login_name": login, "password": password, "entity_count": len(resolved), "scenario": scenario})
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var sequence uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(out, "stop")); err == nil {
				return
			}
			command, err := nhf009ReadControl(out, sequence)
			if err != nil {
				t.Fatal(err)
			}
			if command == nil {
				continue
			}
			switch command.action {
			case nhf009DenyWrite:
				setPolicy(false)
			case nhf009AllowWrite:
				setPolicy(true)
			case nhf009Outage:
				calls.network.cut()
			case nhf009RestoreNetwork:
				calls.network.outage.Store(false)
			case nhf009WithdrawCover, nhf009WithdrawLock, nhf009WithdrawAlarm:
				for index, device := range devices {
					selected := command.action == nhf009WithdrawCover && device.kind == nhf009Cover || command.action == nhf009WithdrawLock && device.kind == nhf009Lock || command.action == nhf009WithdrawAlarm && device.kind == nhf009Alarm
					if !selected {
						continue
					}
					description := device.capability()
					switch device.kind {
					case nhf009Cover:
						description.GetCover().Operations = nil
						description.GetCover().SetPositionPercent = nil
					case nhf009Lock:
						description.GetLock().Operations = nil
					case nhf009Alarm:
						description.GetAlarm().Operations = nil
					case nhf009Garage:
						t.Fatal("unexpected synthetic withdrawal")
					default:
						t.Fatal("unparsed synthetic kind")
					}
					candidate, err := integrations.NewCandidate(device.key, []*registrywire.CapabilityDescription{description})
					if err != nil {
						t.Fatal(err)
					}
					changed, err := source.Discover(ctx, []integrations.Candidate{candidate})
					if err != nil || len(changed) != 1 {
						t.Fatal("synthetic withdrawal", err)
					}
					bindings[index] = changed[0]
				}
			case nhf009Rediscover:
				bindings, err = source.Discover(ctx, candidates)
				if err != nil || len(bindings) != len(devices) {
					t.Fatal("synthetic rediscovery", err)
				}
				publish(false)
			case nhf009DropObservations:
				publish(true)
			case nhf009RestoreObservations:
				publish(false)
			case nhf009ConfirmLast:
				last, err := driver.nhf009Last(command.command)
				if err != nil {
					t.Fatal(err)
				}
				if err := last.ConfirmProtocol(nhf009ProtocolReceipt{command: *command.command}); err != nil {
					t.Fatal(err)
				}
				protocolConfirmations++
			case nhf009ObserveLast:
				if err := nhf009ObserveCommand(ctx, driver, command.command, source, bindings, devices); err != nil {
					t.Fatal(err)
				}
				observedEffects++
			default:
				t.Fatal("unparsed NHF009 control")
			}
			sequence = command.sequence
			ack := map[string]any{"sequence": sequence, "action": command.action}
			nhf003WritePrivate(t, out, fmt.Sprintf("ack-%02d.json", sequence), ack)
		}
	}
}
