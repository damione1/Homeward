package configdocuments

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
)

// This fixture uses migrated PostgreSQL, protected SQL startup and the actual
// document/Owner implementations. Runtime attachment is deliberately absent:
// both tests must fail on the stale precondition before activation admission.
func nha055Documents(t *testing.T) (*Service, context.Context, string, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	port := testpg.Start(t).Port
	lookup := func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" }
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, lookup)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapMFAPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	path := filepath.Join(root, "configuration.yaml")
	raw := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\npostgres:\n  host: 127.0.0.1\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\ndata_dir: %q\nexternal_workers: []\n", port, filepath.Join(root, "private-data")))
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), lookup)
	if err != nil {
		t.Fatal(err)
	}
	admitted, _, err := configactivation.ReconcileStartup(ctx, store, startup)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := registry.ParseManifest(scope.ManifestV1())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := registry.NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := sequencer.New(sequencer.RealClock{}, 16)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := configactivation.NewOwner(configactivation.OwnerInput{Store: store, Startup: startup, Current: admitted, Registry: repository, Projection: projection, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := owner.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	secrets, err := admitted.Secrets()
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(Input{Store: store, Owner: owner, Startup: startup, Secrets: secrets})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service, ctx, path, raw
}

func nha055DocumentBasis(t *testing.T, service *Service, ctx context.Context, expires time.Time) (Snapshot, ExpectedDocument, WriteBasis) {
	t.Helper()
	current, err := service.Read(ctx, operationalID())
	if err != nil {
		t.Fatal(err)
	}
	workspace, _ := current.Workspace().SHA256()
	expected, err := NewExpectedDocument(operationalID(), current.Document().Ref().Revision(), current.Document().SourceSHA256(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	basis, err := NewWriteBasis(current.Generation(), current.ManifestSHA256(), current.CatalogCut().SHA256(), expires)
	if err != nil {
		t.Fatal(err)
	}
	return current, expected, basis
}

func nha055DocumentsUnchanged(t *testing.T, service *Service, ctx context.Context, before Snapshot, path string, raw []byte, info os.FileInfo) {
	t.Helper()
	after, err := service.Read(ctx, operationalID())
	if err != nil {
		t.Fatal(err)
	}
	exact, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(exact, raw) || !os.SameFile(stat, info) || !stat.ModTime().Equal(info.ModTime()) || stat.Mode() != info.Mode() || after.Generation() != before.Generation() || after.ManifestSHA256() != before.ManifestSHA256() || after.Document().Ref() != before.Document().Ref() || after.Workspace().ReconciliationPending() {
		t.Fatal("stale planned write touched the workspace or active generation")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if len(entry.Name()) > len(".homeward-config-") && entry.Name()[:len(".homeward-config-")] == ".homeward-config-" {
			t.Fatal("pre-write refusal created staging artifacts")
		}
	}
}

func TestNHA055RealDocumentWriterRechecksReceiptExpiryAfterMutationWait(t *testing.T) {
	service, ctx, path, raw := nha055Documents(t)
	deadline := time.Now().Add(50 * time.Millisecond)
	before, expected, basis := nha055DocumentBasis(t, service, ctx, deadline)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	release, err := service.acquireMutation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	author, _ := storage.NewConfigAuthor("nha055:expiry-test")
	done := make(chan error, 1)
	go func() {
		_, err := service.WritePlanned(ctx, expected, append(bytes.Clone(raw), []byte("# must not stage\n")...), author, basis)
		done <- err
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		release()
		t.Fatal(ctx.Err())
	}
	release()
	select {
	case err := <-done:
		nha149Failure(t, err, Conflict)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	nha055DocumentsUnchanged(t, service, ctx, before, path, raw, info)
}

func TestNHA055RealDocumentWriterRejectsChangedCatalogWithSameActivePointer(t *testing.T) {
	service, ctx, path, raw := nha055Documents(t)
	before, expected, basis := nha055DocumentBasis(t, service, ctx, time.Now().Add(time.Minute))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Register sealed history with exactly the already-retained version. The
	// current document and active pointer remain identical, but the catalogue
	// cut must still invalidate the authenticated preview.
	outcome, err := service.Store().InConfigTransactionOutcome(ctx, func(tx *storage.ConfigTx) error {
		_, err := tx.RegisterCompiledGeneration("configuration.yaml", before.Document().SourceSHA256(), []storage.ConfigVersionRef{before.Document().Ref()})
		return err
	})
	if err != nil || outcome != storage.TransactionCommitted {
		t.Fatal("real independent history registration failed", err)
	}
	author, _ := storage.NewConfigAuthor("nha055:catalog-test")
	_, err = service.WritePlanned(ctx, expected, append(bytes.Clone(raw), []byte("# stale catalog\n")...), author, basis)
	nha149Failure(t, err, Conflict)
	nha055DocumentsUnchanged(t, service, ctx, before, path, raw, info)
}
