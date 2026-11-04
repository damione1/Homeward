package server

import (
	"path/filepath"
	"testing"

	imported "github.com/damione1/Homeward/compat/ha/dashboard"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/authorization"
)

func TestNHF009CapturedCorpusRetainsAllTwentyViewRoutes(t *testing.T) {
	root := filepath.Join("..", "..", "compat", "ha", "dashboard", "testdata", "nhf009")
	id, err := authorization.ParseDashboardID("synthetic-corpus")
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, tc := range []struct {
		file, url string
		views     int
	}{
		{"dashboard-alarm.inventory.yaml", "dashboard-alarm", 1},
		{"dashboard-tablet.inventory.yaml", "dashboard-tablet", 1},
		{"default.inventory.yaml", "lovelace", 18},
	} {
		t.Run(tc.url, func(t *testing.T) {
			source, err := imported.NewSource(id, tc.url, tc.file)
			if err != nil {
				t.Fatal(err)
			}
			result, err := imported.ImportWithProfile(root, source, imported.CompilationRecipes)
			if err != nil {
				t.Fatal(err)
			}
			definition, present := result.Definition()
			if !present {
				t.Fatal("captured source definition missing")
			}
			layout, err := projectDashboardLayout(result, nhf007Resolve)
			if err != nil {
				t.Fatal(err)
			}
			if len(layout.Views) != tc.views {
				t.Fatal("source views lost", len(layout.Views), tc.views)
			}
			nodes := map[string]*api.DashboardLayoutNode{}
			for _, node := range layout.Nodes {
				nodes[node.Id] = node
			}
			seen := map[string]bool{}
			for index, id := range layout.Views {
				node := nodes[id]
				if node == nil || node.GetView() == nil || seen[node.GetView().Route] || node.GetView().Route != definition.Views()[index].NativeRoute() || node.DisplayName != definition.Views()[index].Title() {
					t.Fatal("source route/title lost")
				}
				seen[node.GetView().Route] = true
			}

			total += len(layout.Views)
		})
	}
	if total != 20 {
		t.Fatalf("only %d of twenty captured views have coherent routes", total)
	}
}
