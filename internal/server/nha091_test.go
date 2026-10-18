package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
)

var nha091Aliases = []string{
	"climate.neviweb130_climate_chambre_eugenie", "climate.neviweb130_climate_chambre_principale",
	"climate.neviweb130_climate_chambre_sous_sol", "climate.neviweb130_climate_living_room",
	"climate.neviweb130_climate_plancher_cuisine", "climate.neviweb130_climate_salle_a_manger",
	"climate.neviweb130_climate_salle_de_bain", "climate.neviweb130_climate_sous_sol",
	"switch.neviweb130_switch_chauffe_eau", "sensor.neviweb130_daily_requests", "sensor.neviweb130_sensor_tableau_elec",
}

func TestNHA091ConfiguredRunComposesElevenReadOnlySources(t *testing.T) {
	var requests, actions atomic.Int32
	var expectedCookie atomic.Value
	expectedCookie.Store("sid=fixture-cookie")
	seen := make(chan struct{}, 32)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		select {
		case seen <- struct{}{}:
		default:
		}
		if r.Method == http.MethodPut {
			actions.Add(1)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.URL.Path != "/api/login" && (r.Header.Get("Session-Id") != "fixture-session" || r.Header.Get("Cookie") != expectedCookie.Load().(string)) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/api/login":
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "fixture-cookie"})
			_, _ = w.Write([]byte(`{"session":"fixture-session","account":{"id":123}}`))
		case r.URL.Path == "/api/locations":
			_, _ = w.Write([]byte(`[{"id":456}]`))
		case r.URL.Path == "/api/devices":
			var rows []string
			for i := 0; i < 11; i++ {
				if i == 9 {
					continue
				}
				model := 1123
				if i == 8 {
					model = 2151
				}
				rows = append(rows, fmt.Sprintf(`{"id":"device-%02d","signature":{"model":%d}}`, i, model))
			}
			_, _ = w.Write([]byte("[" + strings.Join(rows, ",") + "]"))
		case strings.HasSuffix(r.URL.Path, "/attribute"):
			if strings.Contains(r.URL.Path, "device-08") {
				_, _ = w.Write([]byte(`{"onOff":"off"}`))
			} else {
				_, _ = w.Write([]byte(`{"roomTemperature":{"value":20},"roomSetpoint":21,"roomSetpointMin":5,"roomSetpointMax":30}`))
			}
		case strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"status":"online"}`))
		case strings.HasSuffix(r.URL.Path, "/notifications"):
			_, _ = w.Write([]byte(`{"occupancyMode":"home"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	cluster := testpg.Start(t)
	dataDir := filepath.Join(t.TempDir(), "private-data")
	var yaml strings.Builder
	fmt.Fprintf(&yaml, "listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\nneviweb:\n  instance: nha091-test\n  base_url: %q\n  account_id: '123'\n  network_id: '456'\n  username_secret: username\n  password_secret: password\n  session_secret: session\n  bindings:\n", dataDir, cluster.Port, upstream.URL)
	for i, alias := range nha091Aliases {
		if i == 9 {
			fmt.Fprintf(&yaml, "    - alias: %q\n", alias)
		} else {
			fmt.Fprintf(&yaml, "    - alias: %q\n      device_id: device-%02d\n", alias, i)
		}
	}
	c, err := config.ParseWithEnv([]byte(yaml.String()), func(key string) (string, bool) { return testpg.Password(), key == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := c.PostgresSettings()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	secrets, err := store.BootstrapSecrets(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ name, value string }{{"username", "fixture-user"}, {"password", "fixture-password"}} {
		value, _ := storage.NewSecretValue([]byte(item.value))
		if err := secrets.RotateCredential(ctx, nha051ServerHandle(t, "nha091-test", item.name), value); err != nil {
			t.Fatal(err)
		}
	}
	store.Close()
	runCtx, stop := context.WithCancel(context.Background())
	defer stop()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var lc net.ListenConfig
	go func() { done <- Run(runCtx, c, lc.Listen, func(addr net.Addr) { ready <- addr }) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Run startup: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not listen")
	}
	for i := 0; i < 14; i++ {
		select {
		case <-seen:
		case <-time.After(10 * time.Second):
			t.Fatalf("initial provider inventory stopped after %d calls", i)
		}
	}
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Run did not stop")
	}
	if actions.Load() != 0 || requests.Load() != 14 {
		t.Fatalf("startup requests/actions=%d/%d", requests.Load(), actions.Load())
	}
	reopened, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	quota, err := storage.NewCloudAttemptQuota(reopened, "123", 5000, "America/Toronto")
	if err != nil {
		t.Fatal(err)
	}
	count, err := quota.Today(ctx)
	if err != nil || count != 14 {
		t.Fatalf("durable attempts=%d %v", count, err)
	}
	manifest, err := registry.ParseManifest(scope.ManifestV1())
	if err != nil {
		t.Fatal(err)
	}
	identities, err := registry.NewRepository(reopened, manifest)
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := identities.List(ctx)
	if err != nil || len(bindings) != 11 {
		t.Fatalf("configured identities=%d %v", len(bindings), err)
	}
	active, err := reopened.BootstrapSecrets(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	value, err := active.Read(ctx, nha051ServerHandle(t, "nha091-test", "session"))
	if err != nil {
		t.Fatal(err)
	}
	bytes, _ := value.Bytes()
	if !strings.Contains(string(bytes), `"state":"active"`) {
		t.Fatal("login session was not committed")
	}
	rotated, err := storage.NewSecretValue([]byte(`{"version":1,"state":"active","session_id":"fixture-session","cookie_header":"sid=fixture-after-restart","account_id":"123"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := active.RotateCredential(ctx, nha051ServerHandle(t, "nha091-test", "session"), rotated); err != nil {
		t.Fatal(err)
	}
	expectedCookie.Store("sid=fixture-after-restart")
	// A new normal Run must preserve and reuse the encrypted durable session.
	// It rechecks the network and reads the eleven sources, but never logs in
	// again or emits an action merely because the process restarted.
	restartCtx, restartStop := context.WithCancel(context.Background())
	defer restartStop()
	restartReady := make(chan net.Addr, 1)
	restartDone := make(chan error, 1)
	go func() { restartDone <- Run(restartCtx, c, lc.Listen, func(addr net.Addr) { restartReady <- addr }) }()
	select {
	case <-restartReady:
	case err := <-restartDone:
		t.Fatalf("Run restart: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("Run restart did not listen")
	}
	for i := 0; i < 13; i++ {
		select {
		case <-seen:
		case <-time.After(10 * time.Second):
			t.Fatalf("restart inventory stopped after %d calls", i)
		}
	}
	restartStop()
	select {
	case err := <-restartDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Run restart did not stop")
	}
	if actions.Load() != 0 || requests.Load() != 27 {
		t.Fatalf("restart requests/actions=%d/%d", requests.Load(), actions.Load())
	}
	count, err = quota.Today(ctx)
	if err != nil || count != 27 {
		t.Fatalf("durable attempts after restart=%d %v", count, err)
	}
}
