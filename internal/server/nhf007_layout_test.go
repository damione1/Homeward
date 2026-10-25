package server

import (
	"os"
	"path/filepath"
	"testing"

	imported "github.com/damione1/Homeward/compat/ha/dashboard"
	dashboardv1 "github.com/damione1/Homeward/gen/home/dashboard/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
)

func nhf007Import(t *testing.T, source string) imported.Result {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "dashboard.yaml"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	id, _ := authorization.ParseDashboardID("synthetic-layout")
	input, err := imported.NewSource(id, "synthetic", "dashboard.yaml")
	if err != nil {
		t.Fatal(err)
	}
	result, err := imported.Import(root, input)
	if err != nil || result.Status() == imported.StatusFatal {
		t.Fatalf("import: %v %v", err, result.Diagnostics())
	}
	return result
}

func nhf007Resolve(alias domain.HAAlias) (domain.EntityID, bool) {
	value, err := alias.Value()
	if err != nil {
		return domain.EntityID{}, false
	}
	id, err := domain.ParseEntityID("synthetic:" + value)
	return id, err == nil
}

func TestNHF007ProjectionRetainsContainersUnknownNamesSourceAndNegotiation(t *testing.T) {
	result := nhf007Import(t, `views:
  - type: sections
    path: home
    title: Synthetic home
    max_columns: 3
    sections:
      - type: grid
        cards:
          - type: heading
            heading: Comfort
            heading_style: title
          - type: grid
            columns: 2
            square: false
            cards:
              - type: tile
                entity: sensor.synthetic_temperature
                name: Temperature
                grid_options:
                  columns: full
                  rows: auto
              - type: horizontal-stack
                cards:
                  - type: tile
                    entity: light.synthetic_lamp
                  - type: custom:unknown-card
                    entity: switch.private
`)
	layout, err := projectDashboardLayout(result, nil, nhf007Resolve)
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Views) != 1 || len(layout.Nodes) != 8 {
		t.Fatalf("forest lost structure: %+v", layout)
	}
	counts := map[dashboardv1.LayoutKind]int{}
	for _, node := range layout.Nodes {
		counts[node.Kind]++
		if node.Source.File != "dashboard.yaml" || node.Source.Line == 0 || node.Source.Path != node.Id {
			t.Fatalf("source identity lost: %+v", node)
		}
		if node.Kind == dashboardv1.LayoutKind_LAYOUT_KIND_UNSUPPORTED {
			if node.Source.Variant != "custom:unknown-card" || node.Diagnostic == "" || node.EntityId != "" {
				t.Fatalf("unknown card must stay visible and inert: %+v", node)
			}
		}
		if node.Title == "Temperature" && (node.EntityId != "synthetic:sensor.synthetic_temperature" || node.ColumnSpan.GetKind() != dashboardv1.LayoutDimensionKind_LAYOUT_DIMENSION_KIND_FULL || node.RowSpan.GetKind() != dashboardv1.LayoutDimensionKind_LAYOUT_DIMENSION_KIND_AUTO) {
			t.Fatalf("tile lost exact binding or dimensions: %+v", node)
		}
	}
	for _, kind := range []dashboardv1.LayoutKind{dashboardv1.LayoutKind_LAYOUT_KIND_VIEW, dashboardv1.LayoutKind_LAYOUT_KIND_SECTION, dashboardv1.LayoutKind_LAYOUT_KIND_GRID, dashboardv1.LayoutKind_LAYOUT_KIND_HORIZONTAL_STACK, dashboardv1.LayoutKind_LAYOUT_KIND_HEADING, dashboardv1.LayoutKind_LAYOUT_KIND_UNSUPPORTED} {
		if counts[kind] != 1 {
			t.Fatalf("missing kind %v: %v", kind, counts)
		}
	}
	for _, support := range []*dashboardv1.ClientLayoutSupport{
		{Profile: imported.Profile, Version: 99},
		{Profile: "future-profile", Version: 1},
		{Profile: imported.Profile, Version: 1, Kinds: []dashboardv1.LayoutKind{dashboardv1.LayoutKind_LAYOUT_KIND_VIEW}},
	} {
		negotiated, err := projectDashboardLayout(result, support, nhf007Resolve)
		if err != nil {
			t.Fatal(err)
		}
		for _, node := range negotiated.Nodes {
			if node.EntityId != "" || (node.Kind != dashboardv1.LayoutKind_LAYOUT_KIND_UNSUPPORTED && node.Kind != dashboardv1.LayoutKind_LAYOUT_KIND_VIEW) {
				t.Fatalf("unsupported negotiation admitted content: %+v", node)
			}
		}
	}
}

func TestNHF007RuntimeStrategyIsSourcedAndInert(t *testing.T) {
	result := nhf007Import(t, "strategy:\n  type: custom:unknown-strategy\n")
	layout, err := projectDashboardLayout(result, nil, nhf007Resolve)
	if err != nil || len(layout.Views) != 1 || len(layout.Nodes) != 1 {
		t.Fatalf("strategy was dropped: %v %+v", err, layout)
	}
	node := layout.Nodes[0]
	if node.Kind != dashboardv1.LayoutKind_LAYOUT_KIND_UNSUPPORTED || node.Source.Variant != "custom:unknown-strategy" || node.Diagnostic == "" || node.EntityId != "" {
		t.Fatalf("strategy must stay visible and inert: %+v", node)
	}
}
