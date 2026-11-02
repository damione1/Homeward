package configplans

import (
	"strings"
	"testing"
)

func TestDashboardWidgetPlansClassifyExactConfiguredReferencesAndOptions(t *testing.T) {
	source := `homeward: {profile: homeward-dashboard-3, owner: configured-owner, url_path: configured}
views:
 - type: sections
   path: home
   title: Configured
   minimum_column_width: 280
   gap: 0
   sections:
    - type: grid
      cards:
       - type: custom:homeward-security
         title: Exact security
         entities: [light.basement_lights]
         grid_options:
          responsive_spans: [{columns: 4, span: 2}]
`
	first := nha053Candidate(t, Dashboard, "dashboard.configured", source)
	compiled := nha053Compile(t, first)
	if compiled.effect != DashboardEffect || !compiled.refs["light.basement_lights"] {
		t.Fatal("widget binding did not enter dashboard plan admission")
	}
	changed := nha053Candidate(t, Dashboard, "dashboard.configured", strings.Replace(source, "span: 2", "span: 3", 1))
	other := nha053Compile(t, changed)
	if compiled.digest == other.digest {
		t.Fatal("configured responsive composition lost semantic hash")
	}
	excluded := nha053Candidate(t, Dashboard, "dashboard.configured", strings.ReplaceAll(source, "light.basement_lights", "light.excluded_outside_manifest"))
	if _, diagnostic := compile(excluded, nha053Manifest(t)); diagnostic != ExcludedReference {
		t.Fatalf("excluded widget alias admitted: %v", diagnostic)
	}
	malformed := nha053Candidate(t, Dashboard, "dashboard.configured", strings.Replace(source, "span: 2", "span: 5", 1))
	if _, diagnostic := compile(malformed, nha053Manifest(t)); diagnostic != UnsupportedSource {
		t.Fatalf("malformed span admitted: %v", diagnostic)
	}
}
