package logicselection

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/scope"
)

func TestNHA158ExactPartitionAndZeroValue(t *testing.T) {
	for _, frozen := range []struct {
		name string
		data []byte
		want string
	}{
		{"logic v1", scope.LogicV1(), "97d7b24d42330273aa1b60d3f0c8c4864f07f7af9d47ff688eaa1ab7ac73aa33"},
		{"manifest v1", scope.ManifestV1(), "d2f15358239b29797906a31f9318d6ad7a8b81be76f9d39421b0aa6d22f52cfa"},
	} {
		sum := sha256.Sum256(frozen.data)
		if hex.EncodeToString(sum[:]) != frozen.want {
			t.Fatalf("%s bytes changed", frozen.name)
		}
	}
	selection, err := LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Retained()) != 17 || len(selection.byID) != 89 {
		t.Fatalf("wrong partition: %d retained / %d total", len(selection.Retained()), len(selection.byID))
	}
	retained := map[string]DecisionRef{
		"automation.heat_the_office_on_work_day":               DecisionQ33,
		"automation.ikea_kitchen_lights_remote":                DecisionQ32,
		"automation.turn_off_basement_climate_the_evening":     DecisionQ32,
		"automation.turn_off_heating_in_guest_bedroom_the_day": DecisionQ32,
		"automation.turn_on_lights_at_sunset":                  DecisionQ34,
		"scene.ceilling_fan_day":                               DecisionQ32,
		"scene.ceilling_fan_night":                             DecisionQ32,
		"scene.climate_day_temperature":                        DecisionQ32,
		"scene.climate_night_temperature":                      DecisionQ32,
		"scene.cosy":                                           DecisionQ32,
		"scene.kitchen_cooking_mode":                           DecisionQ32,
		"scene.kitchen_cosy_mode":                              DecisionQ32,
		"scene.living_room_daylight":                           DecisionQ32,
		"scene.living_room_in_between":                         DecisionQ32,
		"scene.night_mode":                                     DecisionQ32,
		"scene.turn_off_living_room":                           DecisionQ32,
		"scene.turn_off_main_floor":                            DecisionQ32,
	}
	for _, definition := range selection.Retained() {
		alias, err := definition.ID().Value()
		if err != nil || retained[alias] != definition.Decision() {
			t.Fatalf("unexpected retained definition %q", alias)
		}
		if definition.Path() != "scope/logic_mvp_sources/"+alias+".yaml" {
			t.Fatalf("retained definition %q lost its admitted source path", alias)
		}
		delete(retained, alias)
	}
	if len(retained) != 0 {
		t.Fatalf("missing retained definitions: %v", retained)
	}
	for alias, disposition := range selection.byID {
		if _, err := alias.Value(); err != nil || disposition == DispositionUnspecified {
			t.Fatal("invalid ID or disposition admitted")
		}
	}
	alias, err := domain.ParseHAAlias("scene.kitchen_cooking_mode")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := selection.DispositionFor(alias); !ok || got != DispositionRetained {
		t.Fatal("retained scene missing")
	}
	copyBytes := scope.LogicMVPV1()
	copyBytes[0] = 'x'
	if scope.LogicMVPV1()[0] == 'x' {
		t.Fatal("embedded amendment bytes are mutable")
	}
	copyDefinitions := selection.Retained()
	copyDefinitions[0] = Definition{}
	if selection.Retained()[0].ID() == (domain.HAAlias{}) {
		t.Fatal("retained definitions are mutable")
	}
	var zero Selection
	if zero.Retained() != nil {
		t.Fatal("zero selection exposes definitions")
	}
	if got, ok := zero.DispositionFor(alias); ok || got != DispositionUnspecified {
		t.Fatal("zero selection admits an ID")
	}
}

