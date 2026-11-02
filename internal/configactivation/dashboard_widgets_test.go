package configactivation

import (
	"strings"
	"testing"

	"github.com/damione1/Homeward/compat/ha/dashboard"
	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
)

func TestPublishedDashboardWidgetsRequireTrustedOwnerAndAdmittedExactReferences(t *testing.T) {
	manifest, err := registry.LoadManifest(scope.BaselineV1())
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := storage.DashboardSourceDescriptor("dashboard.configured", "dashboard.yaml")
	if err != nil {
		t.Fatal(err)
	}
	source := `homeward: {profile: homeward-dashboard-3, owner: configured-owner, url_path: configured}
views:
 - type: sections
   path: home
   title: Configured
   sections:
    - type: grid
      cards:
       - type: custom:homeward-room
         room_id: configured-room
         temperature: {entity: light.basement_lights, field: measurement}
         activity_entities: [light.basement_lights]
         active_states: ['on']
`
	group := nha055SourceGroup(t, descriptor, "source.dashboard.configured", source)
	compiled, err := CompileSourceGroups([]SourceGroup{group}, manifest, catalogue)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := authorization.ParseDashboardID("dashboard.configured")
	definition, ok := compiled.Dashboard(id)
	owner, present := definition.Owner()
	text, _ := owner.Value()
	if !ok || !present || text != "configured-owner" || definition.Profile() != dashboard.DashboardProfile || definition.Requirements().Version() != 3 {
		t.Fatal("published source lost trusted profile/owner")
	}
	otherDescriptor, _ := storage.DashboardSourceDescriptor("dashboard.other", "dashboard.yaml")
	otherGroup := nha055SourceGroup(t, otherDescriptor, "source.dashboard.other", source)
	if _, err := CompileSourceGroups([]SourceGroup{group, otherGroup}, manifest, catalogue); err == nil {
		t.Fatal("duplicate configured URL published an ambiguous navigation index")
	}
	distinct := nha055SourceGroup(t, otherDescriptor, "source.dashboard.other", strings.Replace(source, "url_path: configured", "url_path: other", 1))
	if _, err := CompileSourceGroups([]SourceGroup{group, distinct}, manifest, catalogue); err != nil {
		t.Fatal("distinct configured URLs rejected", err)
	}
	for _, invalid := range []string{strings.Replace(source, "owner: configured-owner", "owner: ''", 1), strings.ReplaceAll(source, "light.basement_lights", "light.excluded_outside_manifest"), strings.Replace(source, "activity_entities: [light.basement_lights]", "activity_entities: [light.excluded_outside_manifest]", 1)} {
		bad := nha055SourceGroup(t, descriptor, "source.dashboard.configured", invalid)
		if _, err := CompileSourceGroups([]SourceGroup{bad}, manifest, catalogue); err == nil {
			t.Fatal("unowned or excluded widget source published")
		}
	}
}
