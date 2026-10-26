package dashboard

import (
	"strings"
	"testing"

	wire "github.com/damione1/Homeward/gen/home/dashboard/v1"
)

func TestNHF009NativeNavigationResolvesConfiguredPagesAndViewScopedPopups(t *testing.T) {
	result := nhf009ImportRecipe(t, nhf009RecipeSource)
	definition, present := result.Definition()
	if !present {
		t.Fatal("missing definition")
	}
	index, err := NewNavigationIndex([]Definition{definition})
	if err != nil {
		t.Fatal(err)
	}
	views := definition.Views()
	from := definition.Source().ID()
	popup := views[0].Sections()[0].Cards()[1]
	for _, path := range []string{"#synthetic-room", "/lovelace#synthetic-room", "/lovelace/0#synthetic-room"} {
		destination, ok := index.Resolve(from, views[0].Location(), path)
		if !ok || destination.Presentation != wire.NativePresentation_NATIVE_PRESENTATION_POPUP || destination.NodeId != popup.Location().Path() {
			t.Fatalf("unresolved/misrouted popup %s: %+v", path, destination)
		}
		destination.NodeId = "changed-copy"
	}
	for _, path := range []string{"/lovelace/second", "/lovelace/1"} {
		destination, ok := index.Resolve(from, views[0].Location(), path)
		if !ok || destination.Presentation != wire.NativePresentation_NATIVE_PRESENTATION_PAGE || destination.NodeId != views[1].Location().Path() {
			t.Fatalf("unresolved page %s", path)
		}
	}
	for _, path := range []string{"#synthetic-room", "#missing", "https://example.invalid", "//example.invalid", "/lovelace/missing", "/lovelace/%30", "/lovelace/0?command=unlock"} {
		if destination, ok := index.Resolve(from, views[1].Location(), path); ok || destination != nil {
			t.Fatalf("unadmitted/cross-view fallback %s", path)
		}
	}
	if destination, ok := index.Resolve(from, Location{}, "/lovelace/second"); ok || destination != nil {
		t.Fatal("unknown source context accepted")
	}
	// Add a second popup with the same anchor to the same source view. The
	// destination must be ambiguous, even though both individual cards parse.
	duplicate := strings.Replace(nhf009RecipeSource, "    - type: custom:mushroom-template-card", "    - type: custom:bubble-card\n      card_type: pop-up\n      hash: '#synthetic-room'\n      name: Duplicate\n      cards: []\n    - type: custom:mushroom-template-card", 1)
	second := nhf009ImportRecipe(t, duplicate)
	secondDefinition, present := second.Definition()
	if !present {
		t.Fatal("missing duplicate definition")
	}
	if _, err := NewNavigationIndex([]Definition{secondDefinition}); err == nil {
		t.Fatal("duplicate popup anchor silently chose a destination")
	}
	if _, err := NewNavigationIndex([]Definition{definition, definition}); err == nil {
		t.Fatal("duplicate dashboard identity silently shadowed a source")
	}
}

func TestNHF009CorpusNavigationUsesAllThirteenConfiguredPopupBodies(t *testing.T) {
	result, err := ImportWithProfile("testdata/nhf009", nha059Source(t, "synthetic_tablet", "dashboard-tablet", "dashboard-tablet.inventory.yaml"), CompilationRecipes)
	if err != nil {
		t.Fatal(err)
	}
	definition, present := result.Definition()
	if !present {
		t.Fatal("missing tablet definition")
	}
	index, err := NewNavigationIndex([]Definition{definition})
	if err != nil {
		t.Fatal(err)
	}
	if len(index.popups) != 13 {
		t.Fatalf("popup bodies omitted: %d diagnostics=%+v", len(index.popups), result.Diagnostics())
	}
	view := definition.Views()[0]
	links := 0
	var check func([]Card)
	check = func(cards []Card) {
		for _, card := range cards {
			if card.Status() != StatusSupported {
				continue
			}
			for _, action := range card.Actions() {
				path, present := action.Navigation()
				if !present || !strings.HasPrefix(path, "#") {
					continue
				}
				links++
				if destination, ok := index.Resolve(definition.Source().ID(), view.Location(), path); !ok || destination.Presentation != wire.NativePresentation_NATIVE_PRESENTATION_POPUP {
					t.Fatalf("configured popup link lost: %s", path)
				}
			}
			check(card.Children())
		}
	}
	for _, section := range view.Sections() {
		if section.Status() == StatusSupported {
			check(section.Cards())
		}
	}
	if links < 13 {
		t.Fatalf("configured Bubble links omitted: %d", links)
	}
}
