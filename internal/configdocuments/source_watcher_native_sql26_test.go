package configdocuments

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
)

// FUTURE SQL26 ONLY: requires the actually delivered migration25 parent and
// registered migration26. It must not be selected against current SQL24 or used
// as proof that source activation/runtime replay is qualified. It tests durable
// grouping→locator admission without activating the constructed generation.
func TestNHA055FutureSQL26NativeWatcherReconstructsOnlyPersistedGroupedLocators(t *testing.T) {
	service, ctx, operational, _ := nha055Documents(t)
	active, present, err := service.store.ReadActiveConfig(ctx)
	if err != nil || !present {
		t.Fatal(err)
	}
	manifest, err := registry.LoadManifest(scope.BaselineV1())
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := storage.DashboardSourceDescriptor("dashboard.nha055_first", "dashboard.yaml")
	if err != nil {
		t.Fatal(err)
	}
	second, err := storage.DashboardSourceDescriptor("dashboard.nha055_second", "dashboard.yaml")
	if err != nil {
		t.Fatal(err)
	}
	automation, err := storage.AutomationSourceDescriptor("automation.nha055_synthetic", "automation.yaml")
	if err != nil {
		t.Fatal(err)
	}
	script, err := storage.ScriptSourceDescriptor("script.nha055_synthetic", "script.yaml")
	if err != nil {
		t.Fatal(err)
	}
	scene, err := storage.SceneSourceDescriptor("scene.ceilling_fan_night", "scene.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ownership, err := storage.OwnershipSourceDescriptor("heating.ownership", "ownership.json")
	if err != nil {
		t.Fatal(err)
	}
	var groups []configactivation.SourceGroup
	for index, fixture := range []struct {
		descriptor storage.SourceDescriptor
		raw        string
	}{
		{first, "views: []\n"}, {second, "# independently bound same entry\nviews: []\n"},
		{automation, "id: nha055\ntriggers:\n - trigger: time\n   at: '18:00:00'\nactions:\n - stop: complete\n"},
		{script, "sequence:\n - stop: complete\n"},
		{scene, "id: fixture\nname: Preview\nentities:\n switch.arc_soundbar_crossfade: 'off'\n"},
		{ownership, string(scope.HeatingOwnershipV1())},
	} {
		id, err := storage.NewConfigDocumentID(fmt.Sprintf("nha055.watcher.source.%d", index))
		if err != nil {
			t.Fatal(err)
		}
		binding, err := storage.NewConfigDocumentBinding(id, fixture.descriptor.Entry())
		file, fileErr := parser.NewSourceFile(fixture.descriptor.Entry(), []byte(fixture.raw))
		if fixture.descriptor.Kind() == storage.ConfigDocumentKindOwnershipSource {
			binding, err = storage.NewJSONConfigDocumentBinding(id, fixture.descriptor.Entry())
			file, fileErr = parser.NewJSONSourceFile(fixture.descriptor.Entry(), []byte(fixture.raw))
		}
		if err != nil || fileErr != nil {
			t.Fatal(err, fileErr)
		}
		group, err := configactivation.NewSourceGroup(fixture.descriptor, []parser.SourceFile{file}, []storage.ConfigDocumentBinding{binding})
		if err != nil {
			t.Fatal(err)
		}
		groups = append(groups, group)
	}
	compiled, err := configactivation.CompileSourceGroups(groups, manifest, catalogue)
	if err != nil || !compiled.Valid() {
		t.Fatal("real closed compilers rejected native fixtures", err)
	}
	revision, _ := manifest.Revision()
	name, err := revision.Value()
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := manifest.Digest()
	compilation, err := configcompilation.NewContext(name, digest, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	author, err := storage.NewConfigAuthor("nha055:watcher-locator-fixture")
	if err != nil {
		t.Fatal(err)
	}
	source, err := storage.NewConfigSource("nha055:watcher-locator-fixture")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := storage.NewConfigVersionMetadata(author, source)
	if err != nil {
		t.Fatal(err)
	}
	var generation storage.ConfigGenerationRevision
	outcome, err := service.store.InConfigTransactionOutcome(ctx, func(tx *storage.ConfigTx) error {
		refs := make([]storage.ConfigVersionRef, 0)
		for _, version := range active.Versions() {
			refs = append(refs, version.Ref())
		}
		for _, group := range groups {
			file := group.Files()[0]
			binding := group.Bindings()[0]
			ref, err := tx.SaveEditableSource(group.Descriptor(), binding, file, metadata)
			if err != nil {
				return err
			}
			refs = append(refs, ref)
		}
		var err error
		generation, err = tx.RegisterCompiledGenerationWithContext(active.Entry(), compiled.SHA256(), refs, compilation)
		return err
	})
	if err != nil || outcome != storage.TransactionCommitted {
		t.Fatal("real migration26 is required before this test", err)
	}
	sealed, err := service.store.ReadConfigGeneration(ctx, generation)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := service.store.ReadEditableSources(ctx, generation, sealed.ManifestSHA256())
	if err != nil {
		t.Fatal(err)
	}
	targets, err := nativeSourceWatchTargets(snapshots)
	if err != nil || len(targets) != 6 {
		t.Fatal("persisted native grouping was lost", err)
	}
	for _, target := range targets {
		for _, member := range target.members {
			var exact []byte
			for _, snapshot := range snapshots {
				if snapshot.Binding() == member.binding {
					exact = snapshot.File().Bytes()
				}
			}
			if len(exact) == 0 {
				t.Fatal("locator member did not come from SQL snapshot")
			}
			nha055ObservedFile(t, operational, target.group, member.binding, exact)
			observed, err := service.workspace.ReadSources(target.group, []storage.ConfigDocumentBinding{member.binding})
			if err != nil || !bytes.Equal(observed[0].Bytes(), exact) {
				t.Fatal("persisted native locator selected other bytes", err)
			}
		}
	}
	after, present, err := service.store.ReadActiveConfig(ctx)
	if err != nil || !present || after.Revision() != active.Revision() || after.ManifestSHA256() != active.ManifestSHA256() {
		t.Fatal("locator qualification activated a generation", err)
	}
}
