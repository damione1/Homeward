package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	wire "github.com/damione1/Homeward/gen/home/dashboard/v1"
	"github.com/damione1/Homeward/internal/domain"
)

func nhf009LayoutSupport() *wire.ClientLayoutSupport {
	return &wire.ClientLayoutSupport{Profile: RecipesProfile, Version: 2, Kinds: []wire.LayoutKind{
		wire.LayoutKind_LAYOUT_KIND_VIEW, wire.LayoutKind_LAYOUT_KIND_SECTION, wire.LayoutKind_LAYOUT_KIND_GRID,
		wire.LayoutKind_LAYOUT_KIND_HORIZONTAL_STACK, wire.LayoutKind_LAYOUT_KIND_VERTICAL_STACK,
		wire.LayoutKind_LAYOUT_KIND_HEADING, wire.LayoutKind_LAYOUT_KIND_TILE, wire.LayoutKind_LAYOUT_KIND_RECIPE,
		wire.LayoutKind_LAYOUT_KIND_POPUP, wire.LayoutKind_LAYOUT_KIND_CHIP_GROUP,
	}}
}

func nhf009ResolveEverySyntheticAlias(alias domain.HAAlias) (domain.EntityID, bool) {
	value, err := alias.Value()
	if err != nil {
		return domain.EntityID{}, false
	}
	id, err := domain.ParseEntityID("synthetic-id:" + value)
	return id, err == nil
}

func TestNHF009NativeLayoutCarriesOnlyNegotiatedRecipesAndResolvesThePopupBody(t *testing.T) {
	result := nhf009ImportRecipe(t, nhf009RecipeSource)
	layout, err := ProjectNativeLayout(result, LayoutProjection{Support: nhf009LayoutSupport(), Entity: nhf009ResolveEverySyntheticAlias})
	if err != nil {
		t.Fatal(err)
	}
	if layout.Version != 2 || len(layout.Views) != 2 {
		t.Fatal("wrong extended wire layout version")
	}
	var button, popup, light *wire.LayoutNode
	for _, node := range layout.Nodes {
		switch node.Id {
		case "$.views[0].sections[0].cards[0]":
			button = node
		case "$.views[0].sections[0].cards[1]":
			popup = node
		case "$.views[0].sections[0].cards[1].cards[0]":
			light = node
		}
	}
	if button == nil || popup == nil || light == nil {
		t.Fatal("source nodes omitted")
	}
	if button.Kind != wire.LayoutKind_LAYOUT_KIND_RECIPE || button.RecipeKind != wire.NativeRecipeKind_NATIVE_RECIPE_KIND_BUBBLE_BUTTON || button.Source.Variant != "custom:bubble-card" {
		t.Fatal("typed Bubble recipe/source missing")
	}
	if button.Recipes == nil || button.Recipes.Interactions[0].GetNavigate().NodeId != popup.Id || button.Recipes.Interactions[0].GetCommand() != nil {
		t.Fatal("popup link not resolved to its exact body or became an action")
	}
	if popup.Kind != wire.LayoutKind_LAYOUT_KIND_POPUP || len(popup.Children) != 1 || popup.Children[0] != light.Id {
		t.Fatal("popup body lost its configured ownership")
	}
	if light.EntityId != "synthetic-id:light.synthetic" || len(light.Features) != 1 || light.Features[0] != wire.NativeFeatureKind_NATIVE_FEATURE_KIND_BRIGHTNESS || light.Recipes.Interactions[0].GetNone() == nil {
		t.Fatal("exact entity, feature or disabled tap lost")
	}
	for _, support := range []*wire.ClientLayoutSupport{nil, {Profile: Profile, Version: 1}, {Profile: RecipesProfile, Version: 99}} {
		inert, err := ProjectNativeLayout(result, LayoutProjection{Support: support, Entity: nhf009ResolveEverySyntheticAlias})
		if err != nil {
			t.Fatal(err)
		}
		for _, node := range inert.Nodes {
			if node.Kind != wire.LayoutKind_LAYOUT_KIND_UNSUPPORTED || node.EntityId != "" || node.Recipes != nil || node.RecipeKind != wire.NativeRecipeKind_NATIVE_RECIPE_KIND_UNSPECIFIED || len(node.Features) != 0 {
				t.Fatal("unnegotiated native recipe escaped")
			}
		}
	}
}

