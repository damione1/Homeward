package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	controls "github.com/damione1/Homeward/gen/home/controls/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/apiresources"
	"github.com/damione1/Homeward/internal/domain"
)

func TestNHA211CoverLockIsExactIndependentAndNeverInferred(t *testing.T) {
	const source = `homeward: {profile: homeward-dashboard-3, owner: dashboard-owner, url_path: configured}
views:
- type: sections
  title: Synthetic
  path: main
  sections:
  - type: grid
    cards:
    - type: tile
      entity: cover.synthetic
      name: Synthetic door
      cover_lock: {entity: lock.synthetic, name: Remote controls}
`
	for _, variant := range []string{"tile", "custom:mushroom-template-card"} {
		t.Run(variant, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "dashboard.yaml"), []byte(strings.Replace(source, "type: tile", "type: "+variant, 1)), 0600); err != nil {
				t.Fatal(err)
			}
			result, err := ImportWithProfile(root, nha059Source(t, "synthetic_dashboard", "lovelace", "dashboard.yaml"), CompilationDashboard)
			if err != nil {
				t.Fatal(err)
			}
			definition, ok := result.Definition()
			if !ok {
				t.Fatal(result.Diagnostics())
			}
			options := apiFixtureProjection(t)
			denied := false
			ambiguous := false
			options.Capabilities = func(alias domain.HAAlias) []apiresources.Capability {
				text, _ := alias.Value()
				name, ok := options.Entity(alias)
				if !ok {
					return nil
				}
				var description *registry.CapabilityDescription
				switch text {
				case "cover.synthetic":
					description = &registry.CapabilityDescription{SchemaId: contracts.CoverSchema, Major: 1, Endpoint: "cover", DisplayName: "Door", Constraints: &registry.CapabilityDescription_Cover{Cover: &controls.CoverConstraints{ObservedStates: []controls.CoverState{controls.CoverState_COVER_STATE_CLOSED}}}}
				case "lock.synthetic":
					if denied {
						return nil
					}
					description = &registry.CapabilityDescription{SchemaId: contracts.LockSchema, Major: 1, Endpoint: "lock", DisplayName: "Lock", Constraints: &registry.CapabilityDescription_Lock{Lock: &controls.LockConstraints{ObservedStates: []controls.LockState{controls.LockState_LOCK_STATE_UNLOCKED}}}}
				default:
					return nil
				}
				identity, _ := domain.ParseEntityID("fixture:" + text)
				caps, err := apiresources.ProjectCapabilities(name.Home(), identity, []*registry.CapabilityDescription{description}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if ambiguous && text == "lock.synthetic" {
					caps = append(caps, caps...)
				}
				return caps
			}
			project := func() *api.DashboardCoverLock {
				t.Helper()
				layout, err := ProjectDashboardDefinition(definition, options)
				if err != nil {
					t.Fatal(err)
				}
				node := layout.Nodes[2]
				if node.GetUnsupported() != nil {
					t.Fatal("cover unexpectedly removed")
				}
				if node.GetTile() != nil {
					return node.GetTile().CoverLock
				}
				return node.GetRecipe().CoverLock
			}
			association := project()
			alias, _ := domain.ParseHAAlias("lock.synthetic")
			expected, _ := options.Entity(alias)
			if association == nil || !strings.HasPrefix(association.Capability, expected.String()+"/capabilities/") || association.DisplayName != "Remote controls" {
				t.Fatal("exact association missing", association)
			}
			denied = true
			if project() != nil {
				t.Fatal("unreadable lock leaked")
			}
			denied = false
			ambiguous = true
			if project() != nil {
				t.Fatal("ambiguous lock selected")
			}
		})
	}
}

func TestNHA211MalformedCoverAssociationsAreInert(t *testing.T) {
	for _, association := range []string{"{entity: lock.synthetic}", "{name: Remote}", "{entity: lock.synthetic, name: Remote, extra: 1}", "{entity: bad, name: Remote}", "{entity: lock.synthetic, name: ''}"} {
		root := t.TempDir()
		source := "homeward: {profile: homeward-dashboard-3, owner: dashboard-owner, url_path: configured}\nviews:\n- type: sections\n  title: Synthetic\n  sections:\n  - type: grid\n    cards:\n    - type: tile\n      entity: light.synthetic\n      cover_lock: " + association + "\n"
		if err := os.WriteFile(filepath.Join(root, "dashboard.yaml"), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		result, err := ImportWithProfile(root, nha059Source(t, "synthetic_dashboard", "lovelace", "dashboard.yaml"), CompilationDashboard)
		if err != nil {
			t.Fatal(err)
		}
		layout := projectAPIFixture(t, result)
		if layout.Nodes[2].GetUnsupported() == nil {
			t.Fatal("malformed association admitted", association)
		}
	}
}
