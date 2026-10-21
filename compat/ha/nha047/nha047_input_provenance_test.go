package nha047_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/damione1/Homeward/compat/ha/nha047"
)

// These envelopes are explicit synthetic decoder/comparator fixtures. Their
// input hashes do not assert that HA or the native probe was executed.
func inputProvenanceCapture(t *testing.T, fixture nha047.Case, side, input string) []byte {
	t.Helper()
	version, source := nha047.GoVersion, "probe"
	if side == "ha" {
		version, source = nha047.HAVersion, "homeassistant.core"
	}
	demand, err := nha047.NewDemand(nha047.SwitchOn, "switch.nha047_value", 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	data, err := nha047.EncodeCapture(fixture, side, version, consumedSnapshot(t, input), syntheticCaptureSources(t, side, source), []nha047.Demand{demand}, true, true)
	if err != nil {
		t.Fatal(err)
	}
	object := presenceObject(t, data)
	object["input_sha256"], err = json.Marshal(nha047.Hash([]byte(input)).String())
	if err != nil {
		t.Fatal(err)
	}
	return presenceEncode(t, object)
}

func inputProvenanceBundle(t *testing.T, fixture nha047.Case, ha, goSide []byte) []byte {
	t.Helper()
	fixtureJSON, err := fixture.JSON()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(struct {
		Fixture json.RawMessage `json:"fixture"`
		HA      json.RawMessage `json:"ha"`
		Go      json.RawMessage `json:"go"`
	}{fixtureJSON, ha, goSide})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestNHA047InputDigestIsRequiredEvenWhenObservationsMatch(t *testing.T) {
	fixture := caseByID(t, "legacy-service-alias")
	ha := presenceObject(t, inputProvenanceCapture(t, fixture, "ha", fixture.YAML()))
	goSide := presenceObject(t, inputProvenanceCapture(t, fixture, "go", fixture.Candidate()))
	delete(ha, "input_sha256")
	delete(goSide, "input_sha256")
	for side, object := range map[string]map[string]json.RawMessage{"ha": ha, "go": goSide} {
		t.Run(side, func(t *testing.T) {
			if _, err := nha047.DecodeCapture(presenceEncode(t, object)); !errors.Is(err, nha047.ErrInvalidCapture) {
				t.Errorf("capture without loaded-input provenance was admitted: %v", err)
			}
		})
	}
	comparison, proof, err := nha047.CompareInput(inputProvenanceBundle(t, fixture, presenceEncode(t, ha), presenceEncode(t, goSide)))
	if !errors.Is(err, nha047.ErrInvalidCapture) || comparison.Equivalent || proof.Kind() != nha047.EvidenceInvalid {
		t.Errorf("matching observations hid absent loaded-input provenance: %+v %v", comparison, err)
	}
}

func TestNHA047InputDigestBindsOriginalAndCandidateRuntimeRoles(t *testing.T) {
	fixture := caseByID(t, "legacy-service-alias")
	if fixture.YAML() == fixture.Candidate() || !strings.Contains(fixture.YAML(), "service: switch.turn_on") || !strings.Contains(fixture.Candidate(), "action: switch.turn_on") {
		t.Fatal("precondition: the registered adaptation must have distinct explicit inputs")
	}
	for _, test := range []struct {
		name    string
		haInput string
		goInput string
		valid   bool
	}{
		{"explicit_original_to_candidate", fixture.YAML(), fixture.Candidate(), true},
		{"ha_loaded_candidate", fixture.Candidate(), fixture.Candidate(), false},
		{"go_loaded_original", fixture.YAML(), fixture.YAML(), false},
		{"roles_inverted", fixture.Candidate(), fixture.YAML(), false},
		{"unrelated_ha_input", "unrelated input", fixture.Candidate(), false},
		{"unrelated_go_input", fixture.YAML(), "unrelated input", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ha := inputProvenanceCapture(t, fixture, "ha", test.haInput)
			goSide := inputProvenanceCapture(t, fixture, "go", test.goInput)
			comparison, proof, err := nha047.CompareInput(inputProvenanceBundle(t, fixture, ha, goSide))
			if test.valid {
				if err != nil || !comparison.Equivalent || proof.Kind() != nha047.DifferentialEquivalent {
					t.Fatalf("explicit synthetic source pair must be admitted: %+v %v", comparison, err)
				}
			} else if !errors.Is(err, nha047.ErrInvalidCapture) || comparison.Equivalent || proof.Kind() != nha047.EvidenceInvalid {
				t.Errorf("input hash was borrowed from the wrong source role: %+v %v", comparison, err)
			}
		})
	}
}

func TestNHA047InputDigestRejectsAbsentMalformedAndZeroScalars(t *testing.T) {
	fixture := caseByID(t, "legacy-service-alias")
	data := inputProvenanceCapture(t, fixture, "ha", fixture.YAML())
	for _, test := range []struct {
		name  string
		value json.RawMessage
	}{
		{"absent", nil},
		{"null", json.RawMessage("null")},
		{"empty", json.RawMessage(`""`)},
		{"invalid", json.RawMessage(`"not-a-digest"`)},
		{"zero", json.RawMessage(`"` + strings.Repeat("0", 64) + `"`)},
		{"wrong_type", json.RawMessage("true")},
	} {
		t.Run(test.name, func(t *testing.T) {
			object := presenceObject(t, data)
			if test.value == nil {
				delete(object, "input_sha256")
			} else {
				object["input_sha256"] = test.value
			}
			if _, err := nha047.DecodeCapture(presenceEncode(t, object)); !errors.Is(err, nha047.ErrInvalidCapture) {
				t.Errorf("invalid explicit input digest was admitted: %v", err)
			}
		})
	}
}

func TestNHA047CaptureEncoderNamesTheExpectedInputForEachRuntime(t *testing.T) {
	fixture := caseByID(t, "legacy-service-alias")
	demand, err := nha047.NewDemand(nha047.SwitchOn, "switch.nha047_value", 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{"ha", "go"} {
		t.Run(side, func(t *testing.T) {
			input, version, source := fixture.YAML(), nha047.HAVersion, "homeassistant.core"
			if side == "go" {
				input, version, source = fixture.Candidate(), nha047.GoVersion, "probe"
			}
			data, err := nha047.EncodeCapture(fixture, side, version, consumedSnapshot(t, input), syntheticCaptureSources(t, side, source), []nha047.Demand{demand}, true, true)
			if err != nil {
				t.Fatal(err)
			}
			object := presenceObject(t, data)
			var digest string
			if err := json.Unmarshal(object["input_sha256"], &digest); err != nil || digest != nha047.Hash([]byte(input)).String() {
				t.Errorf("encoder lost its expected %s input role: hash=%q err=%v", side, digest, err)
			}
		})
	}
}
