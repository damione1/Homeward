package nha045

import (
	"encoding/json"
	"testing"
)

// Boundary regressions preserve all ten fixture snapshots and change one
// contradictory input at a time.
func TestNHA045FixtureRefusesCrossPhasePayload(t *testing.T) {
	all, err := Corpus()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		fixture int
		kind    PhaseKind
		edit    func(*phaseWire)
	}{
		{"advance_run", 0, Advance, func(p *phaseWire) { p.Run = "a" }},
		{"advance_entity", 0, Advance, func(p *phaseWire) { p.Entity = "switch.nha045_effect" }},
		{"invoke_state", 0, Invoke, func(p *phaseWire) { p.State = "on" }},
		{"invoke_count", 0, Invoke, func(p *phaseWire) { p.Count = 1 }},
		{"startup_wrong_object", 0, AwaitStartup, func(p *phaseWire) { p.Object = "script.nha045_absent" }},
		{"terminal_wrong_object", 0, AwaitTerminal, func(p *phaseWire) { p.Object = "script.nha045_absent" }},
		{"publish_run", 6, Publish, func(p *phaseWire) { p.Run = "a" }},
		{"publish_brightness_over_profile", 6, Publish, func(p *phaseWire) { p.Brightness = ptr(101.0) }},
		{"publish_brightness_negative", 6, Publish, func(p *phaseWire) { p.Brightness = ptr(-1.0) }},
		{"emit_state", 4, Emit, func(p *phaseWire) { p.State = "on" }},
		{"waiting_count", 4, AwaitWaiting, func(p *phaseWire) { p.Count = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := all[tc.fixture].JSON()
			if err != nil {
				t.Fatal(err)
			}
			var wire fixtureWire
			if err = json.Unmarshal(data, &wire); err != nil {
				t.Fatal(err)
			}
			found := false
			for i := range wire.Phases {
				if wire.Phases[i].Kind == phaseName(tc.kind) {
					tc.edit(&wire.Phases[i])
					found = true
					break
				}
			}
			if !found {
				t.Fatal("counterexample references absent phase")
			}
			data, err = json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = DecodeFixture(data); !issueIs(err, InvalidFixture) {
				t.Fatalf("contradictory phase admitted: %v", err)
			}
		})
	}
}

func TestNHA045FixtureRefusesNullPrimitivePhaseFields(t *testing.T) {
	all, err := Corpus()
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"at_ms", "count"} {
		t.Run(field, func(t *testing.T) {
			data, err := all[0].JSON()
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]json.RawMessage
			if err = json.Unmarshal(data, &wire); err != nil {
				t.Fatal(err)
			}
			var phases []map[string]json.RawMessage
			if err = json.Unmarshal(wire["phases"], &phases); err != nil {
				t.Fatal(err)
			}
			// First Invoke has valid primitive zeroes: null must not silently
			// decode into those otherwise valid zero-valued fields.
			phases[0][field] = json.RawMessage("null")
			wire["phases"], err = json.Marshal(phases)
			if err != nil {
				t.Fatal(err)
			}
			data, err = json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = DecodeFixture(data); !issueIs(err, InvalidFixture) {
				t.Fatalf("primitive JSON null silently admitted: %v", err)
			}
		})
	}
}
