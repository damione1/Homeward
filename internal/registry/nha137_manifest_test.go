package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/damione1/Homeward/scope"
)

func TestNHA137ManifestActiveScopeAndExactLineage(t *testing.T) {
	if _, err := LoadManifest(scope.Revision{}); err == nil {
		t.Fatal("zero scope admitted")
	}
	baseline, err := LoadManifest(scope.BaselineV1())
	if err != nil {
		t.Fatal(err)
	}
	ecobee, err := LoadManifest(scope.EcobeeMVPV1())
	if err != nil {
		t.Fatal(err)
	}
	home, _ := ParseHAAlias("climate.home")
	if baseline.permits(home) || !baseline.recognizes(home) || !ecobee.permits(home) || !ecobee.recognizes(home) {
		t.Fatal("active admission and lineage conflated")
	}
	if _, ok := baseline.SourceFor(home); ok {
		t.Fatal("inactive source visible")
	}
	source, ok := ecobee.SourceFor(home)
	text, valid := source.Value()
	if !ok || !valid || text != "homekit_controller" {
		t.Fatal("documentary source missing")
	}
	for _, text := range []string{
		"binary_sensor.bedroom_motion", "binary_sensor.bedroom_occupancy", "binary_sensor.home_motion", "binary_sensor.home_occupancy",
		"button.bedroom_identify", "button.home_clear_hold", "button.home_identify", "select.home_current_mode", "select.home_temperature_display_units",
		"sensor.bedroom_battery", "sensor.bedroom_temperature", "sensor.home_current_humidity", "sensor.home_current_temperature", "switch.home_airplay_enable", "switch.home_mute", "climate.foreign", "light.homekit_bridge",
	} {
		alias, _ := ParseHAAlias(text)
		if baseline.permits(alias) || ecobee.permits(alias) || baseline.recognizes(alias) || ecobee.recognizes(alias) {
			t.Fatalf("excluded %s admitted", text)
		}
	}
	for _, fixture := range []struct {
		manifest          Manifest
		entities, sources int
		revision          scope.Revision
	}{{baseline, 850, 38, scope.BaselineV1()}, {ecobee, 851, 39, scope.EcobeeMVPV1()}} {
		counts, ok := fixture.manifest.Counts()
		if !ok || counts.Entities != fixture.entities || counts.Sources != fixture.sources || counts.Diagnostics != 19 {
			t.Fatalf("counts %+v", counts)
		}
		revision, ok := fixture.manifest.Revision()
		if !ok || revision != fixture.revision {
			t.Fatal("loaded revision missing")
		}
		if len(fixture.manifest.lineage) != 851 {
			t.Fatal("lineage not exact union")
		}
	}
	if baseline.permits(home) {
		t.Fatal("constructing amendment mutated prior manifest")
	}
	var raw struct {
		Entities []struct {
			ID string `json:"entity_id"`
		}
	}
	if err := json.Unmarshal(scope.ManifestV1(), &raw); err != nil {
		t.Fatal(err)
	}
	ids := []string{"climate.home"}
	for _, row := range raw.Entities {
		ids = append(ids, row.ID)
		alias, _ := ParseHAAlias(row.ID)
		if !baseline.permits(alias) || !ecobee.permits(alias) {
			t.Fatal("v1 identity lost", row.ID)
		}
	}
	sort.Strings(ids)
	want := sha256.Sum256([]byte(strings.Join(ids, "\n") + "\n"))
	got, ok := ecobee.Digest()
	if !ok || got != want {
		t.Fatal("digest not calculated from union")
	}
	original, _ := baseline.Digest()
	if hex.EncodeToString(original[:]) != scope.BaselineIDsSHA256 {
		t.Fatal("baseline digest changed")
	}
}

func TestNHA137DoesNotRelaxLegacyV1Parser(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal(scope.ManifestV1(), &raw); err != nil {
		t.Fatal(err)
	}
	raw["entities"] = append(raw["entities"].([]any), map[string]any{"entity_id": "climate.home", "source": "homekit_controller"})
	mutant, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseManifest(mutant); err == nil {
		t.Fatal("legacy parser admitted new allowlist without revision boundary")
	}
	if _, ok := (Manifest{}).Revision(); ok {
		t.Fatal("zero manifest has a scope revision")
	}
	if _, ok := (Manifest{}).Counts(); ok {
		t.Fatal("zero manifest has counters")
	}
	if _, ok := (Manifest{}).ScopeAmendment(); ok {
		t.Fatal("zero manifest has amendment provenance")
	}
}
