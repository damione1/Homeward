package configactivation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/logicselection"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
)

func nha055SourceGroup(t *testing.T, descriptor storage.SourceDescriptor, document, raw string) SourceGroup {
	t.Helper()
	id, err := storage.NewConfigDocumentID(document)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := storage.NewConfigDocumentBinding(id, descriptor.Entry())
	if descriptor.Kind() == storage.ConfigDocumentKindOwnershipSource {
		binding, err = storage.NewJSONConfigDocumentBinding(id, descriptor.Entry())
	}
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.NewSourceFile(descriptor.Entry(), []byte(raw))
	if descriptor.Kind() == storage.ConfigDocumentKindOwnershipSource {
		file, err = parser.NewJSONSourceFile(descriptor.Entry(), []byte(raw))
	}
	if err != nil {
		t.Fatal(err)
	}
	group, err := NewSourceGroup(descriptor, []parser.SourceFile{file}, []storage.ConfigDocumentBinding{binding})
	if err != nil {
		t.Fatal(err)
	}
	return group
}

func TestNHA055SourceGroupsCompileFiveExistingDomainFormatsAndBindExactBytes(t *testing.T) {
	manifest, err := registry.LoadManifest(scope.BaselineV1())
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	dashboard, err := storage.DashboardSourceDescriptor("dashboard.nha055", "dashboard.yaml")
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
	groups := []SourceGroup{
		nha055SourceGroup(t, dashboard, "source.dashboard.nha055", "views: []\n"),
		nha055SourceGroup(t, automation, "source.automation.nha055", "id: nha055\ntriggers:\n - trigger: time\n   at: '18:00:00'\nactions:\n - stop: complete\n"),
		nha055SourceGroup(t, script, "source.script.nha055", "sequence:\n - stop: complete\n"),
		nha055SourceGroup(t, scene, "source.scene.nha055", "id: fixture\nname: Preview\nentities:\n switch.arc_soundbar_crossfade: 'off'\n"),
		nha055SourceGroup(t, ownership, "source.ownership.nha055", string(scope.HeatingOwnershipV1())),
	}
	compiled, err := CompileSourceGroups(groups, manifest, catalogue)
	if err != nil || !compiled.Valid() || len(compiled.Automations()) != 1 || len(compiled.Scripts()) != 1 || len(compiled.Scenes()) != 1 || len(compiled.Groups()) != 5 {
		t.Fatal("existing closed compilers did not retain the exact five source families", err)
	}
	if _, present := compiled.Ownership(); !present {
		t.Fatal("ownership compiler result lost")
	}
	if compiled.SHA256() != sourceCompilationDigest(groups) {
		t.Fatal("compiled proof and transaction source transcript differ")
	}
	changed := append([]SourceGroup(nil), groups...)
	changed[0] = nha055SourceGroup(t, dashboard, "source.dashboard.nha055", "# exact-byte-only change NHA055_PRIVATE_SOURCE\nviews: []\n")
	other, err := CompileSourceGroups(changed, manifest, catalogue)
	if err != nil || other.SHA256() == compiled.SHA256() {
		t.Fatal("semantic-equivalent source change did not invalidate the exact source proof", err)
	}
	encoded, err := json.Marshal(other)
	if err != nil || bytes.Contains(encoded, []byte("NHA055_PRIVATE_SOURCE")) || bytes.Contains([]byte(fmt.Sprintf("%#v", changed[0])), []byte("NHA055_PRIVATE_SOURCE")) {
		t.Fatal("source compilation leaked private source", err)
	}
}

func TestNHA055SourceGroupsRefuseDeferredSelectionAndExcludedDashboardReferences(t *testing.T) {
	manifest, err := registry.LoadManifest(scope.BaselineV1())
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := logicselection.LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	deferred := 0
	for _, entry := range selection.Entries() {
		id := entry.ID()
		disposition, known := selection.DispositionFor(id)
		if known && disposition == logicselection.DispositionDeferred {
			name, err := id.Value()
			if err != nil {
				t.Fatal(err)
			}
			if selectedSourceIdentity(selection, name) {
				t.Fatal("deferred source acquired runtime configuration admission")
			}
			deferred++
		}
	}
	if deferred != 72 {
		t.Fatalf("selection baseline changed unexpectedly: %d deferred", deferred)
	}
	descriptor, err := storage.DashboardSourceDescriptor("dashboard.nha055", "dashboard.yaml")
	if err != nil {
		t.Fatal(err)
	}
	group := nha055SourceGroup(t, descriptor, "source.dashboard.nha055", "views:\n - type: sections\n   path: preview\n   title: Preview\n   sections:\n    - type: grid\n      cards:\n       - type: tile\n         entity: light.excluded_outside_manifest\n")
	if _, err := CompileSourceGroups([]SourceGroup{group}, manifest, catalogue); err == nil {
		t.Fatal("dashboard source escaped the authoritative manifest")
	}
}
