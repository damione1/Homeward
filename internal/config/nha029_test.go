package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func nha029Lookup(name string) (string, bool) {
	return "NHA029_private_env_value", name == "NHA029_DB_PASSWORD"
}

func nha029Combined(dataDir string) []byte {
	return []byte(fmt.Sprintf(`# exact operational comment and args
listen: '127.0.0.1:8123'
postgres:
  host: 127.0.0.1
  port: 5432
  database: homeward
  user: homeward
  password_env: NHA029_DB_PASSWORD
  sslmode: disable
data_dir: %q
workday:
  timezone: America/Toronto
external_workers:
  - instance: fixture
    executable: /bin/sh
    args: ["--opaque=NHA029_arg_canary"]
    secret_names: []
    bindings: []
`, dataDir))
}

func TestNHA029SingleDecoderSeparatesLaunchAndOperationalReplay(t *testing.T) {
	raw := nha029Combined(t.TempDir())
	if _, err := ParseWithEnv(raw, nha029Lookup); err != nil {
		t.Fatalf("legacy combined admission changed: %v", err)
	}
	// SQL replay must not resolve a stale launch-only password_env value.
	operational, err := ParseOperationalWithEnv(raw, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("operational replay resolved stale launch fields: %v", err)
	}
	if settings, ok := operational.WorkdaySettings(); !ok || !settings.Valid() {
		t.Fatal("explicit IANA Workday profile was lost")
	}
	if _, err := operational.PostgresSettings(); !errors.Is(err, ErrUninitialized) {
		t.Fatalf("replayed operational source supplied launch authority: %v", err)
	}
	if _, err := ParseWithEnv(raw, func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("legacy combined parser stopped validating launch secret reference")
	}
	if _, err := ParseBootstrapLaunchWithEnv(raw, nha029Lookup); err == nil {
		t.Fatal("launch sidecar admitted operational fields")
	}
	unknown := append(bytes.Clone(raw), []byte("unknown_future_field: true\n")...)
	if _, err := ParseOperationalWithEnv(unknown, nha029Lookup); !errors.Is(err, ErrInvalidYAML) {
		t.Fatalf("operational replay admitted an unknown key: %v", err)
	}
}

func TestNHA029StartupPathSelectionExactCaptureAndDefaultSidecar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "configuration.yaml")
	raw := nha029Combined(filepath.Join(dir, "private-data"))
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := LoadStartupPaths(path, DefaultBootstrapChoice(), nha029Lookup)
	if err != nil || !startup.Valid() || startup.Origin() != BootstrapOriginLegacyCombined {
		t.Fatalf("valid legacy source refused: %v", err)
	}
	physical, ok := startup.BootstrapPath()
	if !ok || physical != path+".bootstrap.yaml" {
		t.Fatal("default sidecar path is not stable")
	}
	operational, err := startup.CaptureWorkspace()
	if err != nil || !bytes.Equal(operational.Bytes(), raw) || operational.LogicalPath() != "configuration.yaml" {
		t.Fatalf("exact workspace capture failed: %v", err)
	}
	if err := operational.ConfirmPhysicalSource(); err != nil {
		t.Fatal(err)
	}
	if err := startup.PublishDefaultBootstrap(); err != nil {
		t.Fatalf("durable default sidecar: %v", err)
	}
	sidecar, err := os.ReadFile(physical)
	if err != nil || bytes.Contains(sidecar, []byte("NHA029_arg_canary")) ||
		bytes.Contains(sidecar, []byte("NHA029_private_env_value")) ||
		!bytes.Contains(sidecar, []byte("NHA029_DB_PASSWORD")) {
		t.Fatalf("generated launch sidecar contains operational or resolved secret data: %v", err)
	}
	if err := os.WriteFile(path, []byte("not: valid: yaml\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := operational.ConfirmPhysicalSource(); !errors.Is(err, ErrStartupSourceChanged) {
		t.Fatalf("path-backed CAS admitted changed bytes: %v", err)
	}
	restarted, err := LoadStartupPaths(path, DefaultBootstrapChoice(), nha029Lookup)
	if err != nil || !restarted.Valid() || restarted.Origin() != BootstrapOriginDefaultSidecar {
		t.Fatalf("dirty workspace prevented bootstrap lookup: %v", err)
	}
	if _, err := restarted.CaptureWorkspace(); err == nil {
		t.Fatal("dirty workspace admitted without explicit capture")
	}
	if _, err := restarted.LaunchConfig(); err != nil {
		t.Fatalf("sidecar did not retain launch settings: %v", err)
	}
	if err := restarted.PublishDefaultBootstrap(); err == nil {
		t.Fatal("existing sidecar was treated as a new publication")
	}
}

func TestNHA029ExplicitReadOnlyBootstrapAndDetachedSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "configuration.yaml")
	raw := nha029Combined(filepath.Join(dir, "private-data"))
	detached, err := NewDetachedStartup(raw, "configuration.yaml", nil, nha029Lookup)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 1
	captured, err := detached.CaptureWorkspace()
	if err != nil || bytes.Equal(captured.Bytes(), raw) {
		t.Fatalf("detached source aliased input bytes: %v", err)
	}
	if err := captured.ConfirmPhysicalSource(); err == nil {
		t.Fatal("detached source claimed physical CAS authority")
	}
	bootstrap, err := detached.BootstrapSource()
	if err != nil || !bootstrap.Valid() {
		t.Fatal(err)
	}
	explicitPath := filepath.Join(dir, "readonly-bootstrap.yaml")
	if err := os.WriteFile(explicitPath, bootstrap.Bytes(), 0400); err != nil {
		t.Fatal(err)
	}
	choice, err := ExistingBootstrap(explicitPath)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadStartupPaths(path, choice, nha029Lookup)
	if err != nil || loaded.Origin() != BootstrapOriginExplicitSidecar {
		t.Fatalf("fresh explicit sidecar required a workspace before DB admission: %v", err)
	}
	if err := loaded.PublishDefaultBootstrap(); err == nil {
		t.Fatal("explicit read-only sidecar was writable by startup")
	}
	if !strings.Contains(fmt.Sprintf("%+v", loaded), "REDACTED") ||
		strings.Contains(fmt.Sprintf("%+v", detached), "NHA029_arg_canary") {
		t.Fatal("source-bearing startup leaked through formatting")
	}
	if _, err := LoadStartupPaths(path, BootstrapChoice{}, nha029Lookup); err == nil {
		t.Fatal("zero bootstrap choice admitted")
	}
}
