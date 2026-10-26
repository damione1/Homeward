package server

import (
	"crypto/sha256"
	"path/filepath"
	"testing"

	imported "github.com/damione1/Homeward/compat/ha/dashboard"
	dashboardv1 "github.com/damione1/Homeward/gen/home/dashboard/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/view"
)

func TestNHF009CapturedCorpusOverviewAndNativeLayoutShareAllTwentyViewRoutes(t *testing.T) {
	root := filepath.Join("..", "..", "compat", "ha", "dashboard", "testdata", "nhf009")
	stamp, err := view.NewConfigStamp(1, sha256.Sum256([]byte("synthetic corpus configuration")))
	if err != nil {
		t.Fatal(err)
	}
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
			overview, err := projectDashboardOverview(definition, stamp, 1, dashboardv1.ServingMode_SERVING_MODE_SYNTHETIC, nhf007Resolve)
			if err != nil {
				t.Fatalf("captured source cannot be served as a dashboard: %v", err)
			}
			layout, err := projectDashboardLayout(result, nil, nhf007Resolve)
			if err != nil {
				t.Fatal(err)
			}
			if len(overview.Rooms)+1 != tc.views || len(layout.Views) != tc.views {
				t.Fatalf("source views lost: overview=%d layout=%d expected=%d", len(overview.Rooms), len(layout.Views), tc.views)
			}
			if overview.Title != definition.Views()[0].Title() {
				t.Fatal("homepage title does not come from the first captured view")
			}
			seen := map[string]bool{}
			for index, room := range overview.Rooms {
				if room.Id == "" || seen[room.Id] || room.Title != definition.Views()[index+1].Title() {
					t.Fatal("native overview lost its distinct source view identity or title")
				}
				seen[room.Id] = true
				found := false
				for _, node := range layout.Nodes {
					if node.Id == layout.Views[index+1] {
						found = node.Route == room.Id && node.Title == room.Title
					}
				}
				if !found {
					t.Fatal("overview route does not resolve to its exact captured native view")
				}
			}
			total += len(overview.Rooms) + 1
		})
	}
	if total != 20 {
		t.Fatalf("only %d of twenty captured views have coherent routes", total)
	}
}
