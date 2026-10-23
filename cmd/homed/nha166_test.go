package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/server"
)

func TestNHA166CLIObservationReachesOnlyTrustedStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n host: '127.0.0.1'\n port: 1\n database: synthetic\n user: synthetic\n password_env: NHA166_SYNTHETIC_PASSWORD\n sslmode: disable\n", t.TempDir())
	t.Setenv("NHA166_SYNTHETIC_PASSWORD", "synthetic-not-connected")
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	var out, diagnostics bytes.Buffer
	called := false
	deps := homedDependencies{runStartup: func(_ context.Context, startup config.StartupInputs, _ server.ListenFunc, _ func(net.Addr)) error {
		called = true
		if !startup.Valid() || !startup.MQTTReadOnlyObservation() {
			t.Fatal("CLI lost guard")
		}
		return nil
	}, listen: func(context.Context, string, string) (net.Listener, error) {
		t.Fatal("unexpected network")
		return nil, nil
	}}
	if code := runHomed(context.Background(), []string{"-config", path, "-observe-mqtt-readonly"}, &out, &diagnostics, deps); code != 0 || !called {
		t.Fatal("operator flag did not reach startup", code)
	}
	called = false
	deps.runStartup = nil
	deps.read = os.ReadFile
	deps.run = func(context.Context, config.Config, server.ListenFunc, func(net.Addr)) error {
		called = true
		return nil
	}
	if code := runHomed(context.Background(), []string{"-config", path, "-observe-mqtt-readonly"}, &out, &diagnostics, deps); code != 2 || called {
		t.Fatal("legacy startup silently ignored immutable guard", code)
	}
}
