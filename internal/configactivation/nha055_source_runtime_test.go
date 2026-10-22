package configactivation

import (
	"context"
	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/reactivetemplate"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
	"testing"
	"time"
)

type nha055SourceResolver struct{}

func (nha055SourceResolver) ResolveEntity(domain.HAAlias) (domain.EntityID, error) {
	return domain.EntityID{}, ErrInvalidStartup
}
func (nha055SourceResolver) ResolveDevice(ir.HADeviceRef) (domain.DeviceID, error) {
	return domain.DeviceID{}, ErrInvalidStartup
}
func (nha055SourceResolver) ResolveCalendar(domain.HAAlias) (automationtriggers.CalendarSourceID, error) {
	return automationtriggers.CalendarSourceID{}, ErrInvalidStartup
}

func TestNHA055SourceRuntimeRebuildsRealProgramAndSceneOverlayFromExactSources(t *testing.T) {
	manifest, err := registry.LoadManifest(scope.BaselineV1())
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := NewSourceRuntimeCompiler(manifest, catalogue, automationtriggers.PlanInput{Zone: "UTC", Resolver: nha055SourceResolver{}})
	if err != nil {
		t.Fatal(err)
	}
	// Compose the same explicit wait-capable template engine as ScriptRuns.
	// An empty permanent-only plan is intentionally inactive.
	templates, err := (reactivetemplate.Plan{}).WithScriptWaits(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	automation, err := storage.AutomationSourceDescriptor("automation.nha055_runtime", "automation.yaml")
	if err != nil {
		t.Fatal(err)
	}
	scene, err := storage.SceneSourceDescriptor("scene.ceilling_fan_night", "scene.yaml")
	if err != nil {
		t.Fatal(err)
	}
	groups := []SourceGroup{
		nha055SourceGroup(t, automation, "source.runtime.nha055", "id: nha055_runtime\ntriggers:\n - trigger: time\n   at: '18:00:00'\nactions:\n - stop: complete\n"),
		nha055SourceGroup(t, scene, "source.runtime.scene.nha055", "id: fixture\nname: Edited\nentities:\n switch.arc_soundbar_crossfade: 'off'\n"),
	}
	first, err := compiler.Compose(context.Background(), groups, templates)
	if err != nil {
		t.Fatal(err)
	}
	runs, valid := first.Runs()
	if !valid || len(runs.AutomationIDs()) != 1 {
		t.Fatal("exact sources did not enter existing run plan")
	}
	triggers, valid := first.Triggers()
	if !valid || len(triggers.AutomationIDs()) != 1 {
		t.Fatal("exact sources did not enter existing trigger plan")
	}
	scenes, valid := first.Scenes()
	if !valid || len(scenes.IDs()) != 12 {
		t.Fatal("scene overlay lost immutable baseline seeds")
	}
	// A restart recomposes from the retained sources, never from the previous IR.
	replayed, err := compiler.Compose(context.Background(), first.Compilation().Groups(), templates)
	if err != nil || replayed.Compilation().SHA256() != first.Compilation().SHA256() {
		t.Fatal("source based recomposition changed exact admitted proof", err)
	}
	absent := &Owner{}
	if err := absent.confirmEditableSourceComposition(context.Background(), Request{sourceReplacement: &groups[0]}, AdmittedGeneration{}); err != ErrProgramCompositionRequired {
		t.Fatal("missing executable port was not rejected before staging", err)
	}
}

func TestNHA055SourceRuntimeSceneOnlyUsesExistingDormantPlans(t *testing.T) {
	manifest, err := registry.LoadManifest(scope.BaselineV1())
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := NewSourceRuntimeCompiler(manifest, catalogue, automationtriggers.PlanInput{Zone: "UTC", Resolver: nha055SourceResolver{}})
	if err != nil {
		t.Fatal(err)
	}
	scene, err := storage.SceneSourceDescriptor("scene.ceilling_fan_night", "scene.yaml")
	if err != nil {
		t.Fatal(err)
	}
	group := nha055SourceGroup(t, scene, "source.runtime.scene.only", "id: fixture\nname: Edited\nentities:\n switch.arc_soundbar_crossfade: 'off'\n")
	image, err := compiler.Compose(context.Background(), []SourceGroup{group}, reactivetemplate.Plan{})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := image.Program(); present {
		t.Fatal("scene-only source invented executable automation composition")
	}
	runs, present := image.Runs()
	if !present || !runs.Valid() || len(runs.AutomationIDs()) != 0 || len(runs.ScriptIDs()) != 0 {
		t.Fatal("scene-only source did not retain admitted dormant runs")
	}
	triggers, present := image.Triggers()
	if !present || !triggers.Valid() || triggers.Count() != 0 {
		t.Fatal("scene-only source did not retain admitted dormant triggers")
	}
	scenes, present := image.Scenes()
	if !present || len(scenes.IDs()) != 12 {
		t.Fatal("scene-only overlay lost baseline seeds")
	}
	if _, err := NewSourceRuntimeCompiler(manifest, catalogue, automationtriggers.PlanInput{Zone: "Not/A_Real_Zone", Resolver: nha055SourceResolver{}}); err == nil {
		t.Fatal("invalid runtime zone admitted")
	}
}
