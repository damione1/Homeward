package configplans

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
)

// Future SQL26 fixture. It exercises the actual read-side capture and exact
// rollback target selection without attaching a run engine or physical ports.
// SQL registrations below are storage fixtures, not runtime publication claims.
func nha055RollbackDocuments(t *testing.T) (*Service, context.Context) {
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
	documents, err := configdocuments.NewService(configdocuments.Input{Store: store, Owner: owner, Startup: startup, Secrets: secrets})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = documents.Close() })
	service, err := NewService(Input{Documents: documents, Owner: owner, Secrets: secrets, Manifest: manifest})
	if err != nil {
		t.Fatal(err)
	}
	return service, ctx
}

func TestNHA055RollbackSourcesEntryAToBSelectsAWhileCapturingCurrentB(t *testing.T) {
	service, ctx := nha055RollbackDocuments(t)
	store := service.documents.Store()
	// The real fixture configures no workers or identities. Bind the same
	// empty declarations to its actual manifest for both entry generations;
	// these bytes remain compilation data and grant no runtime capability.
	revision, present := service.manifest.Revision()
	if !present {
		t.Fatal("fixture manifest has no canonical revision")
	}
	manifestRevision, err := revision.Value()
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest, present := service.manifest.Digest()
	if !present {
		t.Fatal("fixture manifest has no admitted ID digest")
	}
	compilation, err := configcompilation.NewContext(manifestRevision, manifestDigest, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := nha055RollbackMember(t, Dashboard, "dashboard.rollback", "entry-a.yaml", "source.dashboard.rollback.a", "entry-a.yaml", "# historical entry A\nviews: []\n")
	b := nha055RollbackMember(t, Dashboard, "dashboard.rollback", "entry-b.yaml", "source.dashboard.rollback.b", "entry-b.yaml", "# current entry B\nviews: []\n")
	author, _ := storage.NewConfigAuthor("nha055:rollback-entry-test")
	source, _ := storage.NewConfigSource("nha055:storage-fixture")
	metadata, err := storage.NewConfigVersionMetadata(author, source)
	if err != nil {
		t.Fatal(err)
	}
	write := func(member rollbackSourceMember, label string) storage.ConfigGenerationSnapshot {
		t.Helper()
		active, present, err := store.ReadActiveConfig(ctx)
		if err != nil || !present {
			t.Fatal("missing fixture operational generation", err)
		}
		var operational storage.ConfigVersionRef
		for _, version := range active.Versions() {
			kind, present := version.Kind()
			if present && kind == storage.ConfigDocumentKindOperationalYAML {
				operational = version.Ref()
			}
		}
		expected, err := storage.ExpectConfigActive(active.Revision(), active.ManifestSHA256())
		if err != nil || !operational.Valid() {
			t.Fatal("invalid operational fixture ref", err)
		}
		attempt, err := storage.RandomActivationAttemptID()
		if err != nil {
			t.Fatal(err)
		}
		var generation storage.ConfigGenerationRevision
		outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *storage.ConfigTx) error {
			ref, err := tx.SaveEditableSource(member.descriptor, member.binding, member.file, metadata)
			if err != nil {
				return err
			}
			generation, err = tx.RegisterCompiledGenerationWithContext("configuration.yaml", sha256.Sum256([]byte(label)), []storage.ConfigVersionRef{operational, ref}, compilation)
			if err != nil {
				return err
			}
			if err := tx.ActivateGeneration(expected, generation); err != nil {
				return err
			}
			return tx.RecordActivationAttempt(attempt, metadata)
		})
		if err != nil || outcome != storage.TransactionCommitted {
			t.Fatal("storage-only entry generation fixture failed", outcome, err)
		}
		result, err := store.ReadConfigGeneration(ctx, generation)
		if err != nil {
			t.Fatal(err)
		}
		storedContext, err := store.ReadConfigCompilationContext(ctx, generation, result.ManifestSHA256(), sha256.Sum256([]byte(label)))
		if err != nil || storedContext.SHA256() != compilation.SHA256() {
			t.Fatal("entry generation lost its canonical compilation input", err)
		}
		return result
	}
	old := write(a, "entry A generation")
	current := write(b, "entry B generation with a new document binding")
	values, err := store.ReadEditableSourceGroup(ctx, old.Revision(), old.ManifestSHA256(), a.descriptor.Kind(), a.descriptor.Identity())
	if err != nil || len(values) != 1 {
		t.Fatal("old retained entry was not found", err)
	}
	members := []rollbackSourceMember{{descriptor: values[0].Descriptor(), binding: values[0].Binding(), ref: values[0].Ref(), file: values[0].File()}}
	// Source A still exists in immutable SQL although it is absent from the
	// active group. SQL existence and workspace absence remain independent.
	expected, err := configdocuments.NewExpectedSource(a.binding, values[0].Ref().Revision(), a.file.SHA256(), false, configdocuments.ExpectAbsentMember())
	if err != nil {
		t.Fatal(err)
	}
	target, err := rollbackSourceCandidate(Dashboard, a.descriptor.Identity(), members, []configdocuments.ExpectedSource{expected})
	if err != nil || target.entry != "entry-a.yaml" || !bytes.Equal(target.files[0].Bytes(), a.file.Bytes()) {
		t.Fatal("rollback target borrowed current entry B", err)
	}
	files, sourceRevision, entry, err := service.capturedEditableSources(ctx, target, current)
	if err != nil || entry != "entry-b.yaml" || sourceRevision < 1 || len(files) != 1 || !bytes.Equal(files[0].Bytes(), b.file.Bytes()) {
		t.Fatal("current entry B was rejected or relabelled as target entry A", err)
	}
	prior, err := NewCandidate(target.family, target.identity, entry, files)
	if err != nil || prior.entry != "entry-b.yaml" || prior.hash == target.hash || target.entry != "entry-a.yaml" {
		t.Fatal("prior digest/entry was falsified by the rollback target", err)
	}
	if _, err := rollbackSourceCandidate(Dashboard, a.descriptor.Identity(), members, nha055RollbackExpected(t, b)); err == nil {
		t.Fatal("current entry B binding overwrote historical entry A membership")
	}

}
