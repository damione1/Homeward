package server

import (
	"os"
	"path/filepath"
	"testing"

	imported "github.com/damione1/Homeward/compat/ha/dashboard"
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

func TestNHF007ProjectionRetainsContainersUnknownNamesAndSource(t *testing.T) {
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
	layout, err := projectDashboardLayout(result, nhf007Resolve)
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Views) != 1 || len(layout.Nodes) != 8 {
		t.Fatalf("forest lost structure: %+v", layout)
	}
	var view, section, grid, stack, heading, unknown bool
	for _, node := range layout.Nodes {
		if node.Source.File != "dashboard.yaml" || node.Source.Line == 0 || node.Source.Path != node.Id {
			t.Fatal("source identity lost", node)
		}
		view = view || node.GetView() != nil
		section = section || node.GetSection() != nil
		grid = grid || node.GetGrid() != nil
		stack = stack || node.GetHorizontalStack() != nil
		heading = heading || node.GetHeading() != nil
		if node.Source.Variant == "custom:unknown-card" {
			unknown = node.GetUnsupported() != nil && node.GetUnsupported().Diagnostic != ""
		}
		if node.DisplayName == "Temperature" && node.GetUnsupported() == nil {
			t.Fatal("undeclared capability became an interactive tile")
		}
	}
	if !view || !section || !grid || !stack || !heading || !unknown {
		t.Fatal("source structure or inert diagnostic lost")
	}
}

func TestNHF007RuntimeStrategyIsSourcedAndInert(t *testing.T) {
	result := nhf007Import(t, "strategy:\n  type: custom:unknown-strategy\n")
	layout, err := projectDashboardLayout(result, nhf007Resolve)
	if err != nil || len(layout.Views) != 1 || len(layout.Nodes) != 1 {
		t.Fatalf("strategy was dropped: %v %+v", err, layout)
	}
	node := layout.Nodes[0]
	if node.GetUnsupported() == nil || node.Source.Variant != "custom:unknown-strategy" || node.GetUnsupported().Diagnostic == "" {
		t.Fatal("strategy must remain sourced and inert", node)
	}
}
