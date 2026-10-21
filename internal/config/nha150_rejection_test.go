package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func nha150CaptureInputs(t *testing.T) (StartupInputs, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "configuration.yaml")
	raw := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: 1\n  database: postgres\n  user: synthetic\n  password_env: NHA150_SYNTHETIC_PASSWORD\n  sslmode: disable\n", filepath.Join(dir, "private-data")))
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := LoadStartupPaths(path, DefaultBootstrapChoice(), func(name string) (string, bool) {
		return "synthetic-not-connected", name == "NHA150_SYNTHETIC_PASSWORD"
	})
	if err != nil {
		t.Fatal(err)
	}
	return startup, path
}
func TestNHA150CapturedSemanticRejectionCarriesActualDigestAndRejectsZero(t *testing.T) {
	var zero WorkspaceRejection
	if _, ok := zero.SourceSHA256(); ok {
		t.Fatal("zero rejection forged captured provenance")
	}
	var absent *WorkspaceRejection
	if _, ok := absent.SourceSHA256(); ok {
		t.Fatal("nil rejection forged captured provenance")
	}
	for _, test := range []struct {
		name string
		raw  []byte
	}{
		{"syntax", []byte("# NHA150_PRIVATE_CANARY\nworkday: [\n")},
		{"multiple_documents", []byte("external_workers: []\n---\nexternal_workers: []\n")},
		{"invalid_embedded_zone", []byte("workday: {timezone: 'NHA150_PRIVATE_CANARY/unknown'}\n")},
	} {
		t.Run(test.name, func(t *testing.T) {
			startup, path := nha150CaptureInputs(t)
			if err := os.WriteFile(path, test.raw, 0600); err != nil {
				t.Fatal(err)
			}
			_, err := startup.CaptureWorkspace()
			var rejected *WorkspaceRejection
			if !errors.As(err, &rejected) {
				t.Fatal("positive parser refusal lost its captured provenance")
			}
			hash, ok := rejected.SourceSHA256()
			if !ok || hash != sha256.Sum256(test.raw) {
				t.Fatal("parser refusal borrowed another observation digest")
			}
			if bytes.Contains([]byte(fmt.Sprintf("%v %#v", err, err)), []byte("NHA150_PRIVATE_CANARY")) {
				t.Fatal("rejection formatted private source")
			}
		})
	}
}
func TestNHA150AmbiguousWorkerFileAvailabilityIsNeverCachedAsSemanticRejection(t *testing.T) {
	startup, path := nha150CaptureInputs(t)
	workerPath := filepath.Join(filepath.Dir(path), "synthetic-worker")
	raw := []byte(fmt.Sprintf("external_workers:\n- instance: nha150-synthetic\n  executable: %q\n", workerPath))
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := startup.CaptureWorkspace()
	var rejected *WorkspaceRejection
	if err == nil || errors.As(err, &rejected) {
		t.Fatal("missing local worker was turned into a permanent semantic refusal")
	}
	// Only create the local executable input; it is never started by this test.
	if err := os.WriteFile(workerPath, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	source, err := startup.CaptureWorkspace()
	if err != nil || source.SHA256() != sha256.Sum256(raw) {
		t.Fatal("unchanged source could not recover after local file became available")
	}
}

func TestNHA150BootstrapAbsenceIsDistinctFromUnsafeAndExplicitSources(t *testing.T) {
	for _, name := range []string{"missing_default_legacy_valid", "absent_explicit_refused", "default_symlink_refused", "published_sidecar_reloads_protected"} {
		t.Run(name, func(t *testing.T) {
			startup, path := nha150CaptureInputs(t)
			bootstrap, present := startup.BootstrapPath()
			if !present || startup.Origin() != BootstrapOriginLegacyCombined {
				t.Fatal("actual missing default did not select valid combined source")
			}
			switch name {
			case "missing_default_legacy_valid":
				_, err := readStartupFile(bootstrap, true)
				if !errors.Is(err, os.ErrNotExist) || !errors.Is(err, ErrStartupSource) {
					t.Fatal("leaf absence lost its safe typed sentinel")
				}
				if strings.Contains(err.Error(), bootstrap) {
					t.Fatal("missing leaf error exposed its selected private path")
				}
				if _, err := startup.CaptureWorkspace(); err != nil {
					t.Fatal("valid combined workspace could not be captured")
				}
			case "absent_explicit_refused":
				choice, err := ExistingBootstrap(filepath.Join(filepath.Dir(path), "explicit-missing.yaml"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := LoadStartupPaths(path, choice, startup.lookup); err == nil {
					t.Fatal("explicit absent bootstrap fell back to combined configuration")
				}
			case "default_symlink_refused":
				source, err := startup.BootstrapSource()
				if err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(filepath.Dir(path), "private-bootstrap-target.yaml")
				if err := os.WriteFile(target, source.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, bootstrap); err != nil {
					t.Fatal(err)
				}
				_, err = readStartupFile(bootstrap, true)
				if err == nil || errors.Is(err, os.ErrNotExist) {
					t.Fatal("unsafe symlink was mistaken for default leaf absence")
				}
				if _, err := LoadStartupPaths(path, DefaultBootstrapChoice(), startup.lookup); err == nil {
					t.Fatal("unsafe default sidecar was followed or ignored")
				}
			case "published_sidecar_reloads_protected":
				if err := startup.PublishDefaultBootstrap(); err != nil {
					t.Fatal("valid default sidecar could not be published")
				}
				info, err := os.Lstat(bootstrap)
				if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
					t.Fatal("published bootstrap was not a private regular file")
				}
				if err := os.WriteFile(path, []byte("external_workers: [\n"), 0600); err != nil {
					t.Fatal(err)
				}
				restarted, err := LoadStartupPaths(path, DefaultBootstrapChoice(), startup.lookup)
				if err != nil || restarted.Origin() != BootstrapOriginDefaultSidecar {
					t.Fatal("protected sidecar did not preserve launch selection with dirty workspace")
				}
				if _, err := restarted.CaptureWorkspace(); err == nil {
					t.Fatal("dirty operational source was admitted through bootstrap reload")
				}
				if err := restarted.ConfirmBootstrapSource(); err != nil {
					t.Fatal("private bootstrap exact-source guard changed")
				}
			default:
				t.Fatal("unhandled bootstrap oracle")
			}
		})
	}
}
