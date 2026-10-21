package report_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/nha047"
	"github.com/damione1/Homeward/compat/ha/report"
	"github.com/damione1/Homeward/contracts"
	registryv1 "github.com/damione1/Homeward/gen/home/registry/v1"
)

// These are adversarial public-API fixtures, not executed HA observations.
func reviewProof(t *testing.T, id string, demands []nha047.Demand) nha047.Proof {
	t.Helper()
	corpus, err := nha047.Corpus()
	if err != nil {
		t.Fatal(err)
	}
	var fixture nha047.Case
	for _, item := range corpus {
		if item.ID() == id {
			fixture = item
		}
	}
	if !fixture.Valid() {
		t.Fatal("missing review fixture", id)
	}
	capture := func(side, version, path string) nha047.Capture {
		data, err := nha047.EncodeCapture(fixture, side, version, reportFixtureSnapshot(t, fixture, side), reportCaptureSources(t, side, path), demands, true, true)
		if err != nil {
			t.Fatal(err)
		}
		value, err := nha047.DecodeCapture(data)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	comparison, proof, err := nha047.Compare(fixture, capture("ha", nha047.HAVersion, "homeassistant.core"), capture("go", nha047.GoVersion, "probe"))
	if err != nil || !comparison.Equivalent {
		t.Fatal("review comparator precondition", comparison, err)
	}
	return proof
}

func reviewClimateInput(t *testing.T, evidence nha047.Evidence, step float64) report.Input {
	t.Helper()
	input := fanInput(t, evidence)
	entities := input.Catalogue.Entities()
	for _, id := range []string{"input_boolean.heating_season", "climate.neviweb130_climate_sous_sol", "climate.neviweb130_climate_chambre_sous_sol"} {
		support := ir.SupportObserveState
		if strings.HasPrefix(id, "climate.") {
			support = ir.SupportClimateSetpoint
		}
		entity, err := ir.NewEntity(id, ir.ReferencePresent, support)
		if err != nil {
			t.Fatal(err)
		}
		entities = append(entities, entity)
		if support == ir.SupportClimateSetpoint {
			description := &registryv1.CapabilityDescription{SchemaId: contracts.ClimateSchema, Major: 1, Endpoint: "setpoint", DisplayName: "Synthetic review climate", Constraints: &registryv1.CapabilityDescription_Climate{Climate: &registryv1.ClimateConstraints{SetpointCelsius: &registryv1.NumberBounds{Minimum: 10, Maximum: 30, Step: &step}}}}
			declaration, err := report.NewDeclaration(entity.Alias(), description, report.NativeOnly)
			if err != nil {
				t.Fatal(err)
			}
			input.Declarations = append(input.Declarations, declaration)
		}
	}
	var err error
	input.Catalogue, err = ir.NewCatalogue(entities, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func TestNHA047FalseConditionProofCannotCoverUnobservedClimateAction(t *testing.T) {
	negative := reviewProof(t, "time-18-false", nil)
	for _, positive := range []bool{false, true} {
		proofs := []nha047.Proof{negative, pairControl045(t)}
		if positive {
			demand, err := nha047.NewDemand(nha047.ClimateTemperature, "climate.nha047_value", 18, 0, "")
			if err != nil {
				t.Fatal(err)
			}
			proofs = append(proofs, reviewProof(t, "time-18-true", []nha047.Demand{demand}))
		}
		evidence, err := nha047.NewEvidence(proofs)
		if err != nil {
			t.Fatal(err)
		}
		value, err := report.Build(reviewClimateInput(t, evidence, 1))
		if err != nil {
			t.Fatal(err)
		}
		object := findObject(t, value, "automation.turn_off_basement_climate_the_evening")
		if positive {
			if object.Verdict() != report.AcceptedUnchanged {
				t.Fatal("observed positive action lost coverage", object.Refusal())
			}
		} else {
			if object.Verdict() != report.Refused || object.Refusal() != report.CoverageMissing {
				t.Fatal("false condition qualified an unobserved action")
			}
			for _, node := range object.Constructions() {
				if node.Feature().Construction() == nha047.ClimateSetpoint && node.Coverage() == report.Covered {
					t.Fatal("unobserved setpoint retained covered state")
				}
			}
		}
	}
}

func TestNHA047EveryProofProducingPublicRouteRequiresPinnedHASources(t *testing.T) {
	corpus, err := nha047.Corpus()
	if err != nil {
		t.Fatal(err)
	}
	var fixture nha047.Case
	for _, item := range corpus {
		if item.ID() == "scene-fan-on" {
			fixture = item
		}
	}
	demand, err := nha047.NewDemand(nha047.FanOn, "fan.nha047_value", 75, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	haBytes, err := nha047.EncodeCapture(fixture, "ha", nha047.HAVersion, reportFixtureSnapshot(t, fixture, "ha"), []nha047.SourceHash{{Path: "homeassistant.core", SHA256: strings.Repeat("a", 64)}}, []nha047.Demand{demand}, true, true)
	if err != nil {
		t.Fatal(err)
	}
	goBytes, err := nha047.EncodeCapture(fixture, "go", nha047.GoVersion, reportFixtureSnapshot(t, fixture, "go"), reportCaptureSources(t, "go", "probe"), []nha047.Demand{demand}, true, true)
	if err != nil {
		t.Fatal(err)
	}
	ha, err := nha047.DecodeCapture(haBytes)
	if err != nil {
		t.Fatal(err)
	}
	goSide, err := nha047.DecodeCapture(goBytes)
	if err != nil {
		t.Fatal(err)
	}
	_, proof, err := nha047.Compare(fixture, ha, goSide)
	if !errors.Is(err, nha047.ErrInvalidCapture) || proof.Kind() != nha047.EvidenceInvalid {
		t.Fatal("direct Compare minted proof with unpinned HA modules", err)
	}
	if _, err := nha047.NewEvidence([]nha047.Proof{proof}); err == nil {
		t.Fatal("invalid proof admitted as evidence")
	}
	fixtureBytes, err := fixture.JSON()
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := json.Marshal(struct {
		Fixture json.RawMessage `json:"fixture"`
		HA      json.RawMessage `json:"ha"`
		Go      json.RawMessage `json:"go"`
	}{fixtureBytes, haBytes, goBytes})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := nha047.CompareInput(bundle); !errors.Is(err, nha047.ErrInvalidCapture) {
		t.Fatal("CompareInput bypassed HA source admission", err)
	}
	encoded, err := json.Marshal(struct {
		Schema      string            `json:"schema"`
		Comparisons []json.RawMessage `json:"comparisons"`
	}{nha047.Schema, []json.RawMessage{bundle}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nha047.DecodeEvidence(encoded); !errors.Is(err, nha047.ErrInvalidCapture) {
		t.Fatal("DecodeEvidence bypassed HA source admission", err)
	}
	// A complete list with one wrong digest must fail as well as a missing list.
	pinned := reportCaptureSources(t, "ha", "homeassistant.core")
	pinned[0].SHA256 = strings.Repeat("b", 64)
	changedBytes, err := nha047.EncodeCapture(fixture, "ha", nha047.HAVersion, reportFixtureSnapshot(t, fixture, "ha"), pinned, []nha047.Demand{demand}, true, true)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := nha047.DecodeCapture(changedBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, proof, err := nha047.Compare(fixture, changed, goSide); !errors.Is(err, nha047.ErrInvalidCapture) || proof.Kind() != nha047.EvidenceInvalid {
		t.Fatal("full source list hid a wrong module digest", err)
	}
}

func TestNHA047ReportUsesCanonicalClimateAndLightStepAdmission(t *testing.T) {
	proofs := []nha047.Proof{pairControl045(t)}
	for _, value := range []float64{15, 18} {
		demand, err := nha047.NewDemand(nha047.ClimateTemperature, "climate.nha047_value", value, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		id := "time-15-true"
		if value == 18 {
			id = "time-18-true"
		}
		proofs = append(proofs, reviewProof(t, id, []nha047.Demand{demand}))
	}
	evidence, err := nha047.NewEvidence(proofs)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []float64{1, 2} {
		result, err := report.Build(reviewClimateInput(t, evidence, step))
		if err != nil {
			t.Fatal(err)
		}
		onGrid := findObject(t, result, "automation.turn_off_basement_climate_the_evening")
		if onGrid.Verdict() != report.AcceptedUnchanged {
			t.Fatal("on-grid18 rejected", step, onGrid.Refusal())
		}
		fifteen := findObject(t, result, "automation.turn_off_heating_in_guest_bedroom_the_day")
		if step == 1 && fifteen.Verdict() != report.AcceptedUnchanged {
			t.Fatal("on-grid15 rejected", fifteen.Refusal())
		}
		if step == 2 && (fifteen.Verdict() != report.Refused || fifteen.Refusal() != report.MissingDeclaredCapability) {
			t.Fatal("off-grid15 accepted by report")
		}
	}
	lightEvidence := lightProjectionBoundaryEvidence(t)
	for _, step := range []float64{100.0 / 255, 1} {
		input := fanInput(t, lightEvidence)
		entities := input.Catalogue.Entities()
		for _, id := range []string{"light.kitchen_lights", "light.kitchen_main_lights", "light.kitchen_sink_light"} {
			entity, err := ir.NewEntity(id, ir.ReferencePresent, ir.SupportObserveState)
			if err != nil {
				t.Fatal(err)
			}
			entities = append(entities, entity)
			description := &registryv1.CapabilityDescription{SchemaId: contracts.LightSchema, Major: 1, Endpoint: "level", DisplayName: "Synthetic stepped light", Constraints: &registryv1.CapabilityDescription_Light{Light: &registryv1.LightConstraints{SetOn: true, BrightnessPercent: &registryv1.NumberBounds{Minimum: 0, Maximum: 100, Step: &step}}}}
			declaration, err := report.NewDeclaration(entity.Alias(), description, report.MQTTHA255)
			if err != nil {
				t.Fatal(err)
			}
			input.Declarations = append(input.Declarations, declaration)
		}
		input.Catalogue, err = ir.NewCatalogue(entities, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		result, err := report.Build(input)
		if err != nil {
			t.Fatal(err)
		}
		object := findObject(t, result, "scene.kitchen_cosy_mode")
		if step != 1 && object.Verdict() != report.AcceptedUnchanged {
			t.Fatal("on-grid brightness rejected", object.Refusal())
		}
		if step == 1 && (object.Verdict() != report.Refused || object.Refusal() != report.MissingDeclaredCapability) {
			t.Fatal("off-grid brightness accepted by report")
		}
	}
}
