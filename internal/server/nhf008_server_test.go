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

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/gen/home/state/v1/statev1connect"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/helpers"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
	"google.golang.org/protobuf/encoding/protojson"
)

// Opt-in test executable: real server composition and helper SQL, fake final
// physical-device driver only. Never included in homed or given a provider URL.
func TestNHF008ControlServer(t *testing.T) {
	out := os.Getenv("HOMEWARD_NHF008_FIXTURE")
	if out == "" {
		t.Skip("selected only by isolated NHF008 qualification")
	}
	root := os.Getenv("HOMEWARD_NHF008_SOURCES")
	if !filepath.IsAbs(out) || !filepath.IsAbs(root) {
		t.Fatal("fresh absolute fixture and source roots required")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	devices, err := nhf008ReadDevices(filepath.Join(root, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := bundle.CompileV2(root, "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := helpers.Definitions(candidate)
	if err != nil || len(definitions) != 8 {
		t.Fatal("eight retained-scope helper kinds required", err)
	}
	manifest, err := registry.LoadManifest(scope.BaselineV1())
	if err != nil {
		t.Fatal(err)
	}
	dashboard, err := nhf005DashboardFixture()
	if err != nil || dashboard == nil {
		t.Fatal("explicit server dashboard source required", err)
	}
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	settings := nha049Config(t, cluster.Port, data)
	ctx, signals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer signals()
	ctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	instance, _ := domain.ParseIntegrationInstanceID("nhf008-fake-only")
	helperInstance, _ := domain.ParseIntegrationInstanceID("homeward-helpers-v1")
	var host *integrations.Manager
	var identities *registry.Repository
	var helperRepository *helpers.Repository
	factory := RuntimeFactory(func(factoryCtx context.Context, store *storage.Store) (RuntimePlan, error) {
		var err error
		identities, err = registry.NewRepository(store, manifest)
		if err != nil {
			return RuntimePlan{}, err
		}
		helperRepository, err = helpers.NewRepository(store)
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
		if err := addActiveHelpers(&plan, candidate, store); err != nil {
			return RuntimePlan{}, err
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
	entries := make(map[string]nhf008Helper)
	initial := make(map[string]any)
	defer func() {
		// Snapshot SQL before closing the store, then join the actual server.
		final, snapshotErr := nhf008HelperSnapshots(context.Background(), helperRepository, entries)
		if snapshotErr != nil {
			t.Error(snapshotErr)
		}
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
			"submit_ids": calls.submit, "get_ids": calls.get, "watch_ids": calls.watch, "entity_count": len(devices) + len(definitions),
			"native_login_calls": calls.logins.Load(), "native_revokes": calls.revokes.Load(), "helper_sql_initial": initial, "helper_sql_final": final,
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
			observation, err := device.observation()
			if err != nil {
				t.Fatalf("synthetic %s observation: %v", device.kind, err)
			}
			if missing && (device.kind == nhf008Thermostat || device.kind == nhf008HVAC) {
				value, err := domain.NewClimateValue(domain.ClimateValueInput{Mode: nhf008Pointer(domain.ClimateModeHeat)})
				if err != nil {
					t.Fatal(err)
				}
				stateValue, err := domain.NewClimateStateValue(value)
				if err != nil {
					t.Fatal(err)
				}
				quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
				at := time.Now().UTC()
				observation, err = integrations.NewStateObservation(device.endpoint, quality, &stateValue, &at, at, nil)
				if err != nil {
					t.Fatal(err)
				}
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
	for _, definition := range definitions {
		alias, err := definition.Alias()
		if err != nil {
			t.Fatal(err)
		}
		key, err := helpers.ConfiguredSourceKey(alias)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := identities.Resolve(ctx, helperInstance, []integrations.SourceKey{key})
		if err != nil || len(rows) != 1 {
			t.Fatal("helper identity", err)
		}
		handle, err := helperRepository.Bind(ctx, rows[0].Entity, definition)
		if err != nil {
			t.Fatal(err)
		}
		text, _ := alias.Value()
		resolved[text] = rows[0].Entity
		entries[text] = nhf008Helper{definition, handle}
	}
	initial, err = nhf008HelperSnapshots(ctx, helperRepository, entries)
	if err != nil {
		t.Fatal(err)
	}
	const login, password = "NHF008 isolated account", "NHF008 synthetic local password"
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
			t.Fatal("helper pilot did not publish all entities")
		}
		time.Sleep(20 * time.Millisecond)
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(data, nha181CertificateName), filepath.Join(data, nha181PrivateKeyName))
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}, ForceAttemptHTTP2: true}
	defer transport.CloseIdleConnections()
	proxy := calls.proxy(t, base, transport, pair, nhf006Ack, true)
	defer func() { proxy.CloseClientConnections(); proxy.Close() }()
	certificate, err := os.ReadFile(filepath.Join(data, nha181CertificateName))
	if err != nil {
		t.Fatal(err)
	}
	nhf003WritePrivate(t, out, "ready.json", map[string]any{"mode": "synthetic-only", "origin": proxy.URL, "ca_pem": string(certificate), "login_name": login, "password": password, "entity_count": len(resolved)})
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
			command, err := nhf008ReadControl(out, sequence)
			if err != nil {
				t.Fatal(err)
			}
			if command == nil {
				continue
			}
			switch command.Action {
			case nhf008ReadSQL:
				// Reading a fixture's own SQL rows never publishes an observation.
			case nhf008DenyWrite:
				setPolicy(false)
			case nhf008AllowWrite:
				setPolicy(true)
			case nhf008WithdrawThermostat:
				for index, device := range devices {
					if device.kind != nhf008Thermostat {
						continue
					}
					description := device.capability()
					description.GetClimate().SetpointCelsius = nil
					candidate, err := integrations.NewCandidate(device.key, []*registrywire.CapabilityDescription{description})
					if err != nil {
						t.Fatal(err)
					}
					changed, err := source.Discover(ctx, []integrations.Candidate{candidate})
					if err != nil || len(changed) != 1 {
						t.Fatal(err)
					}
					bindings[index] = changed[0]
				}
			case nhf008Rediscover:
				bindings, err = source.Discover(ctx, candidates)
				if err != nil || len(bindings) != len(devices) {
					t.Fatal(err)
				}
				publish(false)
			case nhf008DropObservations:
				publish(true)
			case nhf008RestoreObservations:
				publish(false)
			case nhf008SQLRace:
				entry := entries["counter.bathroom_heating_used_today"]
				current, err := helperRepository.Restore(ctx, entry.handle, entry.definition)
				if err != nil {
					t.Fatal(err)
				}
				value := helpers.Counter(2)
				// Competing local SQL writer deliberately does not publish. The
				// frontend's old helper_revision must fail the real pilot's CAS.
				if _, err := helperRepository.CompareAndSwap(ctx, entry.handle, entry.definition, current.Revision(), value); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("unparsed NHF008 control")
			}
			sequence = command.Sequence
			ack := map[string]any{"sequence": sequence, "action": command.Action}
			if command.Action == nhf008ReadSQL {
				rows, err := nhf008HelperSnapshots(ctx, helperRepository, entries)
				if err != nil {
					t.Fatal(err)
				}
				ack["helpers"] = rows
			}
			nhf003WritePrivate(t, out, fmt.Sprintf("ack-%02d.json", sequence), ack)
		}
	}
}
