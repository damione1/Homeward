package nha045

import (
	"testing"

	"github.com/damione1/Homeward/internal/automationruns"
)

func TestNHA045NativeRefusalRejectsZeroAndNonRefusalDecisions(t *testing.T) {
	for _, decision := range []automationruns.Decision{automationruns.DecisionInvalid, automationruns.DecisionFinished, automationruns.DecisionAdmitted, automationruns.Decision(255)} {
		if _, e := admitNativeRefusal(decision); !issueIs(e, InvalidObservation) {
			t.Fatalf("non-refusal native decision admitted: %d %v", decision, e)
		}
	}
	for _, tc := range []struct {
		native automationruns.Decision
		want   Outcome
	}{{automationruns.DecisionSingleBusy, SingleBusy}, {automationruns.DecisionMaxExceeded, MaxExceeded}} {
		value, e := admitNativeRefusal(tc.native)
		if e != nil {
			t.Fatal(e)
		}
		got, e := value.outcome()
		if e != nil || got != tc.want {
			t.Fatalf("native decision changed: %d %v", tc.native, e)
		}
	}
}

func TestNHA045ExpectedChecksHaveExplicitAbsenceAndOwnedValues(t *testing.T) {
	var typedNil *actionCheck
	for _, value := range []expectedCheck{nil, typedNil, admissionCheck{}, actionCheck{}, terminalCheck{}} {
		if _, e := describeCheck(value); e == nil {
			t.Fatal("zero or typed-nil expected check admitted")
		}
	}
	boundary := []byte(`{"kind":"admission","run":null,"entity":null,"at_ms":0,"outcome":"accepted"}`)
	wire, e := decodeCheckBoundary(boundary)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := admitCheck(wire); e != nil {
		t.Fatalf("explicit optional absence refused: %v", e)
	}
	for _, bad := range [][]byte{
		[]byte(`{"kind":"admission","entity":null,"at_ms":0,"outcome":"accepted"}`),
		[]byte(`{"kind":"admission","run":null,"at_ms":0,"outcome":"accepted"}`),
	} {
		if _, e := decodeCheckBoundary(bad); e == nil {
			t.Fatal("implicit expected absence admitted")
		}
	}
	run, entity, outcome := "a", "switch.nha045_effect", "completed"
	input := checkWire{Kind: factName(Action), Run: &run, Entity: &entity, Outcome: &outcome}
	value, e := admitCheck(input)
	if e != nil {
		t.Fatal(e)
	}
	run = "b"
	entity = "switch.nha045_other"
	outcome = "command_failed"
	description, e := describeCheck(value)
	if e != nil {
		t.Fatal(e)
	}
	if !description.target.run.present || description.target.run.value.value != "a" || !description.entity.present || description.entity.value.value != "switch.nha045_effect" || description.outcome != Completed {
		t.Fatal("admitted expected check retained wire pointers")
	}
}
