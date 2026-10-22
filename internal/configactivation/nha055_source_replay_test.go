package configactivation

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
)

// This fixture qualifies immutable source storage and SQL restart replay. It
// does not claim publication of a dashboard client or a runtime program. It
// intentionally requires the real migration26 and must not be run against a
// projected migration25 parent or treated as an AC pass before that delivery.
func TestNHA055RealSQLSourceReplayRecompilesExactBytesAndRejectsPurgedMembers(t *testing.T) {
	store, ctx, port := nha029Store(t)
	root := t.TempDir()
	path := filepath.Join(root, "configuration.yaml")
	operational := append(nha029Launch(port, filepath.Join(root, "private-data")), []byte("external_workers: []\n")...)
	if err := os.WriteFile(path, operational, 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029Lookup)
	if err != nil {
		t.Fatal(err)
	}
	admitted, _, err := ReconcileStartup(ctx, store, startup)
	if err != nil {
		t.Fatal(err)
	}
	active, present, err := store.ReadActiveConfig(ctx)
	if err != nil || !present || len(active.Versions()) != 1 {
		t.Fatal("real protected SQL startup missing", err)
	}
	operationalRef := active.Versions()[0].Ref()
	manifest, err := registry.LoadManifest(scope.BaselineV1())
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture has no workers or configured identity/capability rows;
	// its empty semantic catalogue above is the actual admitted input.
	// Persist that canonical context so SQL26 seal and restart replay both
	// retain the same input, without populating Host declarations.
	revision, present := manifest.Revision()
	if !present {
		t.Fatal("fixture manifest has no canonical revision")
	}
	manifestRevision, err := revision.Value()
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest, present := manifest.Digest()
	if !present {
		t.Fatal("fixture manifest has no admitted ID digest")
	}
	compilation, err := configcompilation.NewContext(manifestRevision, manifestDigest, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := storage.DashboardSourceDescriptor("dashboard.nha055_replay", "dashboard.yaml")
	if err != nil {
		t.Fatal(err)
	}
	author, _ := storage.NewConfigAuthor("nha055:isolated-source-replay")
	source, _ := storage.NewConfigSource("nha055:isolated-source-replay")
	metadata, _ := storage.NewConfigVersionMetadata(author, source)
	var first storage.ConfigGenerationRevision
	var firstManifest [32]byte
	for index := 0; index < 22; index++ {
		raw := fmt.Sprintf("# NHA055_SQL_PRIVATE_CANARY revision %d\nviews: []\n", index)
		group := nha055SourceGroup(t, descriptor, "source.dashboard.nha055_replay", raw)
		compiled, err := CompileSourceGroups([]SourceGroup{group}, manifest, catalogue)
		if err != nil || !compiled.Valid() {
			t.Fatal("real domain compiler rejected source", err)
		}
		expected, err := storage.ExpectConfigActive(active.Revision(), active.ManifestSHA256())
		if err != nil {
			t.Fatal(err)
		}
		attempt, err := storage.RandomActivationAttemptID()
		if err != nil {
			t.Fatal(err)
		}
		var target storage.ConfigGenerationRevision
		outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *storage.ConfigTx) error {
			ref, err := tx.SaveEditableSource(descriptor, group.bindings[0], group.files[0], metadata)
			if err != nil {
				return err
			}
			hash := compiledSourceGroupsHash(compiledOperationalHash(admitted.SourceSHA256(), admitted.legacy), []SourceGroup{group})
			target, err = tx.RegisterCompiledGenerationWithContext(active.Entry(), hash, []storage.ConfigVersionRef{operationalRef, ref}, compilation)
			if err != nil {
				return err
			}
			if err := tx.ActivateGeneration(expected, target); err != nil {
				return err
			}
			return tx.RecordActivationAttempt(attempt, metadata)
		})
		if err != nil || outcome != storage.TransactionCommitted {
			t.Fatal("real source transaction/activation failed (migration26 must actually exist)", index, err)
		}
		active, present, err = store.ReadActiveConfig(ctx)
		if err != nil || !present || active.Revision() != target {
			t.Fatal("new sealed source generation missing", err)
		}
		if index == 0 {
			first, firstManifest = active.Revision(), active.ManifestSHA256()
			if err := os.WriteFile(path, []byte("invalid: [ workspace\n"), 0600); err != nil {
				t.Fatal(err)
			}
			restart, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029Lookup)
			if err != nil {
				t.Fatal(err)
			}
			replayed, disposition, err := ReconcileStartup(ctx, store, restart)
			if err != nil || disposition != StartupReplayed || len(replayed.extraGroups) != 1 {
				t.Fatal("SQL source replay depended on the invalid editable workspace", err)
			}
			if !bytes.Equal(replayed.extraGroups[0].files[0].Bytes(), []byte(raw)) {
				t.Fatal("SQL replay changed exact editable source bytes")
			}
			recompiled, err := CompileSourceGroups(replayed.extraGroups, manifest, catalogue)
			if err != nil || recompiled.SHA256() != compiled.SHA256() {
				t.Fatal("replay did not recompile the exact durable source set", err)
			}
		}
	}
	historical, err := store.ReadConfigGeneration(ctx, first)
	if err != nil || historical.Availability() != storage.ConfigGenerationIncomplete {
		t.Fatal("retention hid a required purged source member", err)
	}
	_, err = store.ReadEditableSources(ctx, first, firstManifest)
	var failure *storage.Error
	if !errors.As(err, &failure) || failure.Kind() != storage.ConfigIncomplete {
		t.Fatal("purged source set was presented as fully replayable", err)
	}
}
