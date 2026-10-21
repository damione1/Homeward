package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
)

// These fixtures exercise the delivered source-bearing NHA-029 startup API.
func nha137StartupFixture(t *testing.T, raw, launch []byte) config.StartupInputs {
	t.Helper()
	startup, err := config.NewDetachedStartup(raw, "configuration.yaml", launch, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	return startup
}

func nha137LaunchFixture(port int, data string, selector string) []byte {
	raw := fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n", data, port)
	if selector != "" {
		raw += "scope_revision: " + selector + "\n"
	}
	return []byte(raw)
}

func TestNHA137DefaultRunUsesCurrentColdScopeOverHistoricalOperationalScope(t *testing.T) {
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	// Operational SQL deliberately retains the amendment spelling throughout
	// all cold starts. Only the separate current launch source chooses scope.
	historical := nha137LaunchFixture(cluster.Port, data, "ecobee_mvp_v1")
	parsed, err := config.ParseWithEnv(historical, func(string) (string, bool) { return testpg.Password(), true })
	if err != nil {
		t.Fatal(err)
	}
	settings, err := parsed.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	manifest, err := registry.LoadManifest(scope.EcobeeMVPV1())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := registry.NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha137-run-synthetic")
	key, _ := integrations.NewSourceKey("synthetic-ecobee", "thermostat")
	ecobee, err := repo.ConfigureBinding(ctx, instance, key, "climate.home")
	if err != nil {
		t.Fatal(err)
	}
	otherKey, _ := integrations.NewSourceKey("synthetic-switch", "relay")
	other, err := repo.ConfigureBinding(ctx, instance, otherKey, "switch.chauffe_eau")
	if err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{"", "ecobee_mvp_v1", "v1", "ecobee_mvp_v1"} {
		startup := nha137StartupFixture(t, historical, nha137LaunchFixture(cluster.Port, data, selector))
		runCtx, stop := context.WithCancel(ctx)
		done, ready, observed := make(chan error, 1), make(chan net.Addr, 1), make(chan *observationRuntime, 1)
		go func() {
			done <- runWithDependencies(runCtx, startup, (&net.ListenConfig{}).Listen, func(address net.Addr) { ready <- address }, nil, runDependencies{observeRuntime: func(actual *observationRuntime) { observed <- actual }})
		}()
		select {
		case <-ready:
		case err := <-done:
			stop()
			t.Fatalf("cold Run %q failed: %v", selector, err)
		case <-ctx.Done():
			stop()
			t.Fatal(ctx.Err())
		}
		actual := <-observed
		image, err := actual.sequence.Capture()
		if err != nil {
			stop()
			<-done
			t.Fatal(err)
		}
		_, hasEcobee := image.Registry[ecobee.Entity]
		_, hasOther := image.Registry[other.Entity]
		wantEcobee := selector == "ecobee_mvp_v1"
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("Run failed to join", ctx.Err())
		}
		if hasEcobee != wantEcobee || !hasOther || len(image.Registry) != 1+boolToNHA137Int(wantEcobee) {
			t.Fatalf("Run %q projected wrong cold scope: ecobee=%t other=%t len=%d", selector, hasEcobee, hasOther, len(image.Registry))
		}
		if got, err := repo.Resolve(ctx, instance, []integrations.SourceKey{key}); err != nil || len(got) != 1 || got[0] != ecobee {
			t.Fatalf("cold start changed durable Ecobee identity: %v %v", got, err)
		}
	}
}

func boolToNHA137Int(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestNHA137RunFactoryCannotReplaceColdScopeWithBaselineRepository(t *testing.T) {
	cluster := testpg.Start(t)
	raw := nha137LaunchFixture(cluster.Port, shortPrivateDataDir(t), "ecobee_mvp_v1")
	startup := nha137StartupFixture(t, raw, raw)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	configured, ready := false, false
	err := RunWithStartupFactory(ctx, startup, (&net.ListenConfig{}).Listen, func(net.Addr) { ready = true }, func(_ context.Context, store *storage.Store) (RuntimePlan, error) {
		manifest, err := registry.LoadManifest(scope.BaselineV1())
		if err != nil {
			return RuntimePlan{}, err
		}
		repo, err := registry.NewRepository(store, manifest)
		if err != nil {
			return RuntimePlan{}, err
		}
		return RuntimePlan{Identity: repo, Configure: func(context.Context, *integrations.Manager, *storage.Store) error { configured = true; return nil }}, nil
	})
	if !errors.Is(err, registry.ErrOutsideManifest) || configured || ready {
		t.Fatalf("factory replaced cold scope: err=%v configured=%t ready=%t", err, configured, ready)
	}
}
