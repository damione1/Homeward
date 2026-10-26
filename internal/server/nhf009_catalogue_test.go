package server

import (
	"path/filepath"
	"testing"

	imported "github.com/damione1/Homeward/compat/ha/dashboard"
	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/dashboard/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
)

// Inputs are exactly the documents covered by the containing capture's held
// dashboard permits. Never resolve routes from an unauthorized source file.
func projectCatalogueLayouts(results []imported.Result, support *wire.ClientLayoutSupport, resolve func(domain.HAAlias) (domain.EntityID, bool)) ([]*wire.NativeLayout, error) {
	definitions := make([]imported.Definition, 0, len(results))
	for _, result := range results {
		definition, present := result.Definition()
		if !present {
			return nil, ErrInvalidPublicPlan
		}
		definitions = append(definitions, definition)
	}
	navigation, err := imported.NewNavigationIndex(definitions)
	if err != nil {
		return nil, err
	}
	layouts := make([]*wire.NativeLayout, 0, len(results))
	for _, result := range results {
		layout, err := imported.ProjectNativeLayout(result, imported.LayoutProjection{Support: support, Entity: resolve, Navigation: &navigation})
		if err != nil {
			return nil, err
		}
		layouts = append(layouts, layout)
	}
	return layouts, nil
}
func nhf009Support() *wire.ClientLayoutSupport {
	return &wire.ClientLayoutSupport{Profile: imported.RecipesProfile, Version: 2, Kinds: []wire.LayoutKind{wire.LayoutKind_LAYOUT_KIND_VIEW, wire.LayoutKind_LAYOUT_KIND_SECTION, wire.LayoutKind_LAYOUT_KIND_GRID, wire.LayoutKind_LAYOUT_KIND_HORIZONTAL_STACK, wire.LayoutKind_LAYOUT_KIND_VERTICAL_STACK, wire.LayoutKind_LAYOUT_KIND_HEADING, wire.LayoutKind_LAYOUT_KIND_TILE, wire.LayoutKind_LAYOUT_KIND_RECIPE, wire.LayoutKind_LAYOUT_KIND_POPUP, wire.LayoutKind_LAYOUT_KIND_CHIP_GROUP, wire.LayoutKind_LAYOUT_KIND_UNSUPPORTED}}
}
func TestNHF009FixtureCatalogueResolvesOnlyCapturedDefinitions(t *testing.T) {
	root := filepath.Join("..", "..", "qualification", "frontend", "recipes")
	devices, err := nhf009ReadDevices(filepath.Join(root, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	for _, device := range devices {
		if err := catalogue.AdmitDriverCapability(device.capability()); err != nil {
			t.Fatal(device.kind, err)
		}
		if _, err := device.observation(false); err != nil {
			t.Fatal(err)
		}
		if _, err := device.observation(true); err != nil {
			t.Fatal(err)
		}
	}
	config, err := (&nhf007Catalogue{file: filepath.Join(root, "catalogue.json")}).load()
	if err != nil {
		t.Fatal(err)
	}
	var results []imported.Result
	for _, entry := range config.Dashboards {
		id, _ := authorization.ParseDashboardID(entry.ID)
		source, err := imported.NewSource(id, entry.URLPath, entry.File)
		if err != nil {
			t.Fatal(err)
		}
		result, err := imported.ImportWithProfile(root, source, imported.CompilationRecipes)
		if err != nil || result.Status() != imported.StatusSupported {
			t.Fatalf("%s: %v %+v", entry.File, err, result.Diagnostics())
		}
		results = append(results, result)
	}
	resolve := func(alias domain.HAAlias) (domain.EntityID, bool) {
		text, err := alias.Value()
		if err != nil {
			return domain.EntityID{}, false
		}
		id, err := domain.ParseEntityID("synthetic:" + text)
		return id, err == nil
	}
	layouts, err := projectCatalogueLayouts(results, nhf009Support(), resolve)
	if err != nil {
		t.Fatal(err)
	}
	var cross, popup bool
	for _, node := range layouts[0].Nodes {
		for _, gesture := range node.GetRecipes().GetInteractions() {
			if destination := gesture.GetNavigate(); destination != nil {
				cross = cross || destination.DashboardId.Value == "synthetic-secondary"
				popup = popup || destination.Presentation == wire.NativePresentation_NATIVE_PRESENTATION_POPUP
			}
		}
	}
	if !cross || !popup {
		t.Fatal("configured cross-dashboard and popup routes were not resolved")
	}
	isolated, err := projectCatalogueLayouts(results[:1], nhf009Support(), resolve)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range isolated[0].Nodes {
		for _, gesture := range node.GetRecipes().GetInteractions() {
			if destination := gesture.GetNavigate(); destination != nil && destination.DashboardId.Value == "synthetic-secondary" {
				t.Fatal("uncaptured dashboard escaped navigation index")
			}
		}
	}
	var diagnostic bool
	for _, node := range isolated[0].Nodes {
		diagnostic = diagnostic || node.Kind == wire.LayoutKind_LAYOUT_KIND_UNSUPPORTED && node.Source != nil && node.Source.Path != "" && node.Diagnostic != ""
	}
	if !diagnostic {
		t.Fatal("unresolved external destination lost its sourced refusal")
	}
	if (&nhf005Dashboard{}).compilationProfile() != imported.CompilationBasic {
		t.Fatal("historical fixtures changed default profile")
	}
}
