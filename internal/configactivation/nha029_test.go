package configactivation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

func nha029Store(t *testing.T) (*storage.Store, context.Context, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	port := testpg.Start(t).Port
	settings, err := storage.ParseSettings(storage.ConnectionInput{
		Host: "127.0.0.1", Port: port, Database: "postgres", User: "homeward_test",
		PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable",
	}, func(name string) (string, bool) {
		return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD"
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if version, err := store.Migrate(ctx); err != nil || version != storage.CurrentVersion() {
		t.Fatalf("migration: %v %v", version, err)
	}
	if _, err := store.BootstrapMFAPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	return store, ctx, port
}

func nha029Launch(port int, dataDir string) []byte {
	return []byte(fmt.Sprintf(`listen: '127.0.0.1:0'
postgres:
  host: 127.0.0.1
  port: %d
  database: postgres
  user: homeward_test
  password_env: HOMEWARD_TEST_PASSWORD
  sslmode: disable
data_dir: %q
`, port, dataDir))
}

func nha029Lookup(name string) (string, bool) {
	return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD"
}

func TestNHA029FirstStartupAutoActivatesBeforeAnyIntegrationAndReplaysDirtyWorkspace(t *testing.T) {
	store, ctx, port := nha029Store(t)
	root := t.TempDir()
	path := filepath.Join(root, "configuration.yaml")
	raw := append(nha029Launch(port, filepath.Join(root, "private-data")), []byte("# exact comment\nexternal_workers: []\n")...)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029Lookup)
	if err != nil {
		t.Fatal(err)
	}
	admitted, disposition, err := ReconcileStartup(ctx, store, startup)
	if err != nil || disposition != StartupInitialized || !admitted.Valid() {
		t.Fatalf("first SQL activation: disposition=%v valid=%t err=%v", disposition, admitted.Valid(), err)
	}
	active, present, err := store.ReadActiveConfig(ctx)
	if err != nil || !present || active.Revision() != admitted.Revision() ||
		active.ManifestFormat() != storage.ConfigManifestCompiledV2 || len(active.Versions()) != 1 {
		t.Fatalf("initial active SQL missing: present=%t err=%v", present, err)
	}
	if rawFromGeneric, ok := active.Versions()[0].Bytes(); ok || rawFromGeneric != nil {
		t.Fatal("generic reader exposed protected operational bytes")
	}
	bootstrap, present, err := store.ReadActiveBootstrap(ctx)
	if err != nil || !present || bootstrap.Path() != "bootstrap.launch.yaml" {
		t.Fatalf("stable SQL bootstrap pointer missing: present=%t err=%v", present, err)
	}
	if _, err := os.Stat(path + ".bootstrap.yaml"); err != nil {
		t.Fatalf("default sidecar not published before activation: %v", err)
	}
	if err := os.WriteFile(path, []byte("not: valid: yaml\n"), 0600); err != nil {
		t.Fatal(err)
	}
	restart, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029Lookup)
	if err != nil {
		t.Fatalf("dirty workspace parsed before SQL replay: %v", err)
	}
	replayed, disposition, err := ReconcileStartup(ctx, store, restart)
	if err != nil || disposition != StartupReplayed || !replayed.Valid() ||
		replayed.Revision() != admitted.Revision() || replayed.ManifestSHA256() != admitted.ManifestSHA256() {
		t.Fatalf("authoritative SQL replay: disposition=%v err=%v", disposition, err)
	}
	changedBootstrap := append(bootstrap.Bytes(), []byte("recorder:\n  budget_mib: 16\n")...)
	if err := os.WriteFile(path+".bootstrap.yaml", changedBootstrap, 0600); err != nil {
		t.Fatal(err)
	}
	changedStartup, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029Lookup)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapOnly, disposition, err := ReconcileStartup(ctx, store, changedStartup)
	if err != nil || disposition != StartupBootstrapUpdated ||
		bootstrapOnly.Revision() != admitted.Revision() || bootstrapOnly.ManifestSHA256() != admitted.ManifestSHA256() {
		t.Fatalf("launch-only restart advanced operational generation: disposition=%v err=%v", disposition, err)
	}
	newBootstrap, present, err := store.ReadActiveBootstrap(ctx)
	if err != nil || !present || newBootstrap.Ref().Revision().Number() <= bootstrap.Ref().Revision().Number() {
		t.Fatalf("launch-only source did not advance bootstrap pointer: present=%t err=%v", present, err)
	}
}

