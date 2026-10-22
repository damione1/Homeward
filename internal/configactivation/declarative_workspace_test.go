package configactivation

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
)

func nha055WorkspaceDefinitions(t *testing.T, groupName, document, entry string) DefinitionChoice {
	t.Helper()
	file, err := parser.NewSourceFile(entry, []byte("input_boolean: {flag: {initial: false}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := bundle.CompileSources(entry, []parser.SourceFile{file})
	if err != nil {
		t.Fatal(err)
	}
	group, err := storage.NewConfigDocumentID(groupName)
	if err != nil {
		t.Fatal(err)
	}
	id, err := storage.NewConfigDocumentID(document)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := storage.NewConfigDocumentBinding(id, entry)
	if err != nil {
		t.Fatal(err)
	}
	choice, err := ReplaceWorkspaceDefinitions(group, candidate, []storage.ConfigDocumentBinding{binding})
	if err != nil {
		t.Fatal(err)
	}
	return choice
}

func TestNHA055WorkspaceDefinitionsBindNamespaceWithoutGuessingDocumentIdentity(t *testing.T) {
	choice := nha055WorkspaceDefinitions(t, "workspace.group", "document.entry", "entry.yaml")
	group, present := choice.workspace.Group()
	if !present || group.String() != "workspace.group" || group == choice.bindings[0].ID() {
		t.Fatal("workspace namespace became a document binding")
	}
	ordinary, err := ReplaceDefinitions(choice.candidate, choice.bindings)
	if err != nil || !ordinary.workspace.Valid() {
		t.Fatal(err)
	}
	if _, present := ordinary.workspace.Group(); present {
		t.Fatal("ordinary replacement invented a physical locator")
	}
	if _, err := ReplaceWorkspaceDefinitions(storage.ConfigDocumentID{}, choice.candidate, choice.bindings); err == nil {
		t.Fatal("missing namespace was admitted")
	}
}

// Future SQL26 qualification of the actual prepare/write/read transaction
// path. Registry publication uses the existing in-memory fake projection;
// no run engine, pilot or command dispatcher is constructed.
func TestNHA055WorkspaceDefinitionsRetainLocatorAcrossOperationalAndReplacementWrites(t *testing.T) {
	store, ctx, port := nha029Store(t)
	root := t.TempDir()
	path := filepath.Join(root, "configuration.yaml")
	raw := append(nha029Launch(port, filepath.Join(root, "private-data")), []byte("external_workers: []\n")...)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029Lookup)
	if err != nil {
		t.Fatal(err)
	}
	current, _, err := ReconcileStartup(ctx, store, startup)
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := current.Secrets()
	if err != nil {
		t.Fatal(err)
	}
	replay, err := storage.NewConfigReplayRepository(store, secrets)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := registry.ParseManifest(scope.ManifestV1())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := registry.NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	_, sink, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.AttachProjection(ctx, sink); err != nil {
		t.Fatal(err)
	}
	author, _ := storage.NewConfigAuthor("nha055:locator-retain")
	source, _ := storage.NewConfigSource("nha055:transaction-fixture")
	launch, err := startup.LaunchConfig()
	if err != nil {
		t.Fatal(err)
	}
	apply := func(choice DefinitionChoice) storage.ConfigGenerationSnapshot {
		t.Helper()
		prepared, err := prepareWorkspaceActivation(ctx, store, startup, current, choice, author, source, nil)
		if err != nil {
			t.Fatal(err)
		}
		gate, err := repo.BeginActivation(ctx)
		if err != nil {
			t.Fatal(err)
		}
		target, outcome, err := writePreparedActivation(ctx, store, replay, gate, prepared, nil)
		if err != nil || outcome != storage.TransactionCommitted {
			_ = gate.ResolveNotCommitted(outcome)
			_ = gate.Release()
			t.Fatal("actual Owner transaction failed", outcome, err)
		}
		active, present, err := store.ReadActiveConfig(ctx)
		if err != nil || !present || active.Revision() != target {
			t.Fatal(err)
		}
		if err := gate.PublishCommitted(ctx, target, active.ManifestSHA256()); err != nil {
			t.Fatal(err)
		}
		if err := gate.Release(); err != nil {
			t.Fatal(err)
		}
		current, err = readAdmitted(ctx, store, replay, target, active.ManifestSHA256(), launch, secrets)
		if err != nil {
			t.Fatal(err)
		}
		return active
	}
	choiceA := nha055WorkspaceDefinitions(t, "workspace.group-a", "document.entry-a", "entry-a.yaml")
	a := apply(choiceA)
	group, present := a.DeclarativeWorkspace().Group()
	if !present || group.String() != "workspace.group-a" || a.Entry() != "entry-a.yaml" {
		t.Fatal("first workspace replacement lost its descriptor")
	}
	if err := os.WriteFile(path, append(append([]byte(nil), raw...), []byte("# operational-only update\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	retained := apply(RetainDefinitions())
	afterGroup, present := retained.DeclarativeWorkspace().Group()
	if !present || afterGroup != group || retained.Entry() != a.Entry() {
		t.Fatal("RetainDefinitions discarded or guessed the locator")
	}
	for _, v := range a.Versions() {
		kind, present := v.Kind()
		if !present || kind != storage.ConfigDocumentKindYAMLSource {
			continue
		}
		found := false
		for _, next := range retained.Versions() {
			found = found || next.Ref() == v.Ref()
		}
		if !found {
			t.Fatal("retention allocated/replaced declarative source refs")
		}
	}
	choiceB := nha055WorkspaceDefinitions(t, "workspace.group-b", "document.entry-b", "entry-b.yaml")
	b := apply(choiceB)
	groupB, present := b.DeclarativeWorkspace().Group()
	if !present || groupB.String() != "workspace.group-b" || b.Entry() != "entry-b.yaml" {
		t.Fatal("entry/group replacement retained target A metadata")
	}
	for _, version := range b.Versions() {
		kind, present := version.Kind()
		if present && kind == storage.ConfigDocumentKindYAMLSource && version.Path() == "entry-a.yaml" {
			t.Fatal("global replacement retained an old entry member")
		}
	}
	old, err := store.ReadConfigGeneration(ctx, a.Revision())
	if err != nil {
		t.Fatal(err)
	}
	oldGroup, present := old.DeclarativeWorkspace().Group()
	if !present || oldGroup != group || old.Entry() != a.Entry() {
		t.Fatal("replacement rewrote the old sealed locator")
	}
}
