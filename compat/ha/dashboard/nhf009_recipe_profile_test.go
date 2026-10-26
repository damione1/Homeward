package dashboard

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const nhf009RecipeSource = `kiosk_mode: {kiosk: true}
views:
- title: Synthetic Home
  sections:
  - type: grid
    cards:
    - type: custom:bubble-card
      card_type: button
      name: Synthetic Room
      button_action:
        tap_action: {action: navigate, navigation_path: '#synthetic-room'}
    - type: custom:bubble-card
      card_type: pop-up
      hash: '#synthetic-room'
      name: Synthetic Room
      sub_button: {main: [], bottom: []}
      cards:
      - type: tile
        entity: light.synthetic
        features: [{type: light-brightness}]
        tap_action: {action: none}
    - type: custom:mushroom-template-card
      primary: Native page
      secondary: ''
      card_mod: {style: 'ha-card { color: red; }'}
      tap_action: {action: navigate, navigation_path: /lovelace/second}
  badges:
  - type: custom:mushroom-chips-card
    chips:
    - type: template
      content: Native chip
      tap_action: {action: none}
  footer:
    card:
      type: horizontal-stack
      cards:
      - type: custom:mushroom-template-badge
        primary: Back home
        tap_action: {action: navigate, navigation_path: /lovelace/0}
- type: sections
  title: Second
  path: second
  sections: []
`

func nhf009ImportRecipe(t *testing.T, content string) Result {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "recipe.yaml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := ImportWithProfile(root, nha059Source(t, "synthetic_dashboard", "lovelace", "recipe.yaml"), CompilationRecipes)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestNHF009RecipeProfileAdmitsExplicitNativeStructureAndPreservesRefusedPresentation(t *testing.T) {
	result := nhf009ImportRecipe(t, nhf009RecipeSource)
	definition, present := result.Definition()
	if !present || result.Status() == StatusFatal {
		t.Fatalf("missing definition: %v", result.Diagnostics())
	}
	if definition.Profile() != RecipesProfile || definition.Requirements().Version() != 2 || len(definition.Views()) != 2 {
		t.Fatal("unversioned extended profile")
	}
	view := definition.Views()[0]
	if view.Status() != StatusSupported || view.Path() != "" || len(view.Sections()) != 3 {
		t.Fatalf("view=%+v", view)
	}
	cards := view.Sections()[0].Cards()
	if cards[0].Recipe() != RecipeBubbleButton || cards[0].Status() != StatusSupported || cards[0].Actions()[0].Kind() != ActionNavigate {
		t.Fatal("Bubble button action not admitted")
	}
	popup := cards[1]
	anchor, valid := popup.Anchor()
	anchorValue, anchorError := anchor.Value()
	if popup.Kind() != ElementPopupCard || !valid || anchorError != nil || anchorValue != "#synthetic-room" || len(popup.Children()) != 1 || popup.Status() != StatusSupported {
		t.Fatal("popup body not owned by its configured anchor")
	}
	if popup.Children()[0].Features()[0].Kind() != FeatureBrightness || popup.Children()[0].Actions()[0].Kind() != ActionNone {
		t.Fatal("feature or explicit disabled tap lost")
	}
	if cards[2].Status() != StatusSupported || cards[2].Recipe() != RecipeTemplate || cards[2].Secondary() != "" {
		t.Fatal("static Mushroom recipe not admitted")
	}
	chips := view.Sections()[1].Cards()[0]
	if chips.Kind() != ElementChipGroup || chips.Children()[0].Recipe() != RecipeTemplateChip || chips.Children()[0].Actions()[0].Kind() != ActionNone {
		t.Fatal("chips not compiled independently")
	}
	var retainedCSS, retainedKiosk bool
	for _, item := range result.Unsupported() {
		retainedCSS = retainedCSS || item.Location().Path() == "$.views[0].sections[0].cards[2].card_mod"
		retainedKiosk = retainedKiosk || item.Location().Path() == "$.kiosk_mode"
	}
	if !retainedCSS || !retainedKiosk || !bytes.Equal(mustEntryBytes(t, result), []byte(nhf009RecipeSource)) {
		t.Fatal("refused properties or original source lost")
	}
	requirements := definition.Requirements()
	client, err := NewClientSupport(2, requirements.Elements(), requirements.Actions(), requirements.Visibility())
	if err != nil || !requirements.SupportedBy(client) {
		t.Fatal("extended native client cannot negotiate requirements")
	}
	if _, err := NewClientSupport(1, requirements.Elements(), requirements.Actions(), requirements.Visibility()); err == nil {
		t.Fatal("old client can claim new recipe elements")
	}
	legacy, err := Restore(result.Source(), result.Sources())
	if err != nil || legacy.Status() != StatusUnsupported {
		t.Fatal("legacy profile changed silently")
	}
	restored, err := RestoreWithProfile(result.Source(), result.Sources(), CompilationRecipes)
	if err != nil || restored.Status() != result.Status() {
		t.Fatal("detached extended restore drift")
	}
}

