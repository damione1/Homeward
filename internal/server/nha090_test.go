package server

import (
	"context"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/energy"
	"github.com/damione1/Homeward/internal/hilo"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
)

func TestNHA090RunUnitCatalogKeepsNotificationCountLocal(t *testing.T) {
	units, err := newRunUnitCatalog(true)
	if err != nil {
		t.Fatal(err)
	}
	unit, err := units.Parse("{notification}")
	if err != nil {
		t.Fatal(err)
	}
	if dimension, err := unit.Dimension(); err != nil || dimension != domain.DimensionCount {
		t.Fatalf("Hilo notification unit: %v %v", dimension, err)
	}
	if _, err := domain.NewUnitCatalog().Parse("{notification}"); err == nil {
		t.Fatal("Run-local Hilo unit escaped into the domain default")
	}
}

func TestNHA090ConfiguredRunPublishesPowerThroughHiloLeftAndClosesGap(t *testing.T) {
	const locationHiloID = "urn:hilo:synthetic:location"
	const meterHiloID = "urn:hilo:synthetic:meter"
	var requests, writes, sseOpens, auxiliaryGets atomic.Int32
	emitSSE := make(chan struct{})
	breakSSE := make(chan struct{})
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			writes.Add(1)
			http.Error(w, "read-only fixture", http.StatusForbidden)
			return
		}
		switch {
		case r.URL.Path == "/Automation/v1/api/Locations" && r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `[{"id":10000,"locationHiloId":"`+locationHiloID+`"}]`)
		case r.URL.Path == "/DeviceHub/negotiate" && r.Method == http.MethodPost:
			if r.Header.Get("Ocp-Apim-Subscription-Key") != "" {
				_, _ = io.WriteString(w, `{"url":"`+upstream.URL+`/DeviceHub","accessToken":"synthetic-edge-token"}`)
			} else if r.Header.Get("Authorization") == "Bearer synthetic-edge-token" {
				_, _ = io.WriteString(w, `{"connectionToken":"synthetic-connection"}`)
			} else {
				http.Error(w, "bad synthetic negotiation", http.StatusUnauthorized)
			}
		case r.URL.Path == "/DeviceHub" && r.Method == http.MethodGet && r.URL.Query().Get("id") == "synthetic-connection":
			connection, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer connection.CloseNow()
			_, handshake, err := connection.Read(r.Context())
			if err != nil || string(handshake) != "{\"protocol\":\"json\",\"version\":1}\x1e" {
				return
			}
			if connection.Write(r.Context(), websocket.MessageText, []byte("{}\x1e")) != nil {
				return
			}
			_, subscribe, err := connection.Read(r.Context())
			if err != nil || !strings.Contains(string(subscribe), "SubscribeToLocation") {
				return
			}
			inventory := `{"type":1,"target":"DeviceListInitialValuesReceived","arguments":[[{"id":1,"hiloId":"` + meterHiloID + `","type":"Meter","modelNumber":"","supportedAttributesList":["Power","Disconnected"]}]]}` + "\x1e"
			if connection.Write(r.Context(), websocket.MessageText, []byte(inventory)) != nil {
				return
			}
			<-r.Context().Done()
		case r.URL.Path == "/api/digital-twin/v3/graphql" && r.Method == http.MethodPost:
			body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil {
				http.Error(w, "bad synthetic query", http.StatusBadRequest)
				return
			}
			if r.Header.Get("Accept") == "text/event-stream" {
				sseOpens.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.(http.Flusher).Flush()
				select {
				case <-emitSSE:
				case <-r.Context().Done():
					return
				}
				for _, power := range []float64{0.5, 0.6} {
					at := time.Now().UTC().Format(time.RFC3339Nano)
					_, _ = fmt.Fprintf(w, "data: {\"data\":{\"onAnyDeviceUpdated\":{\"deviceType\":\"BasicSmartMeter\",\"locationHiloId\":%q,\"transmissionTime\":%q,\"device\":{\"hiloId\":%q,\"connectionStatus\":1,\"power\":{\"value\":%v,\"kind\":\"KILOWATT\"}}}}}\n\n", locationHiloID, at, meterHiloID, power)
					w.(http.Flusher).Flush()
					select {
					case <-time.After(150 * time.Millisecond):
					case <-r.Context().Done():
						return
					}
				}
				select {
				case <-breakSSE:
				case <-r.Context().Done():
				}
				return
			}
			if !strings.Contains(string(body), "query getLocation") {
				_, _ = io.WriteString(w, `{"errors":[{"message":"PersistedQueryNotFound"}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":{"getLocation":{"hiloId":"`+locationHiloID+`","devices":[{"deviceType":"BasicSmartMeter","hiloId":"`+meterHiloID+`","connectionStatus":1,"power":{"value":0.355,"kind":"KILOWATT"}}]}}}`)
		case strings.Contains(r.URL.Path, "/Notifications/Notifications/Locations/") && r.Method == http.MethodGet:
			auxiliaryGets.Add(1)
			_, _ = io.WriteString(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/Weather") && r.Method == http.MethodGet:
			auxiliaryGets.Add(1)
			_, _ = io.WriteString(w, `[{"temperature":18.5}]`)
		default:
			http.Error(w, "unconfigured synthetic Hilo path", http.StatusNotFound)
		}
	}))
	defer func() { upstream.CloseClientConnections(); upstream.Close() }()
	cluster := testpg.Start(t)
	dataDir := shortPrivateDataDir(t)
	var input strings.Builder
	fmt.Fprintf(&input, "listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\nhilo:\n  instance: nha090-run-active\n  account_id: synthetic-account\n  location_id: 10000\n  location_hilo_id: %s\n  credential_secret: synthetic-credential\n  daily_limit: 500\n  lease_window_seconds: 180\n  energy_max_gap_seconds: 300\n  api_base_url: %q\n  platform_base_url: %q\n  bindings:\n", dataDir, cluster.Port, locationHiloID, upstream.URL, upstream.URL)
	deviceID := 2
	for _, alias := range hilo.RequiredDeviceAliases() {
		id := deviceID
		deviceID++
		if alias == "sensor.meter00_power" {
			id = 1
		}
		fmt.Fprintf(&input, "    - {alias: %q, device_id: %d}\n", alias, id)
	}
	configured, err := config.ParseWithEnv([]byte(input.String()), func(name string) (string, bool) {
		return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD"
	})
	if err != nil {
		t.Fatal(err)
	}
	pg, err := configured.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), pg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	secrets, err := store.BootstrapSecrets(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	envelope := fmt.Sprintf(`{"version":1,"access":"synthetic-access","refresh":"synthetic-refresh","expires_at":%q,"scope":"openid offline_access https://HiloDirectoryB2C.onmicrosoft.com/hiloapis/user_impersonation","profile":"hilo-android-b2c-v1"}`, time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano))
	secret, err := storage.NewSecretValue([]byte(envelope))
	if err != nil {
		t.Fatal(err)
	}
	if err := secrets.RotateCredential(context.Background(), nha051ServerHandle(t, "nha090-run-active", "synthetic-credential"), secret); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var listener net.ListenConfig
	go func() { done <- Run(runCtx, configured, listener.Listen, func(address net.Addr) { ready <- address }) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("active Run exited before ready: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("active Hilo Run did not reach ready")
	}
	close(emitSSE)
	manifest, err := registry.ParseManifest(scope.ManifestV1())
	if err != nil {
		t.Fatal(err)
	}
	identities, err := registry.NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	source, err := identities.LookupAlias(context.Background(), "sensor.meter00_power")
	if err != nil {
		t.Fatal(err)
	}
	output, err := identities.LookupAlias(context.Background(), "sensor.hilo_energy_total")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := energy.HiloLeftV1(source.Entity, output.Entity, 300*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	counters, err := energy.NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var before energy.Checkpoint
	positiveTotal := false
	for time.Now().Before(deadline) {
		before, err = counters.Load(context.Background(), profile)
		if err == nil && before.Revision() >= 2 && before.Coverage() == energy.CoverageContinuous {
			numerator, _, fractionErr := before.TotalFraction()
			if fractionErr == nil {
				if quantity, valid := new(big.Int).SetString(numerator, 10); valid && quantity.Sign() > 0 {
					positiveTotal = true
					break
				}
			}
		}
		select {
		case runErr := <-done:
			t.Fatalf("active Run stopped before Hilo energy: %v", runErr)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if before.Revision() < 2 || before.Coverage() != energy.CoverageContinuous || !positiveTotal {
		t.Fatalf("Hilo power did not reach the same NHA-108 left engine: revision=%d coverage=%d diagnostic=%d err=%v requests=%d", before.Revision(), before.Coverage(), before.Diagnostic(), err, requests.Load())
	}
	close(breakSSE)
	deadline = time.Now().Add(5 * time.Second)
	var after energy.Checkpoint
	for time.Now().Before(deadline) {
		after, err = counters.Load(context.Background(), profile)
		if err == nil && after.Revision() > before.Revision() && after.Coverage() == energy.CoverageDegraded {
			break
		}
		select {
		case runErr := <-done:
			t.Fatalf("active Run stopped before loss fence: %v", runErr)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if after.Revision() <= before.Revision() || after.Coverage() != energy.CoverageDegraded {
		t.Fatalf("SSE loss was not fenced through NHA-108: before=%d after=%d coverage=%d diagnostic=%d err=%v", before.Revision(), after.Revision(), after.Coverage(), after.Diagnostic(), err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("active Run failed to join cloud, energy and Store: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("active Hilo Run did not join after cancellation")
	}
	if writes.Load() != 0 || sseOpens.Load() != 1 || auxiliaryGets.Load() > 2 {
		t.Fatalf("read-only Hilo request profile changed: writes=%d SSE=%d auxiliary GET=%d", writes.Load(), sseOpens.Load(), auxiliaryGets.Load())
	}
}

func TestNHA090ConfiguredRunProjectsAllRetainedAliasesWithoutLogin(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer upstream.Close()
	cluster := testpg.Start(t)
	dataDir := shortPrivateDataDir(t)
	var input strings.Builder
	fmt.Fprintf(&input, "listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\nhilo:\n  instance: nha090-run\n  account_id: synthetic-account\n  location_id: 10000\n  location_hilo_id: urn:hilo:synthetic:location\n  credential_secret: synthetic-absent\n  daily_limit: 500\n  lease_window_seconds: 180\n  energy_max_gap_seconds: 300\n  api_base_url: %q\n  platform_base_url: %q\n  bindings:\n", dataDir, cluster.Port, upstream.URL, upstream.URL)
	for i, alias := range hilo.RequiredDeviceAliases() {
		fmt.Fprintf(&input, "    - {alias: %q, device_id: %d}\n", alias, i+1)
	}
	configured, err := config.ParseWithEnv([]byte(input.String()), func(name string) (string, bool) {
		return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD"
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var listener net.ListenConfig
	go func() { done <- Run(runCtx, configured, listener.Listen, func(address net.Addr) { ready <- address }) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Run exited before ready: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("Hilo Run did not reach ready")
	}
	pg, err := configured.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), pg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manifest, err := registry.ParseManifest(scope.ManifestV1())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := registry.NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	aliases := hilo.RetainedAliases()
	if len(aliases) != 58 {
		t.Fatalf("retained profile: %d", len(aliases))
	}
	for _, name := range aliases {
		alias, err := repository.LookupAlias(context.Background(), name)
		if err != nil || !alias.Current || alias.Retired {
			t.Fatalf("Run did not bind %s before ready: %+v %v", name, alias, err)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run did not join the read-only Hilo pilot: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Hilo Run did not join after cancellation")
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("startup or shutdown contacted a cloud endpoint without a credential: %d requests", got)
	}
}
