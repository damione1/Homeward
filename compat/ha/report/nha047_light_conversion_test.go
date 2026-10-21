package report_test

import (
	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/nha047"
	"github.com/damione1/Homeward/compat/ha/report"
	"github.com/damione1/Homeward/contracts"
	registryv1 "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"testing"
)

// These synthetic tuples test applicability at the public report boundary.
// They are not published as executed HA/native qualification evidence.
func lightProjectionBoundaryEvidence(t *testing.T) nha047.Evidence {
	t.Helper()
	all, e := nha047.Corpus()
	if e != nil {
		t.Fatal(e)
	}
	var proofs []nha047.Proof
	for _, sample := range []struct {
		id         string
		brightness float64
	}{{"scene-light-102-0", 102}, {"scene-light-51-0", 51}, {"scene-light-76-0", 76}} {
		var fixture nha047.Case
		for _, candidate := range all {
			if candidate.ID() == sample.id {
				fixture = candidate
			}
		}
		if !fixture.Valid() {
			t.Fatal("missing registered projection case")
		}
		demand, e := nha047.NewDemand(nha047.LightOn, "light.nha047_value", sample.brightness, 0, "")
		if e != nil {
			t.Fatal(e)
		}
		capture := func(side, version, path string) nha047.Capture {
			data, e := nha047.EncodeCapture(fixture, side, version, reportFixtureSnapshot(t, fixture, side), reportCaptureSources(t, side, path), []nha047.Demand{demand}, true, true)
			if e != nil {
				t.Fatal(e)
			}
			value, e := nha047.DecodeCapture(data)
			if e != nil {
				t.Fatal(e)
			}
			return value
		}
		_, proof, e := nha047.Compare(fixture, capture("ha", nha047.HAVersion, "homeassistant.core"), capture("go", nha047.GoVersion, "probe"))
		if e != nil {
			t.Fatal(e)
		}
		proofs = append(proofs, proof)
	}
	evidence, e := nha047.NewEvidence(proofs)
	if e != nil {
		t.Fatal(e)
	}
	return evidence
}

func TestNHA047SceneProjectionCannotQualifyAnUnobservedLightConversion(t *testing.T) {
	evidence := lightProjectionBoundaryEvidence(t)
	for _, conversion := range []report.Conversion{report.MQTTHA255, report.MQTTHA254, report.CasetaHALight100} {
		t.Run(string(rune('0'+conversion)), func(t *testing.T) {
			input := fanInput(t, evidence)
			entities := input.Catalogue.Entities()
			step := 1.0
			for _, id := range []string{"light.kitchen_lights", "light.kitchen_main_lights", "light.kitchen_sink_light"} {
				alias, e := domain.ParseHAAlias(id)
				if e != nil {
					t.Fatal(e)
				}
				entity, e := ir.NewEntity(id, ir.ReferencePresent, ir.SupportObserveState)
				if e != nil {
					t.Fatal(e)
				}
				entities = append(entities, entity)
				description := &registryv1.CapabilityDescription{SchemaId: contracts.LightSchema, Major: 1, Endpoint: "level", DisplayName: "Synthetic declared light", Constraints: &registryv1.CapabilityDescription_Light{Light: &registryv1.LightConstraints{SetOn: true, BrightnessPercent: &registryv1.NumberBounds{Minimum: 0, Maximum: 100}, TransitionSeconds: &registryv1.NumberBounds{Minimum: 0, Maximum: 6553, Step: &step}}}}
				declaration, e := report.NewDeclaration(alias, description, conversion)
				if e != nil {
					t.Fatal(e)
				}
				input.Declarations = append(input.Declarations, declaration)
			}
			var e error
			input.Catalogue, e = ir.NewCatalogue(entities, nil, nil)
			if e != nil {
				t.Fatal(e)
			}
			value, e := report.Build(input)
			if e != nil {
				t.Fatal(e)
			}
			object := findObject(t, value, "scene.kitchen_cosy_mode")
			if conversion == report.MQTTHA255 {
				if object.Verdict() != report.AcceptedUnchanged {
					t.Fatalf("observed conversion refused: %s", object.Refusal())
				}
			} else {
				if object.Verdict() != report.Refused || object.Refusal() != report.CoverageMissing {
					t.Fatal("unobserved conversion acquired a verdict from MQTT255 projection")
				}
				found := 0
				for _, node := range object.Constructions() {
					if node.Feature().Construction() == nha047.SceneLightBrightness {
						found++
						if node.Coverage() != report.Partial || node.Refusal() != report.CoverageMissing {
							t.Fatal("other-conversion proof presented as covering")
						}
					}
				}
				if found != 3 {
					t.Fatal("three imported light members lost provenance")
				}
			}
		})
	}
}