func TestNHF009UnresolvedRecipeAndUnsupportedAncestorCannotPublishPartialCommands(t *testing.T) {
	result := nhf009ImportRecipe(t, strings.Replace(nhf009RecipeSource, "#synthetic-room'}", "#missing'}", 1))
	layout, err := ProjectNativeLayout(result, LayoutProjection{Support: nhf009LayoutSupport(), Entity: nhf009ResolveEverySyntheticAlias})
	if err != nil {
		t.Fatal(err)
	}
	if layout.Nodes[2].Kind != wire.LayoutKind_LAYOUT_KIND_UNSUPPORTED || layout.Nodes[2].Recipes != nil {
		t.Fatal("unresolved navigation became a default action")
	}
	support := nhf009LayoutSupport()
	var kinds []wire.LayoutKind
	for _, kind := range support.Kinds {
		if kind != wire.LayoutKind_LAYOUT_KIND_POPUP {
			kinds = append(kinds, kind)
		}
	}
	support.Kinds = kinds
	layout, err = ProjectNativeLayout(nhf009ImportRecipe(t, nhf009RecipeSource), LayoutProjection{Support: support, Entity: nhf009ResolveEverySyntheticAlias})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range layout.Nodes {
		if strings.HasPrefix(node.Id, "$.views[0].sections[0].cards[1]") && (node.Kind != wire.LayoutKind_LAYOUT_KIND_UNSUPPORTED || node.EntityId != "" || node.Recipes != nil) {
			t.Fatal("unsupported popup parent leaked actionable child")
		}
	}
}

func TestNHF009BoundedOrVisibilityKeepsEveryBranchAndRefusesUnboundHAUsers(t *testing.T) {
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
          state_not: ''
`
	result := nhf009ImportRecipe(t, source)
	definition, present := result.Definition()
	if !present || result.Status() != StatusSupported {
		t.Fatalf("visibility profile not admitted: %+v", result.Diagnostics())
	}
	layout, err := ProjectNativeLayout(result, LayoutProjection{Support: nhf009LayoutSupport(), Entity: nhf009ResolveEverySyntheticAlias})
	if err != nil {
		t.Fatal(err)
	}
	branches := layout.Nodes[2].Recipes.Visibility.GetAll().Conditions[0].GetAny().Conditions
	if len(branches) != 2 || branches[1].GetState().State != "" || branches[1].GetState().Comparison != wire.NativeComparison_NATIVE_COMPARISON_NOT_EQUAL {
		t.Fatal("or branch, negative predicate or empty string lost")
	}
	if !contains(definition.Requirements().Visibility(), VisibilityAny) || !contains(definition.Requirements().Visibility(), VisibilityState) {
		t.Fatal("nested predicate requirements omitted")
	}
	result = nhf009ImportRecipe(t, strings.Replace(source, "- condition: or\n        conditions:", "- condition: user\n        conditions:", 1))
	layout, err = ProjectNativeLayout(result, LayoutProjection{Support: nhf009LayoutSupport(), Entity: nhf009ResolveEverySyntheticAlias})
	if err != nil {
		t.Fatal(err)
	}
	if layout.Nodes[2].Kind != wire.LayoutKind_LAYOUT_KIND_UNSUPPORTED || layout.Nodes[2].Recipes != nil || layout.Nodes[2].EntityId != "" {
		t.Fatal("HA user visibility became native account authorization")
	}
}

func TestNHF009NavigationIndexFromAnOlderSourceCannotBindNewRecipes(t *testing.T) {
	old := nhf009ImportRecipe(t, nhf009RecipeSource)
	definition, present := old.Definition()
	if !present {
		t.Fatal("missing old definition")
	}
	index, err := NewNavigationIndex([]Definition{definition})
	if err != nil {
		t.Fatal(err)
	}
	current := nhf009ImportRecipe(t, strings.Replace(nhf009RecipeSource, "primary: Native page", "primary: Changed page", 1))
	if _, err := ProjectNativeLayout(current, LayoutProjection{Support: nhf009LayoutSupport(), Entity: nhf009ResolveEverySyntheticAlias, Navigation: &index}); err == nil {
		t.Fatal("old source navigation was mixed with a new document")
	}
}

func TestNHF009RecipeClientCanConsumeBasicTreeWithoutUpgradingItsSemantics(t *testing.T) {
	root := t.TempDir()
	source := `views:
- type: sections
  title: Synthetic basic
  path: home
  sections:
  - type: grid
    cards:
    - type: tile
      entity: light.synthetic
`
	if err := os.WriteFile(filepath.Join(root, "basic.yaml"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := Import(root, nha059Source(t, "synthetic_basic", "lovelace", "basic.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	layout, err := ProjectNativeLayout(result, LayoutProjection{Support: nhf009LayoutSupport(), Entity: nhf009ResolveEverySyntheticAlias})
	if err != nil {
		t.Fatal(err)
	}
	if layout.Version != 1 {
		t.Fatal("Basic semantics were upgraded implicitly")
	}
	var tile bool
	for _, node := range layout.Nodes {
		tile = tile || node.Kind == wire.LayoutKind_LAYOUT_KIND_TILE
		if node.Recipes != nil || node.RecipeKind != wire.NativeRecipeKind_NATIVE_RECIPE_KIND_UNSPECIFIED {
			t.Fatal("Basic tree acquired recipes")
		}
	}
	if !tile {
		t.Fatalf("v2 renderer lost the Basic tile: diagnostics=%+v nodes=%+v", result.Diagnostics(), layout.Nodes)
	}
}
