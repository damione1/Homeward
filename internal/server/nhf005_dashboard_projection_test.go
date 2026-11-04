package server

import (
	"path/filepath"
	"testing"

	imported "github.com/damione1/Homeward/compat/ha/dashboard"
	"github.com/damione1/Homeward/internal/authorization"
)

func TestNHF005DashboardProjectionUsesExternalSourceAndExactBindings(t *testing.T) {
	id, err := authorization.ParseDashboardID("synthetic-dashboard")
	if err != nil {
		t.Fatal(err)
	}
	for _, filename := range []string{"home.yaml", "alternate.yaml"} {
		source, err := imported.NewSource(id, "synthetic", filename)
		if err != nil {
			t.Fatal(err)
		}
		root, err := filepath.Abs("../../qualification/frontend/dashboards")
		if err != nil {
			t.Fatal(err)
		}
		result, err := imported.Import(root, source)
		if err != nil || result.Status() != imported.StatusSupported {
			t.Fatal(filename, err, result.Diagnostics())
		}
		definition, ok := result.Definition()
		if !ok {
			t.Fatal("missing source definition")
		}
		layout, err := projectDashboardLayout(result, nhf007Resolve)
		if err != nil || len(layout.Views) == 0 {
			t.Fatal(filename, err)
		}
		for index, id := range layout.Views {
			found := false
			for _, node := range layout.Nodes {
				if node.Id == id {
					found = node.GetView() != nil && node.GetView().Route == definition.Views()[index].NativeRoute() && node.DisplayName == definition.Views()[index].Title()
				}
			}
			if !found {
				t.Fatal("exact source view/title lost")
			}
		}
		for _, node := range layout.Nodes {
			if node.GetTile() != nil {
				t.Fatal("alias without declared native slot manufactured a tile")
			}
		}
	}
}
