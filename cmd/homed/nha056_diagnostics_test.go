package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/server"
)

func TestNHA056HomedActualRunnerExpurgatesAllErrorPaths(t *testing.T) {
	const canary = "NHA056_HOMED_PASSWORD_URL_PATH_COORDS_TOKEN_CANARY"
	var out, diagnostics bytes.Buffer
	dependencies := homedDependencies{read: os.ReadFile, run: func(context.Context, config.Config, server.ListenFunc, func(net.Addr)) error {
		t.Fatal("unexpected runtime start")
		return nil
	}, listen: func(context.Context, string, string) (net.Listener, error) {
		t.Fatal("unexpected network")
		return nil, errors.New("synthetic")
	}}
	missing := filepath.Join(t.TempDir(), canary)
	if code := runHomed(context.Background(), []string{"-config", missing}, &out, &diagnostics, dependencies); code != 1 || !strings.Contains(diagnostics.String(), obs.LogCode_LOG_CODE_PROCESS_CONFIG_READ_FAILED.String()) || strings.Contains(diagnostics.String(), canary) {
		t.Fatal("actual file error was leaked or suppressed")
	}
	path := filepath.Join(t.TempDir(), "synthetic.yaml")
	if err := os.WriteFile(path, []byte("bad: ["+canary), 0600); err != nil {
		t.Fatal(err)
	}
	diagnostics.Reset()
	if code := runHomed(context.Background(), []string{"-config", path}, &out, &diagnostics, dependencies); code != 1 || !strings.Contains(diagnostics.String(), obs.LogCode_LOG_CODE_PROCESS_CONFIG_ADMISSION_FAILED.String()) || strings.Contains(diagnostics.String(), canary) {
		t.Fatal("actual YAML admission was leaked or suppressed")
	}
	t.Setenv("NHA056_SYNTHETIC_DB_PASSWORD", canary)
	text := fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n host: '127.0.0.1'\n port: 5432\n database: synthetic\n user: synthetic\n password_env: NHA056_SYNTHETIC_DB_PASSWORD\n sslmode: disable\n", t.TempDir())
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	for _, ready := range []bool{false, true} {
		diagnostics.Reset()
		out.Reset()
		dependencies.run = func(_ context.Context, _ config.Config, _ server.ListenFunc, callback func(net.Addr)) error {
			if ready {
				callback(&net.UnixAddr{Name: canary, Net: "unix"})
			}
			return &url.Error{Op: "synthetic", URL: canary, Err: errors.New(canary)}
		}
		if code := runHomed(context.Background(), []string{"-config", path}, &out, &diagnostics, dependencies); code != 1 {
			t.Fatal("runtime failure changed exit status")
		}
		expected := obs.LogCode_LOG_CODE_PROCESS_RUNTIME_START_FAILED
		if ready {
			expected = obs.LogCode_LOG_CODE_PROCESS_RUNTIME_FAILED
		}
		if !strings.Contains(diagnostics.String(), expected.String()) || strings.Contains(out.String()+diagnostics.String(), canary) {
			t.Fatal("actual runner leaked a provider error or address")
		}
	}
	diagnostics.Reset()
	out.Reset()
	dependencies.run = func(context.Context, config.Config, server.ListenFunc, func(net.Addr)) error { panic(canary) }
	if code := runHomed(context.Background(), []string{"-config", path}, &out, &diagnostics, dependencies); code != 1 || strings.Contains(out.String()+diagnostics.String(), canary) {
		t.Fatal("panic escaped the actual runner")
	}
}
