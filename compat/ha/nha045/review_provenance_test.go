package nha045

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
)

func TestNHA045FixtureCaptureProvenance(t *testing.T) {
	r, native := joinedNativeOrderFixture(t, 0)
	original := r.f
	goCapture := projectedNativeOrder(t, r, native)
	// This HA reference changes metadata only. Its facts/order come from the
	// actual synthetic Go rig; it makes no claim of HA execution or isolation.
	haCapture := syntheticNativeOrderReference(t, goCapture)
	originalJSON, err := original.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var revisedWire fixtureWire
	if err := json.Unmarshal(originalJSON, &revisedWire); err != nil {
		t.Fatal(err)
	}
	if original.ID() != "01-restart-delay" || len(revisedWire.Objects) != 1 || strings.Count(revisedWire.Objects[0].YAML, "mode: restart\n") != 1 {
		t.Fatal("precondition: original restart fixture is not unique")
	}
	revisedWire.Objects[0].YAML = strings.Replace(revisedWire.Objects[0].YAML, "mode: restart\n", "mode: single\n", 1)
	revisedJSON, err := json.Marshal(revisedWire)
	if err != nil {
		t.Fatal(err)
	}
	revised, err := DecodeFixture(revisedJSON)
	if err != nil {
		t.Fatalf("precondition: revised fixture was not admitted: %v", err)
	}
	if revised.ID() != original.ID() || revised.SHA256() == original.SHA256() ||
		!reflect.DeepEqual(revised.wire.Phases, original.wire.Phases) ||
		!reflect.DeepEqual(revised.wire.Checks, original.wire.Checks) ||
		!reflect.DeepEqual(revised.wire.Entities, original.wire.Entities) ||
		!reflect.DeepEqual(revised.wire.Forbidden, original.wire.Forbidden) {
		t.Fatal("precondition: fixture revision did not preserve identity/checks/phases and change its SHA")
	}
	// Prove that the revised YAML also passes the real delivered compiler.
	// Its phases are deliberately not executed: this test reuses old captures.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	revisedRig, err := buildRig(ctx, revised)
	if err != nil {
		t.Fatalf("precondition: revised YAML did not compile: %v", err)
	}
	t.Cleanup(func() {
		if err := revisedRig.dispose(); err != nil {
			t.Error(err)
		}
	})
	if revisedRig.scripts[revised.wire.Root].Mode() != ir.AutomationSingle {
		t.Fatal("precondition: revised YAML did not produce native single mode")
	}
	haJSON, err := haCapture.JSON()
	if err != nil {
		t.Fatal(err)
	}
	goJSON, err := goCapture.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if haCapture.wire.FixtureSHA256 != original.SHA256() || goCapture.wire.FixtureSHA256 != original.SHA256() {
		t.Fatal("precondition: old captures lost their original fixture provenance")
	}
	input := func(fixture json.RawMessage) []byte {
		t.Helper()
		encoded, err := json.Marshal(struct {
			Fixture json.RawMessage `json:"fixture"`
			HA      json.RawMessage `json:"ha"`
			Go      json.RawMessage `json:"go"`
		}{Fixture: fixture, HA: haJSON, Go: goJSON})
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	positive := func(t *testing.T) {
		t.Helper()
		comparison, err := CompareInput(input(originalJSON))
		if err != nil || !comparison.Equivalent || len(comparison.Differences) != 0 {
			t.Fatalf("exact fixture provenance must remain equivalent: %+v %v", comparison, err)
		}
		for _, capture := range []Capture{haCapture, goCapture} {
			if err := ValidateExpected(original, capture); err != nil {
				t.Fatalf("exact fixture expectations refused: %v", err)
			}
		}
	}
	t.Run("original", positive)
	t.Run("revised_fixture_compare_input", func(t *testing.T) {
		comparison, err := CompareInput(input(revisedJSON))
		if !issueIs(err, InvalidObservation) || comparison.Equivalent || len(comparison.Differences) == 0 {
			t.Errorf("equal old captures accepted under revised fixture: %+v %v", comparison, err)
		}
	})
	for _, test := range []struct {
		name    string
		capture Capture
	}{{"revised_fixture_validate_ha", haCapture}, {"revised_fixture_validate_go", goCapture}} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateExpected(revised, test.capture); !issueIs(err, InvalidObservation) {
				t.Errorf("old capture accepted under revised fixture expectations: %v", err)
			}
		})
	}
	t.Run("original_replay", positive)
}