func TestNHF009FullCorpusCanBeDiagnosedByTheExplicitRecipeProfileWithoutLosingViews(t *testing.T) {
	for _, tc := range []struct {
		file, url string
		views     int
	}{
		{"map.inventory.yaml", "map", 0},
		{"dashboard-home.inventory.yaml", "dashboard-home", 0},
		{"dashboard-alarm.inventory.yaml", "dashboard-alarm", 1},
		{"dashboard-tablet.inventory.yaml", "dashboard-tablet", 1},
		{"default.inventory.yaml", "lovelace", 18},
	} {
		t.Run(tc.url, func(t *testing.T) {
			result, err := ImportWithProfile("testdata/nhf009", nha059Source(t, "synthetic_dashboard", tc.url, tc.file), CompilationRecipes)
			if err != nil {
				t.Fatal(err)
			}
			definition, present := result.Definition()
			if !present || result.Status() == StatusFatal || len(definition.Views()) != tc.views {
				t.Fatalf("views lost: status=%v diagnostics=%v", result.Status(), result.Diagnostics())
			}
			original, err := os.ReadFile(filepath.Join("testdata/nhf009", tc.file))
			if err != nil || !bytes.Equal(mustEntryBytes(t, result), original) {
				t.Fatal("source rewritten")
			}
			t.Logf("views=%d diagnostics=%d", len(definition.Views()), len(result.Diagnostics()))
		})
	}
}

func TestNHF009AmbiguousGestureTemplateAndFunctionalUnknownAreInert(t *testing.T) {
	for _, properties := range []string{
		"primary: '{{ states(\"sensor.synthetic\") }}'",
		"primary: Static\n      card_type: button",
		"primary: Static\n      browser_mod: {service: anything}",
		"primary: Static\n      button_action: {tap_action: {action: none}}",
	} {
		result := nhf009ImportRecipe(t, "views:\n- type: sections\n  title: Main\n  path: main\n  sections:\n  - type: grid\n    cards:\n    - type: custom:mushroom-template-card\n      "+properties+"\n")
		definition, present := result.Definition()
		if !present {
			t.Fatal("definition discarded")
		}
		card := definition.Views()[0].Sections()[0].Cards()[0]
		if card.Status() != StatusUnsupported || len(result.Unsupported()) == 0 {
			t.Fatal("functional unknown or template was admitted")
		}
		for _, action := range card.Actions() {
			if action.Status() == StatusSupported {
				t.Fatal("inert recipe still publishes executable actions")
			}
		}
	}
	ambiguous := nhf009ImportRecipe(t, strings.Replace(nhf009RecipeSource, "      button_action:", "      tap_action: {action: none}\n      button_action:", 1))
	definition, present := ambiguous.Definition()
	if !present {
		t.Fatal("missing ambiguous definition")
	}
	card := definition.Views()[0].Sections()[0].Cards()[0]
	if card.Status() != StatusUnsupported {
		t.Fatal("direct and nested Bubble gestures chose an arbitrary winner")
	}
	for _, action := range card.Actions() {
		if action.Status() == StatusSupported {
			t.Fatal("ambiguous gesture remained actionable")
		}
	}
}
