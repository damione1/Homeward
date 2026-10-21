package nha047_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/damione1/Homeward/compat/ha/nha047"
)

func presenceObject(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	return object
}

func presenceEncode(t *testing.T, object map[string]json.RawMessage) []byte {
	t.Helper()
	data, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func omitPresenceField(t *testing.T, data []byte, field string) []byte {
	t.Helper()
	object := presenceObject(t, data)
	if _, exists := object[field]; !exists {
		t.Fatalf("precondition: field %q is absent before mutation", field)
	}
	delete(object, field)
	return presenceEncode(t, object)
}

func omitPresenceElementField(t *testing.T, data []byte, collection string, index int, field string) []byte {
	t.Helper()
	object := presenceObject(t, data)
	var elements []json.RawMessage
	if err := json.Unmarshal(object[collection], &elements); err != nil {
		t.Fatal(err)
	}
	if index < 0 || index >= len(elements) {
		t.Fatalf("precondition: collection %s has no element %d", collection, index)
	}
	elements[index] = omitPresenceField(t, elements[index], field)
	encoded, err := json.Marshal(elements)
	if err != nil {
		t.Fatal(err)
	}
	object[collection] = encoded
	return presenceEncode(t, object)
}

func TestNHA047RegisteredCaseRequiresExplicitEmptyAndFalseScalars(t *testing.T) {
	fixture := caseByID(t, "scene-light-off")
	data, err := fixture.JSON()
	if err != nil {
		t.Fatal(err)
	}
	fields := presenceObject(t, data)
	for field, expected := range map[string]string{
		"candidate": `""`,
		"at":        `""`,
		"season":    "false",
		"workday":   "false",
	} {
		if string(fields[field]) != expected {
			t.Fatalf("precondition: %s should explicitly contain %s", field, expected)
		}
		t.Run("absent_"+field, func(t *testing.T) {
			if _, err := nha047.DecodeCase(omitPresenceField(t, data, field)); !errors.Is(err, nha047.ErrInvalidInput) {
				t.Errorf("omitted required %s was reconstructed from its Go zero: %v", field, err)
			}
		})
	}
	if admitted, err := nha047.DecodeCase(data); err != nil || admitted.Digest() != fixture.Digest() {
		t.Fatalf("explicit zero/false/empty values must remain admitted: %v", err)
	}
}

func TestNHA047RegisteredCaseRequiresNestedDemandAndFeatureScalars(t *testing.T) {
	fixture := caseByID(t, "scene-light-off")
	data, err := fixture.JSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, collection := range []string{"ha_expected", "go_expected"} {
		for _, field := range []string{"value", "secondary", "code"} {
			t.Run(collection+"_absent_"+field, func(t *testing.T) {
				mutated := omitPresenceElementField(t, data, collection, 0, field)
				if _, err := nha047.DecodeCase(mutated); !errors.Is(err, nha047.ErrInvalidInput) {
					t.Errorf("omitted required DemandWire.%s was reconstructed from its zero: %v", field, err)
				}
			})
		}
	}
	for index, name := range []string{"light_off", "metadata"} {
		for _, field := range []string{"value", "secondary"} {
			t.Run("feature_"+name+"_absent_"+field, func(t *testing.T) {
				mutated := omitPresenceElementField(t, data, "features", index, field)
				if _, err := nha047.DecodeCase(mutated); !errors.Is(err, nha047.ErrInvalidInput) {
					t.Errorf("omitted required FeatureWire.%s was reconstructed from its zero: %v", field, err)
				}
			})
		}
	}
	if admitted, err := nha047.DecodeCase(data); err != nil || admitted.Digest() != fixture.Digest() {
		t.Fatalf("explicit nested zero parameters must remain admitted: %v", err)
	}
}

func TestNHA047CaptureRequiresNestedZeroAndEmptyDemandFields(t *testing.T) {
	fixture := caseByID(t, "scene-light-off")
	demand, err := nha047.NewDemand(nha047.LightOff, "light.nha047_value", 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	data, err := nha047.EncodeCapture(fixture, "go", nha047.GoVersion, syntheticInputSnapshot(t, fixture, "go"),
		[]nha047.SourceHash{{Path: "probe", SHA256: strings.Repeat("a", 64)}},
		[]nha047.Demand{demand}, true, true)
	if err != nil {
		t.Fatal(err)
	}
	// This is a synthetic decoder oracle, not a claim of native/HA execution.
	for _, field := range []string{"value", "secondary", "code"} {
		t.Run("absent_"+field, func(t *testing.T) {
			mutated := omitPresenceElementField(t, data, "demands", 0, field)
			if _, err := nha047.DecodeCapture(mutated); !errors.Is(err, nha047.ErrInvalidCapture) {
				t.Errorf("capture admitted an omitted required DemandWire.%s: %v", field, err)
			}
		})
	}
	for _, field := range []string{"kind", "target"} {
		t.Run("nonzero_control_"+field, func(t *testing.T) {
			if _, err := nha047.DecodeCapture(omitPresenceElementField(t, data, "demands", 0, field)); err == nil {
				t.Errorf("missing nonzero %s control was unexpectedly admitted", field)
			}
		})
	}
	for _, field := range []string{"path", "sha256"} {
		t.Run("source_control_"+field, func(t *testing.T) {
			if _, err := nha047.DecodeCapture(omitPresenceElementField(t, data, "sources", 0, field)); err == nil {
				t.Errorf("missing source provenance %s was unexpectedly admitted", field)
			}
		})
	}
	if _, err := nha047.DecodeCapture(data); err != nil {
		t.Fatalf("explicit zero/empty demand fields must remain admitted: %v", err)
	}
}

func TestNHA047CasePresenceNegativeControlsRemainStrict(t *testing.T) {
	fixture := caseByID(t, "scene-light-89-357")
	data, err := fixture.JSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"schema", "id", "scenario", "object", "yaml"} {
		t.Run("nonzero_"+field, func(t *testing.T) {
			if _, err := nha047.DecodeCase(omitPresenceField(t, data, field)); err == nil {
				t.Errorf("missing nonzero %s control was unexpectedly admitted", field)
			}
		})
	}
	for _, field := range []string{"kind", "target", "value", "secondary"} {
		t.Run("nonzero_demand_"+field, func(t *testing.T) {
			if _, err := nha047.DecodeCase(omitPresenceElementField(t, data, "go_expected", 0, field)); err == nil {
				t.Errorf("missing nonzero demand %s control was unexpectedly admitted", field)
			}
		})
	}
	object := presenceObject(t, data)
	object["season"] = json.RawMessage("null")
	if _, err := nha047.DecodeCase(presenceEncode(t, object)); err == nil {
		t.Error("null must not acquire the meaning false")
	}
}
