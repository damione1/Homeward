package nha045

import (
	"encoding/json"
	"testing"
)

func TestNHA045PhaseBoundaryRefusesNullAndWrongPrimitiveTypes(t *testing.T) {
	all, e := Corpus()
	if e != nil {
		t.Fatal(e)
	}
	data, e := all[0].JSON()
	if e != nil {
		t.Fatal(e)
	}
	var fixture fixtureWire
	if e = json.Unmarshal(data, &fixture); e != nil {
		t.Fatal(e)
	}
	data, e = json.Marshal(fixture.Phases[0])
	if e != nil {
		t.Fatal(e)
	}
	for _, field := range []string{"id", "kind", "at_ms", "run", "object", "entity", "state", "count"} {
		for _, invalid := range []json.RawMessage{json.RawMessage("null"), json.RawMessage("false")} {
			t.Run(field+"/"+string(invalid), func(t *testing.T) {
				var value map[string]json.RawMessage
				if e := json.Unmarshal(data, &value); e != nil {
					t.Fatal(e)
				}
				value[field] = invalid
				encoded, e := json.Marshal(value)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = decodePhaseBoundary(encoded); e == nil {
					t.Fatal("invalid primitive admitted")
				}
			})
		}
	}
	if _, e = decodePhaseBoundary(data); e != nil {
		t.Fatalf("explicit nullable brightness refused: %v", e)
	}
}

func TestNHA045PhaseZeroTypedNilAndBoundaryCopies(t *testing.T) {
	id, e := parseID("phase-001")
	if e != nil {
		t.Fatal(e)
	}
	var absent *invokePhase
	for _, p := range []phase{{}, {id: phaseID{id}, valid: true}, {id: phaseID{id}, operation: absent, valid: true}, {id: phaseID{id}, operation: invokePhase{}, valid: true}, {id: phaseID{id}, operation: publishPhase{}, valid: true}} {
		if _, e := p.boundary(); e == nil {
			t.Fatal("zero or typed-nil phase admitted")
		}
	}
	objects := map[string]bool{"script.nha045_root": true}
	entities := map[string]bool{"light.nha045_source": true}
	input := phaseWire{ID: "phase-001", Kind: phaseName(Publish), Entity: "light.nha045_source", State: "on", Brightness: ptr(1.0)}
	p, e := admitPhase(input, objects, entities, make(map[runKey]objectID))
	if e != nil {
		t.Fatal(e)
	}
	*input.Brightness = 99
	first, e := p.boundary()
	if e != nil {
		t.Fatal(e)
	}
	if first.Brightness == nil || *first.Brightness != 1 {
		t.Fatal("phase retained mutable input")
	}
	*first.Brightness = 98
	second, e := p.boundary()
	if e != nil {
		t.Fatal(e)
	}
	if second.Brightness == nil || *second.Brightness != 1 {
		t.Fatal("phase boundary exposed admitted storage")
	}
}
