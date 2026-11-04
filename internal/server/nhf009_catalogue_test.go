package server

import (
	"path/filepath"
	"testing"

	imported "github.com/damione1/Homeward/compat/ha/dashboard"
	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apiresources"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
)

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
	layouts, err := projectCatalogueLayouts(results, resolve)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := apitypes.ParseHomeName("homes/qualification-dashboard")
	secondary, _ := authorization.ParseDashboardID("synthetic-secondary")
	secondaryName, _ := apiresources.DashboardNameForIdentity(home, secondary)
	var cross, popup bool
	for _, node := range layouts[0].Nodes {
		for _, gesture := range node.GetRecipe().GetInteractions().GetInteractions() {
			if destination := gesture.GetNavigate(); destination != nil {
				cross = cross || destination.Dashboard == secondaryName.String()
				popup = popup || destination.Presentation == wire.DashboardPresentation_DASHBOARD_PRESENTATION_POPUP
			}
		}
	}
	if !cross || !popup {
		t.Fatal("configured cross-dashboard and popup routes were not resolved")
	}
	isolated, err := projectCatalogueLayouts(results[:1], resolve)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range isolated[0].Nodes {
		for _, gesture := range node.GetRecipe().GetInteractions().GetInteractions() {
			if destination := gesture.GetNavigate(); destination != nil && destination.Dashboard == secondaryName.String() {
				t.Fatal("uncaptured dashboard escaped navigation index")
			}
		}
	}
	var diagnostic bool
	for _, node := range isolated[0].Nodes {
		diagnostic = diagnostic || node.GetUnsupported() != nil && node.Source != nil && node.Source.Path != "" && node.GetUnsupported().Diagnostic != ""
	}
	if !diagnostic {
		t.Fatal("unresolved external destination lost its sourced refusal")
	}
}
