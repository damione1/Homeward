package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	statewire "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/gen/home/state/v1/statev1connect"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/helpers"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/view"
	"github.com/damione1/Homeward/scope"
)

// The fixed installation has no input_number. This independent test manifest
// admits the explicit contract corpus to the real SQL registry, without editing
// scope assets or weakening Run's immutable launch-selection guard.
func nhf008NumberManifest(alias domain.HAAlias) (registry.Manifest, error) {
	text, err := alias.Value()
	if err != nil {
		return registry.Manifest{}, err
	}
	ids := []string{text}
	for index := 1; index < 850; index++ {
		ids = append(ids, fmt.Sprintf("sensor.nhf008_number_fixture_%03d", index))
	}
	sort.Strings(ids)
	digest := sha256.Sum256([]byte(strings.Join(ids, "\n") + "\n"))
	type entry struct {
		ID     string `json:"entity_id"`
		Source string `json:"source"`
	}
	doc := struct {
		Version  int     `json:"version"`
		Digest   string  `json:"ids_sha256"`
		Entities []entry `json:"entities"`
	}{Version: 1, Digest: hex.EncodeToString(digest[:])}
	for _, id := range ids {
		doc.Entities = append(doc.Entities, entry{id, "nhf008-number-contract-only"})
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return registry.Manifest{}, err
	}
	return registry.ParseManifest(raw)
}