func TestNHA158InvalidAmendments(t *testing.T) {
	baseline := scope.LogicV1()
	manifest, err := registry.ParseManifest(scope.ManifestV1())
	if err != nil {
		t.Fatal(err)
	}
	load := func() rawAmendment {
		t.Helper()
		var raw rawAmendment
		if err := json.Unmarshal(scope.LogicMVPV1(), &raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	parse := func(raw rawAmendment, source fs.FS) error {
		t.Helper()
		data, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		_, err = Parse(data, baseline, manifest, source)
		return err
	}
	for _, tc := range []struct {
		name string
		edit func(*rawAmendment)
		want error
	}{
		{"missing", func(raw *rawAmendment) { raw.Deferred = raw.Deferred[1:] }, ErrMissingLogicID},
		{"duplicate", func(raw *rawAmendment) { raw.Deferred = append(raw.Deferred, raw.Deferred[0]) }, ErrDuplicateLogicID},
		{"overlap", func(raw *rawAmendment) {
			raw.Deferred = append(raw.Deferred, rawDeferred{EntityID: raw.Retained[0].EntityID, Active: raw.Retained[0].Active})
		}, ErrOverlappingLogicID},
		{"unknown", func(raw *rawAmendment) { raw.Deferred[0].EntityID = "scene.foreign" }, ErrUnknownLogicID},
		{"changed selection", func(raw *rawAmendment) {
			raw.Retained[0].EntityID = raw.Deferred[0].EntityID
			raw.Deferred[0].EntityID = "automation.heat_the_office_on_work_day"
		}, ErrInvalidAmendment},
		{"active", func(raw *rawAmendment) { on := true; raw.Retained[0].Active = &on }, ErrLogicActivation},
		{"absent active", func(raw *rawAmendment) { raw.Retained[0].Active = nil }, ErrInvalidAmendment},
		{"wrong source hash", func(raw *rawAmendment) {
			raw.Retained[0].Definition.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
		}, ErrDefinitionProvenance},
		{"uppercase source hash", func(raw *rawAmendment) {
			raw.Retained[0].Definition.SHA256 = strings.ToUpper(raw.Retained[0].Definition.SHA256)
		}, ErrDefinitionProvenance},
		{"wrong source path", func(raw *rawAmendment) { raw.Retained[0].Definition.Path = "../elsewhere.yaml" }, ErrDefinitionProvenance},
		{"wrong source section", func(raw *rawAmendment) { raw.Retained[0].Definition.SourceSection = "scene.other" }, ErrDefinitionProvenance},
		{"wrong decision", func(raw *rawAmendment) { raw.Retained[0].DecisionRef = "Q37" }, ErrInvalidAmendment},
		{"wrong baseline digest", func(raw *rawAmendment) {
			raw.BaselineLogicSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
		}, ErrBaselineMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := load()
			tc.edit(&raw)
			if err := parse(raw, scope.LogicMVPSourcesV1()); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	raw := load()
	if err := parse(raw, changedSourceFS{scope.LogicMVPSourcesV1()}); !errors.Is(err, ErrDefinitionProvenance) {
		t.Fatalf("tampered source got %v", err)
	}
	if _, err := Parse(scope.LogicMVPV1(), baseline, registry.Manifest{}, scope.LogicMVPSourcesV1()); !errors.Is(err, ErrBaselineMismatch) {
		t.Fatalf("zero manifest got %v", err)
	}
	var nilSource *changedSourceFS
	if _, err := Parse(scope.LogicMVPV1(), baseline, manifest, nilSource); !errors.Is(err, ErrInvalidAmendment) {
		t.Fatalf("typed nil source got %v", err)
	}
	var nilMap nilMapFS
	if _, err := Parse(scope.LogicMVPV1(), baseline, manifest, nilMap); !errors.Is(err, ErrInvalidAmendment) {
		t.Fatalf("typed nil map source got %v", err)
	}
}

type changedSourceFS struct{ fs.FS }

type nilMapFS map[string][]byte

func (nilMapFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

func (f changedSourceFS) ReadFile(name string) ([]byte, error) {
	data, err := fs.ReadFile(f.FS, name)
	if err == nil && name == "logic_mvp_sources/automation.heat_the_office_on_work_day.yaml" {
		data[0] = 'x'
	}
	return data, err
}
