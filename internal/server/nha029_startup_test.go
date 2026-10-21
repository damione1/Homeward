package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

// A Run fixture carries the exact YAML to the same admission path as main.
// The parsed Config remains useful for direct assertions, but never grants
// permission to serve without the source and its SQL generation.
type nha029RunFixture struct {
	config.Config
	startup config.StartupInputs
}

func nha029TestEnv(name string) (string, bool) {
	return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD"
}

func nha029RunFixtureFromRaw(t *testing.T, raw []byte) nha029RunFixture {
	t.Helper()
	parsed, err := config.ParseWithEnv(raw, nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	startup, err := config.NewDetachedStartup(raw, "configuration.yaml", nil, nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	return nha029RunFixture{Config: parsed, startup: startup}
}

func nha029RunStartupFromRaw(t *testing.T, raw []byte) config.StartupInputs {
	t.Helper()
	startup, err := config.NewDetachedStartup(raw, "configuration.yaml", nil, nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	return startup
}

func TestNHA029RunRejectsMissingSourceBeforeFactoryOrListener(t *testing.T) {
	called := false
	listen := func(context.Context, string, string) (net.Listener, error) {
		called = true
		return nil, errors.New("listener must not run")
	}
	factory := func(context.Context, *storage.Store) (RuntimePlan, error) {
		called = true
		return RuntimePlan{}, errors.New("factory must not run")
	}
	err := RunWithStartupFactory(context.Background(), config.StartupInputs{}, listen, func(net.Addr) { called = true }, factory)
	if !errors.Is(err, ErrInvalidContext) || called {
		t.Fatalf("missing source reached composition: err=%v called=%t", err, called)
	}
	type nilContext struct{ context.Context }
	var typedNil *nilContext
	validSource := nha029RunStartupFromRaw(t, []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: 1\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n", shortPrivateDataDir(t))))
	err = RunWithStartupFactory(typedNil, validSource, listen, func(net.Addr) { called = true }, factory)
	if !errors.Is(err, ErrInvalidContext) || called {
		t.Fatalf("typed-nil context reached composition: err=%v called=%t", err, called)
	}
}

func TestNHA029RunAdmitsSQLBeforeFactoryAndReplaysDirtyWorkspace(t *testing.T) {
	cluster := testpg.Start(t)
	workspace := t.TempDir()
	path := filepath.Join(workspace, "configuration.yaml")
	dataDir := shortPrivateDataDir(t)
	raw := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n# exact source comment\n", dataDir, cluster.Port))
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	first, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	stopAtFactory := errors.New("observed admitted SQL generation")
	var firstRevision storage.ConfigGenerationRevision
	var firstHash [32]byte
	factoryCalls, listenerCalls, readyCalls := 0, 0, 0
	listen := func(context.Context, string, string) (net.Listener, error) {
		listenerCalls++
		return nil, errors.New("listener reached after factory refusal")
	}
	ready := func(net.Addr) { readyCalls++ }
	factory := func(ctx context.Context, store *storage.Store) (RuntimePlan, error) {
		factoryCalls++
		active, found, err := store.ReadActiveConfig(ctx)
		if err != nil || !found || active.ManifestFormat() != storage.ConfigManifestCompiledV2 ||
			active.Availability() != storage.ConfigGenerationComplete {
			t.Fatalf("factory preceded admitted active generation: found=%t err=%v", found, err)
		}
		bootstrap, found, err := store.ReadActiveBootstrap(ctx)
		if err != nil || !found || bootstrap.Path() != "bootstrap.launch.yaml" {
			t.Fatalf("factory preceded admitted bootstrap: found=%t err=%v", found, err)
		}
		if factoryCalls == 1 {
			firstRevision, firstHash = active.Revision(), active.ManifestSHA256()
		} else if active.Revision() != firstRevision || active.ManifestSHA256() != firstHash {
			t.Fatal("dirty workspace changed the SQL-authoritative generation")
		}
		return RuntimePlan{}, stopAtFactory
	}
	if err := RunWithStartupFactory(context.Background(), first, listen, ready, factory); !errors.Is(err, stopAtFactory) {
		t.Fatalf("first startup failed before SQL admission: %v", err)
	}
	if factoryCalls != 1 || listenerCalls != 0 || readyCalls != 0 {
		t.Fatalf("first startup order: factory=%d listener=%d ready=%d", factoryCalls, listenerCalls, readyCalls)
	}
	if err := os.WriteFile(path, []byte("not: valid: yaml\n"), 0600); err != nil {
		t.Fatal(err)
	}
	restarted, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		t.Fatalf("dirty workspace blocked bootstrap locator: %v", err)
	}
	if err := RunWithStartupFactory(context.Background(), restarted, listen, ready, factory); !errors.Is(err, stopAtFactory) {
		t.Fatalf("SQL replay failed before factory: %v", err)
	}
	if factoryCalls != 2 || listenerCalls != 0 || readyCalls != 0 {
		t.Fatalf("replay order: factory=%d listener=%d ready=%d", factoryCalls, listenerCalls, readyCalls)
	}
	if err := os.Remove(filepath.Join(dataDir, ".env")); err != nil {
		t.Fatal(err)
	}
	if err := RunWithStartupFactory(context.Background(), restarted, listen, ready, factory); err == nil || errors.Is(err, stopAtFactory) {
		t.Fatalf("missing master key admitted runtime: %v", err)
	}
	if factoryCalls != 2 || listenerCalls != 0 || readyCalls != 0 {
		t.Fatalf("missing key reached composition: factory=%d listener=%d ready=%d", factoryCalls, listenerCalls, readyCalls)
	}
}