func TestNHA029FreshExplicitReadOnlyBootstrapAndInvalidSourceFailClosed(t *testing.T) {
	store, ctx, port := nha029Store(t)
	root := t.TempDir()
	dataDir := filepath.Join(root, "private-data")
	path := filepath.Join(root, "configuration.yaml")
	explicit := filepath.Join(root, "operator-bootstrap.yaml")
	if err := os.WriteFile(explicit, nha029Launch(port, dataDir), 0400); err != nil {
		t.Fatal(err)
	}
	choice, err := config.ExistingBootstrap(explicit)
	if err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(path, choice, nha029Lookup)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReconcileStartup(ctx, store, startup); err == nil {
		t.Fatal("missing initial workspace initialized master key or generation")
	}
	if _, err := os.Stat(filepath.Join(dataDir, ".env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid initial source published a key: %v", err)
	}
	if err := os.WriteFile(path, []byte("# admitted exact source\nexternal_workers: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	admitted, disposition, err := ReconcileStartup(ctx, store, startup)
	if err != nil || disposition != StartupInitialized || !admitted.Valid() {
		t.Fatalf("fresh explicit sidecar refused: disposition=%v err=%v", disposition, err)
	}
	if _, err := os.Stat(path + ".bootstrap.yaml"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explicit read-only mode wrote a default sidecar: %v", err)
	}
	stored, err := os.ReadFile(explicit)
	if err != nil || !bytes.Equal(stored, nha029Launch(port, dataDir)) {
		t.Fatalf("explicit bootstrap was changed: %v", err)
	}
}

func TestNHA029LegacyGenerationUpgradeRetainsEntryAndSources(t *testing.T) {
	store, ctx, port := nha029Store(t)
	root := t.TempDir()
	dataDir := filepath.Join(root, "private-data")
	if _, err := store.BootstrapSecrets(ctx, dataDir); err != nil {
		t.Fatal(err)
	}
	legacyBytes := []byte("input_boolean:\n  flag:\n    name: Legacy flag\n")
	legacySource, err := parser.NewSourceFile("legacy.yaml", legacyBytes)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := bundle.CompileSources("legacy.yaml", []parser.SourceFile{legacySource})
	if err != nil {
		t.Fatal(err)
	}
	id, err := storage.NewConfigDocumentID("legacy.source")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := storage.NewConfigDocumentBinding(id, "legacy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := startupMetadata("test:legacy-source")
	if err != nil {
		t.Fatal(err)
	}
	var oldGeneration storage.ConfigGenerationRevision
	outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *storage.ConfigTx) error {
		refs, err := tx.SaveCandidate(candidate, []storage.ConfigDocumentBinding{binding}, metadata)
		if err != nil {
			return err
		}
		oldGeneration, err = tx.RegisterGeneration(candidate, refs)
		if err != nil {
			return err
		}
		return tx.ActivateGeneration(storage.ExpectNoConfigActive(), oldGeneration)
	})
	if outcome != storage.TransactionCommitted || err != nil {
		t.Fatalf("legacy fixture: outcome=%v err=%v", outcome, err)
	}
	path := filepath.Join(root, "configuration.yaml")
	raw := append(nha029Launch(port, dataDir), []byte("external_workers: []\n")...)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029Lookup)
	if err != nil {
		t.Fatal(err)
	}
	admitted, disposition, err := ReconcileStartup(ctx, store, startup)
	if err != nil || disposition != StartupUpgraded || !admitted.Valid() || admitted.Revision() == oldGeneration {
		t.Fatalf("v1→v2 upgrade: disposition=%v err=%v", disposition, err)
	}
	legacy, present := admitted.LegacyCandidate()
	if !present || !legacy.Valid() || legacy.SHA256() != candidate.SHA256() {
		t.Fatal("legacy definitions were not retained in admitted v2 generation")
	}
	active, present, err := store.ReadActiveConfig(ctx)
	if err != nil || !present || active.Entry() != "legacy.yaml" || len(active.Versions()) != 2 {
		t.Fatalf("legacy entry was replaced by operational locator: present=%t err=%v", present, err)
	}
}

type nha029NilContext struct{ context.Context }

func TestNHA029NewStartupPortRejectsZeroAndTypedNil(t *testing.T) {
	var typedNil *nha029NilContext
	if _, _, err := ReconcileStartup(typedNil, nil, config.StartupInputs{}); !errors.Is(err, ErrInvalidStartup) {
		t.Fatalf("typed nil context reached storage: %v", err)
	}
	if (AdmittedGeneration{}).Valid() {
		t.Fatal("zero admitted generation was usable")
	}
}