// Contract fixture only: actual Auth/State/Action handlers, authority, Host,
// helper pilot and SQL. It does not qualify Run/startup or production scope.
// The only mutable target is a local number row; there is no physical driver.
func TestNHF008NumberServer(t *testing.T) {
	out, root := os.Getenv("HOMEWARD_NHF008_NUMBER_FIXTURE"), os.Getenv("HOMEWARD_NHF008_SOURCES")
	if out == "" {
		t.Skip("selected only by isolated number contract qualification")
	}
	if !filepath.IsAbs(out) || !filepath.IsAbs(root) {
		t.Fatal("fresh absolute number fixture/source required")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	candidate, err := bundle.CompileV2(root, "number.yaml")
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := helpers.Definitions(candidate)
	if err != nil || len(definitions) != 1 || definitions[0].Kind() != helpers.KindNumber {
		t.Fatal("one number definition required", err)
	}
	definition := definitions[0]
	alias, err := definition.Alias()
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := nhf008NumberManifest(alias)
	if err != nil {
		t.Fatal(err)
	}
	rig := nha162NewRPCRig(t)
	rig.server.Close()
	ctx, signals := signal.NotifyContext(rig.ctx, os.Interrupt, syscall.SIGTERM)
	defer signals()
	identity, err := registry.NewRepository(rig.store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	helperRepository, err := helpers.NewRepository(rig.store)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nhf008-number-contract-only")
	key, err := helpers.ConfiguredSourceKey(alias)
	if err != nil {
		t.Fatal(err)
	}
	aliasText, _ := alias.Value()
	binding, err := identity.ConfigureBinding(ctx, instance, key, aliasText)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := helperRepository.Bind(ctx, binding.Entity, definition)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := helperRepository.Restore(ctx, handle, definition); err != nil {
		t.Fatal(err)
	}
	entries := map[string]nhf008Helper{aliasText: {definition, handle}}
	initial, err := nhf008HelperSnapshots(ctx, helperRepository, entries)
	if err != nil {
		t.Fatal(err)
	}
	sequence, err := sequencer.New(sequencer.RealClock{}, 16384)
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.AttachProjection(ctx, sequence); err != nil {
		t.Fatal(err)
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(identity, sequence, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.AttachAuthorization(rig.coordinator.authority); err != nil {
		t.Fatal(err)
	}
	commands, err := integrations.NewDurableCommandService(ctx, host, nha094Generation{}, rig.store)
	if err != nil {
		t.Fatal(err)
	}
	joined := true
	stopCommands := sync.OnceFunc(func() {
		stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := commands.Close(stop); err != nil {
			joined = false
			t.Error(err)
		}
	})
	defer stopCommands()
	buttons, err := scope.RetainedHelperButtonPolicy()
	if err != nil {
		t.Fatal(err)
	}
	factory, err := helpers.NewPilotFactory(helperRepository, definitions, buttons)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := integrations.NewPilotSpec(instance, factory)
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := integrations.NewSupervisor(host, []integrations.PilotSpec{spec}, integrations.DefaultRetryPolicy(), integrations.RealTimer{}, integrations.RandomJitter{})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	stopSupervisor := sync.OnceFunc(func() {
		stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := supervisor.Stop(stop); err != nil {
			joined = false
			t.Error(err)
		}
	})
	defer stopSupervisor()
	stamp, err := view.NewConfigStamp(7, sha256.Sum256([]byte("NHF008 isolated number contract configuration")))
	if err != nil {
		t.Fatal(err)
	}
	configuration := nha050ViewConfig{stamp}
	dashboard, err := nhf005DashboardFixture()
	if err != nil || dashboard == nil {
		t.Fatal("explicit number dashboard required", err)
	}
	dashboard.authority, dashboard.activation = rig.coordinator.authority, configuration
	account, _ := rig.admin.Value()
	if err := dashboard.install(account, map[string]domain.EntityID{aliasText: binding.Entity}); err != nil {
		t.Fatal(err)
	}
	grants := "  - scope: {family: dashboards, operation: read}\n    objects: {dashboards: {own: {}}}\n"
	for _, item := range []struct{ family, operation string }{{"state_registry", "read"}, {"actions", "read"}, {"actions", "write"}} {
		grants += fmt.Sprintf("  - scope: {family: %s, operation: %s}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n", item.family, item.operation)
	}
	nha162SetPolicy(t, rig, rig.admin, grants)
	plan, err := NewPublicPlan(nil, nil, accountTransportAudit{store: rig.store})
	if err != nil {
		t.Fatal(err)
	}
	if err := dashboard.register(plan); err != nil {
		t.Fatal(err)
	}
	core, err := prepareCoreRPC(ctx, rig.coordinator, sequence, identity, configuration, commands)
	if err != nil {
		t.Fatal(err)
	}
	if err := registerCoreRPCRoutes(plan, core); err != nil {
		t.Fatal(err)
	}
	plan, _, err = composeSessionsWithCoordinator(plan, rig.store, rig.coordinator)
	if err != nil {
		t.Fatal(err)
	}
	settings := config.DefaultPublicSettings()
	compiled, err := compilePublicPlan(plan, settings)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(compiled.handler(settings, newPublicAdmissions()))
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	pair, err := tls.LoadX509KeyPair(filepath.Join(data, nha181CertificateName), filepath.Join(data, nha181PrivateKeyName))
	if err != nil {
		t.Fatal(err)
	}
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}
	server.StartTLS()
	stopServer := sync.OnceFunc(func() { server.CloseClientConnections(); server.Close() })
	defer stopServer()
	client := nha181Client(roots)
	defer client.CloseIdleConnections()
	const login, password = "NHA162 Admin", "NHA162 synthetic admin password canary"
	tokens := nha162RunLogin(t, ctx, authv1connect.NewAuthServiceClient(client, server.URL), login, password)
	deadline := time.Now().Add(10 * time.Second)
	for {
		page, err := statev1connect.NewStateServiceClient(client, server.URL).OpenView(ctx, nha141BearerRequest(&statewire.OpenViewRequest{Filter: &statewire.ViewFilter{}, PageSize: 16}, tokens.AccessToken))
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Msg.Entities) == 1 && page.Msg.Entities[0].State.GetHelper() != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("number pilot not published")
		}
		time.Sleep(20 * time.Millisecond)
	}
	calls := new(nhf006Calls)
	transport := client.Transport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	proxy := calls.proxy(t, server.URL, transport, pair, nhf006Ack, true)
	stopProxy := sync.OnceFunc(func() { proxy.CloseClientConnections(); proxy.Close() })
	defer stopProxy()
	defer func() {
		final, err := nhf008HelperSnapshots(context.Background(), helperRepository, entries)
		if err != nil {
			t.Error(err)
		}
		count := host.CommandDriverEntries()
		stopProxy()
		stopServer()
		stopSupervisor()
		stopCommands()
		calls.mu.Lock()
		defer calls.mu.Unlock()
		nhf003WritePrivate(t, out, "result.json", map[string]any{"server_joined": joined, "fixture_mode": "number-contract-only", "entity_count": 1,
			"submit_ids": calls.submit, "get_ids": calls.get, "watch_ids": calls.watch, "native_login_calls": calls.logins.Load(), "native_revokes": calls.revokes.Load(),
			"fake_driver_calls": 0, "command_driver_entries": count, "helper_sql_initial": initial, "helper_sql_final": final,
			"dashboard_get_calls": dashboard.gets.Load(), "dashboard_watch_calls": dashboard.watches.Load(), "real_device_writes": 0})
	}()
	certificate, err := os.ReadFile(filepath.Join(data, nha181CertificateName))
	if err != nil {
		t.Fatal(err)
	}
	nhf003WritePrivate(t, out, "ready.json", map[string]any{"mode": "synthetic-only", "fixture_mode": "number-contract-only", "origin": proxy.URL, "ca_pem": string(certificate), "login_name": login, "password": password, "entity_count": 1})
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var previous uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(out, "stop")); err == nil {
				return
			}
			command, err := nhf008ReadControl(out, previous)
			if err != nil {
				t.Fatal(err)
			}
			if command == nil {
				continue
			}
			if command.Action != nhf008SQLRaceNumber && command.Action != nhf008ReadSQL {
				t.Fatal("unsupported number-only control")
			}
			if command.Action == nhf008SQLRaceNumber {
				current, err := helperRepository.Restore(ctx, handle, definition)
				if err != nil {
					t.Fatal(err)
				}
				value, err := helpers.Number(1)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := helperRepository.CompareAndSwap(ctx, handle, definition, current.Revision(), value); err != nil {
					t.Fatal(err)
				}
			}
			previous = command.Sequence
			ack := map[string]any{"sequence": previous, "action": command.Action}
			if command.Action == nhf008ReadSQL {
				rows, err := nhf008HelperSnapshots(ctx, helperRepository, entries)
				if err != nil {
					t.Fatal(err)
				}
				ack["helpers"] = rows
			}
			nhf003WritePrivate(t, out, fmt.Sprintf("ack-%02d.json", previous), ack)
		}
	}
}
