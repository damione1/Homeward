package dashboard

import (
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"strings"
	"testing"
)

func TestNHF009NativeLayoutResolvesExactPopupAndCapability(t *testing.T) {
	layout := projectAPIFixture(t, nhf009ImportRecipe(t, nhf009RecipeSource))
	if len(layout.Views) != 2 {
		t.Fatal("configured roots lost")
	}
	nodes := map[string]*api.DashboardLayoutNode{}
	for _, n := range layout.Nodes {
		nodes[n.Id] = n
	}
	button := nodes["$.views[0].sections[0].cards[0]"]
	popup := nodes["$.views[0].sections[0].cards[1]"]
	light := nodes["$.views[0].sections[0].cards[1].cards[0]"]
	if button.GetRecipe() == nil || button.Source.Variant != "custom:bubble-card" || popup.GetPopup() == nil || light.GetTile() == nil {
		t.Fatal("closed node alternatives missing")
	}
	actions := button.GetRecipe().GetInteractions().GetInteractions()
	if len(actions) != 1 || actions[0].GetNavigate().GetNodeId() != popup.Id || actions[0].GetCommand() != nil {
		t.Fatal("navigation became a command or changed target")
	}
	children := popup.GetPopup().GetChildren().GetChildren()
	if len(children) != 1 || children[0] != light.Id {
		t.Fatal("popup ownership lost")
	}
	if _, err := apitypes.ParseCapabilityName(light.GetTile().Capability); err != nil {
		t.Fatal(err)
	}
	if len(light.GetTile().Features) != 1 || light.GetTile().Features[0] != api.DashboardFeatureKind_DASHBOARD_FEATURE_KIND_BRIGHTNESS || light.GetTile().GetInteractions().GetInteractions()[0].GetNone() == nil {
		t.Fatal("brightness or explicit no-op lost")
	}
}

func TestNHF009UnresolvedRecipeCannotPublishPartialCommands(t *testing.T) {
	result := nhf009ImportRecipe(t, strings.Replace(nhf009RecipeSource, "#synthetic-room'}", "#missing'}", 1))
	layout := projectAPIFixture(t, result)
	if layout.Nodes[2].GetUnsupported() == nil || layout.Nodes[2].Visibility != nil {
		t.Fatal("unresolved navigation became executable")
	}
}

func TestNHF009BoundedOrVisibilityKeepsTypedBranchesAndRefusesHAUsers(t *testing.T) {
	source := `views:
- type: sections
  title: Home
  path: main
  sections:
  - type: grid
    cards:
    - type: tile
      entity: light.synthetic
      visibility:
      - condition: or
        conditions:
        - condition: state
          entity: input_boolean.synthetic
          state: 'on'
        - condition: state
          entity: input_boolean.synthetic
          state_not: 'off'
`
	layout := projectAPIFixture(t, nhf009ImportRecipe(t, source))
	branches := layout.Nodes[2].GetVisibility().GetAll().GetConditions()[0].GetAny().GetConditions()
	if len(branches) != 2 || branches[1].GetState().GetComparison() != api.DashboardComparison_DASHBOARD_COMPARISON_NOT_EQUAL {
		t.Fatal("typed disjunction lost")
	}
	for _, invalid := range []string{strings.Replace(source, "state_not: 'off'", "state_not: ''", 1), strings.Replace(source, "- condition: or\n        conditions:", "- condition: user\n        conditions:", 1)} {
		projected := projectAPIFixture(t, nhf009ImportRecipe(t, invalid))
		if projected.Nodes[2].GetUnsupported() == nil {
			t.Fatal("unknown typed state or HA user became native authority")
		}
	}
}

func TestNHF009NavigationIndexFromAnOlderSourceCannotBindNewRecipes(t *testing.T) {
	old, _ := nhf009ImportRecipe(t, nhf009RecipeSource).Definition()
	index, err := NewNavigationIndex([]Definition{old})
	if err != nil {
		t.Fatal(err)
	}
	current, _ := nhf009ImportRecipe(t, strings.Replace(nhf009RecipeSource, "primary: Native page", "primary: Changed page", 1)).Definition()
	options := apiFixtureProjection(t)
	options.Navigation = &index
	if _, err = ProjectDashboardDefinition(current, options); err == nil {
		t.Fatal("mixed source captures admitted")
	}
}
