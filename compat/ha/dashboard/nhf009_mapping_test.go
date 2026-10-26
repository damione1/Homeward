package dashboard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/damione1/Homeward/compat/ha/parser"
)

func TestNHF009FullCorpusMappingHasTwentyViewsAndDoesNotInferInstalledCards(t *testing.T) {
	const root = "testdata/nhf009"
	var provenance struct {
		Fixtures []struct {
			File   string `json:"file"`
			Bytes  int    `json:"bytes"`
			SHA256 string `json:"sha256"`
		} `json:"fixtures"`
	}
	data, err := os.ReadFile(filepath.Join(root, "provenance.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &provenance); err != nil {
		t.Fatal(err)
	}
	if len(provenance.Fixtures) != 5 {
		t.Fatal("expected the five stored dashboards, exporting default/lovelace once")
	}
	views, strategies := 0, 0
	families := map[Family]int{}
	variants := map[string]int{}
	for _, fixture := range provenance.Fixtures {
		data, err := os.ReadFile(filepath.Join(root, fixture.File))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		if len(data) != fixture.Bytes || hex.EncodeToString(digest[:]) != fixture.SHA256 {
			t.Fatalf("source drift: %s", fixture.File)
		}
		doc, err := parser.LoadProfile(root, fixture.File, parser.SyntaxProfileLovelace)
		if err != nil {
			t.Fatal(err)
		}
		occurrences, err := MapOccurrences(doc)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, occurrence := range occurrences {
			location := occurrence.Location
			if seen[location.Path()] {
				t.Fatalf("duplicate occurrence %s/%s", fixture.File, location.Path())
			}
			seen[location.Path()] = true
			if location.Position().File != fixture.File || location.Position().Line < 1 || location.Position().Column < 1 {
				t.Fatalf("unsourced occurrence: %+v", occurrence)
			}
			if occurrence.Decision.Ticket.String() == "" || occurrence.Decision.Reason == "" || occurrence.Decision.Treatment == TreatmentInvalid {
				t.Fatalf("unmapped occurrence: %+v", occurrence)
			}
			if occurrence.Kind == OccurrenceView {
				views++
			}
			if occurrence.Kind == OccurrenceStrategy {
				strategies++
				if occurrence.Decision.Treatment != TreatmentRefused {
					t.Fatal("runtime strategy was admitted without captured views")
				}
			}
			families[occurrence.Decision.Family]++
			variants[occurrence.Variant]++
		}
	}
	if views != 20 || strategies != 2 {
		t.Fatalf("views=%d strategies=%d", views, strategies)
	}
	// Independent inventory counts, not counts inferred from HACS/resources.
	for variant, count := range map[string]int{"tile": 238, "heading": 59, "custom:mushroom-template-card": 32,
		"custom:bubble-card": 26, "custom:mushroom-template-badge": 12, "custom:mushroom-chips-card": 1,
		"template": 5, "custom:mushroom-select-card": 1, "custom:mushroom-alarm-control-panel-card": 1,
		"custom:calendar-card-pro": 3, "custom:frigate-card": 9, "custom:mini-media-player": 3} {
		if variants[variant] != count {
			t.Fatalf("%s: occurrences=%d expected=%d", variant, variants[variant], count)
		}
	}
	if families[FamilyUnknown] != 0 {
		t.Fatalf("unexpected unclassified corpus families: %+v", variants)
	}
	if families[FamilyStreamline] != 0 || families[FamilyScheduler] != 0 {
		t.Fatal("installed resource inferred as a used card")
	}
	t.Logf("views=%d strategies=%d variants=%v", views, strategies, variants)
}

func TestNHF009MappingRetainsUnknownSourceAndIgnoresServiceDataAndResources(t *testing.T) {
	data := []byte(`resources:
- type: module
  url: /installed-but-unused.js
views:
- sections:
  - type: grid
    cards:
    - type: custom:future-card
      styles: 'javascript:never-run'
      tap_action:
        action: perform-action
        data:
          cards:
          - type: tile
    - type: custom:streamline-card
    - type: custom:scheduler-card
`)
	file, err := parser.NewSourceFile("source.yaml", data)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parser.LoadSources("source.yaml", []parser.SourceFile{file}, parser.SyntaxProfileLovelace)
	if err != nil {
		t.Fatal(err)
	}
	occurrences, err := MapOccurrences(doc)
	if err != nil {
		t.Fatal(err)
	}
	var variants []string
	for _, occurrence := range occurrences {
		variants = append(variants, occurrence.Variant)
	}
	if !reflect.DeepEqual(variants, []string{"sections", "grid", "custom:future-card", "custom:streamline-card", "custom:scheduler-card"}) {
		t.Fatalf("invented/omitted occurrences: %v", variants)
	}
	unknown := occurrences[2]
	if unknown.Decision.Family != FamilyUnknown || unknown.Decision.Treatment != TreatmentRefused {
		t.Fatal("unknown card not refused")
	}
	styles, present := unknown.Source.Lookup("styles")
	text, valid := scalarText(styles)
	if !present || !valid || text != "javascript:never-run" {
		t.Fatal("unsupported source discarded")
	}
	if occurrences[3].Decision.Ticket != MappingRecipes || occurrences[4].Decision.Ticket != MappingLogic {
		t.Fatal("wrong deferred mapping")
	}
}
