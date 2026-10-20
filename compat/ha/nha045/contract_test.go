package nha045

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func testCapture(t *testing.T, name, raw string) Capture {
	t.Helper()
	runtime := runtimeWire{Name: name, Version: GoVersion, SourceSHA256: []sourceWire{{"probe", strings.Repeat("a", 64)}}}
	if name == "ha" {
		runtime.Version = HAVersion
		runtime.Python = ptr(HAPython)
		runtime.Jinja = ptr(HAJinja)
		runtime.SourceSHA256 = append([]sourceWire(nil), haSources...)
	}
	w := captureWire{Schema: Schema, Scenario: "fixture", FixtureSHA256: strings.Repeat("a", 64), Runtime: runtime, Runs: []runWire{{Key: "a", Raw: raw, Object: "script.nha045_root"}}, Facts: []factWire{
		{Key: "a/start", Kind: "start", Run: ptr(raw), Object: "script.nha045_root", Node: "entry"},
		{Key: "a/action/switch.nha045_effect/1", Kind: "action", Run: ptr(raw), AtMS: 5000, Object: "script.nha045_root", Node: "switch.nha045_effect/1", Data: dataWire{Outcome: ptr("completed"), Entity: ptr("switch.nha045_effect"), Service: ptr("switch.turn_on")}},
		{Key: "a/terminal", Kind: "terminal", Run: ptr(raw), AtMS: 5000, Object: "script.nha045_root", Node: "terminal", Data: dataWire{Outcome: ptr("finished")}},
	}, Edges: []edgeWire{{"a/start", "a/action/switch.nha045_effect/1"}, {"a/action/switch.nha045_effect/1", "a/terminal"}}, Orders: []streamWire{{"control", []string{"a/start", "a/terminal"}}, {"node/a/root", []string{"a/action/switch.nha045_effect/1"}}}, Terminal: ptr(true), Raw: json.RawMessage(`{}`)}
	data, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	c, err := DecodeCapture(data)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func changed(t *testing.T, c Capture, edit func(*captureWire)) Capture {
	t.Helper()
	data, e := c.JSON()
	if e != nil {
		t.Fatal(e)
	}
	var w captureWire
	if e = json.Unmarshal(data, &w); e != nil {
		t.Fatal(e)
	}
	edit(&w)
	data, e = json.Marshal(w)
	if e != nil {
		t.Fatal(e)
	}
	out, e := DecodeCapture(data)
	if e != nil {
		t.Fatal(e)
	}
	return out
}
func issueIs(err error, want IssueKind) bool {
	var issue *Issue
	return errors.As(err, &issue) && issue.Kind() == want
}

func TestNHA045OpaqueRunRenamingIsBijective(t *testing.T) {
	ha := testCapture(t, "ha", "ha-context-123")
	g := testCapture(t, "go", "3/7")
	result, e := Compare(ha, g)
	if e != nil || !result.Equivalent {
		t.Fatalf("renaming diverged: %+v %v", result, e)
	}
}
func TestNHA045ActionTypeMultiplicityAndOrderAreNotSortedAway(t *testing.T) {
	ha := testCapture(t, "ha", "ha-a")
	g := testCapture(t, "go", "go-a")
	tests := []struct {
		name string
		edit func(*captureWire)
	}{
		{"missing_action", func(w *captureWire) {
			w.Facts = append(w.Facts[:1], w.Facts[2:]...)
			w.Edges = []edgeWire{{"a/start", "a/terminal"}}
			w.Orders = w.Orders[:1]
		}},
		{"duplicate_action", func(w *captureWire) {
			v := w.Facts[1]
			v.Key += "-duplicate"
			w.Facts = append(w.Facts, v)
			w.Orders[1].Keys = append(w.Orders[1].Keys, v.Key)
		}},
		{"inverted_fifo", func(w *captureWire) {
			w.Edges = []edgeWire{{"a/terminal", "a/action/switch.nha045_effect/1"}, {"a/start", "a/terminal"}}
		}},
		{"lost_join", func(w *captureWire) { w.Edges = w.Edges[:1] }},
		{"changed_payload", func(w *captureWire) { w.Facts[1].Data.Entity = ptr("switch.nha045_other") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, e := Compare(ha, changed(t, g, test.edit))
			if !issueIs(e, SemanticDivergence) || result.Equivalent || len(result.Differences) == 0 {
				t.Fatalf("change hidden: %+v %v", result, e)
			}
		})
	}
}
func TestNHA045TransitiveEdgesAndIndependentCollectionOrder(t *testing.T) {
	ha := testCapture(t, "ha", "ha-a")
	g := changed(t, testCapture(t, "go", "go-a"), func(w *captureWire) {
		w.Edges = append(w.Edges, edgeWire{"a/start", "a/terminal"})
		w.Facts[0], w.Facts[2] = w.Facts[2], w.Facts[0]
	})
	if _, e := Compare(ha, g); e != nil {
		t.Fatal(e)
	}
}
func TestNHA045RejectsRunFusionOrphansCyclesAndMissingFields(t *testing.T) {
	c := testCapture(t, "go", "go-a")
	data, _ := c.JSON()
	var original captureWire
	if e := json.Unmarshal(data, &original); e != nil {
		t.Fatal(e)
	}
	tests := []struct {
		name string
		edit func(*captureWire)
	}{
		{"fusion", func(w *captureWire) {
			w.Runs = append(w.Runs, runWire{Key: "b", Raw: "go-a", Object: "script.nha045_root"})
		}},
		{"orphan", func(w *captureWire) { w.Facts[1].Run = ptr("unknown") }},
		{"orphan_parent", func(w *captureWire) { w.Runs[0].Parent = ptr("missing"); w.Runs[0].Callsite = ptr("sequence[0]") }},
		{"cycle", func(w *captureWire) { w.Edges = append(w.Edges, edgeWire{"a/terminal", "a/start"}) }},
		{"evicted", func(w *captureWire) { w.Evicted = 1 }},
		{"not_joined", func(w *captureWire) { w.Terminal = ptr(false) }},
		{"absent_outcome", func(w *captureWire) { w.Facts[1].Data.Outcome = nil }},
		{"wrong_variant", func(w *captureWire) { w.Facts[0].Data.Entity = ptr("switch.nha045_effect") }},
		{"state_with_execution_outcome", func(w *captureWire) {
			w.Facts = append(w.Facts, factWire{Key: "state/contradiction", Kind: factName(State), Object: "house", Node: "state", Data: dataWire{Outcome: ptr(outcomeName(CommandFailed)), Entity: ptr("switch.nha045_effect"), State: ptr("on")}})
		}},
		{"missing_sources", func(w *captureWire) { w.Runtime.SourceSHA256 = nil }},
		{"duplicate_sources", func(w *captureWire) {
			w.Runtime.SourceSHA256 = append(w.Runtime.SourceSHA256, w.Runtime.SourceSHA256[0])
		}},
		{"source_empty_path", func(w *captureWire) { w.Runtime.SourceSHA256[0].Path = "" }},
		{"source_invalid_sha", func(w *captureWire) { w.Runtime.SourceSHA256[0].SHA256 = "not-a-digest" }},
		{"missing_native_terminal", func(w *captureWire) { w.Orders[0].Keys = w.Orders[0].Keys[:1] }},
		{"wrong_native_run", func(w *captureWire) { w.Orders[1].Scope = "node/b/root" }},
		{"duplicate_native_witness", func(w *captureWire) {
			w.Orders = append(w.Orders, streamWire{"node/a/root/1", []string{w.Facts[1].Key}})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var w captureWire
			if e := json.Unmarshal(data, &w); e != nil {
				t.Fatal(e)
			}
			test.edit(&w)
			b, e := json.Marshal(w)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = DecodeCapture(b); !issueIs(e, InvalidObservation) {
				t.Fatalf("bad capture admitted: %v", e)
			}
		})
	}
	for _, b := range []string{`{}`, strings.Replace(string(data), `"at_ms": 0,`, "", 1), strings.Replace(string(data), `"schema":`, `"unexpected":0,"schema":`, 1), strings.Replace(string(data), `"schema":`, `"schema":"nha045/v1","schema":`, 1)} {
		if _, e := DecodeCapture([]byte(b)); !issueIs(e, InvalidObservation) {
			t.Fatalf("malformed JSON admitted: %v", e)
		}
	}
}
func TestNHA045ZeroValuesAndSnapshots(t *testing.T) {
	if _, e := (Capture{}).JSON(); !issueIs(e, InvalidObservation) {
		t.Fatal("zero capture admitted")
	}
	if _, e := (Fixture{}).JSON(); !issueIs(e, InvalidFixture) {
		t.Fatal("zero fixture admitted")
	}
	c := testCapture(t, "go", "go-a")
	a, _ := c.JSON()
	a[0] = 'x'
	b, e := c.JSON()
	if e != nil || b[0] != '{' {
		t.Fatal("mutable snapshot exposed")
	}
}
func TestNHA045TenFixturesStayWithinAdmittedProfile(t *testing.T) {
	all, e := Corpus()
	if e != nil {
		t.Fatal(e)
	}
	if len(all) != 10 {
		t.Fatalf("fixtures: %d", len(all))
	}
	seen := make(map[string]bool)
	for _, f := range all {
		if seen[f.ID()] || !admittedSHA(f.SHA256()) {
			t.Fatal("identity/hash invalid")
		}
		seen[f.ID()] = true
		data, e := f.JSON()
		if e != nil {
			t.Fatal(e)
		}
		copy, e := DecodeFixture(data)
		if e != nil || copy.SHA256() != f.SHA256() {
			t.Fatal("roundtrip drift")
		}
		for _, o := range f.wire.Objects {
			for _, outside := range []string{"input_boolean.", "counter.increment", "input_number.", "scene.create", "192.168.", "!include", "!secret"} {
				if strings.Contains(o.YAML, outside) {
					t.Fatalf("%s imports %s", f.ID(), outside)
				}
			}
		}
	}
}
func TestNHA045FixtureRejectsUnknownMissingAndOversizedInput(t *testing.T) {
	all, e := Corpus()
	if e != nil {
		t.Fatal(e)
	}
	data, _ := all[0].JSON()
	for _, bad := range [][]byte{[]byte(`{}`), append(append([]byte(nil), data...), data...), []byte(strings.Repeat(" ", MaxInputBytes+1)), []byte(strings.Replace(string(data), `"schema":`, `"new_field":false,"schema":`, 1))} {
		if _, e := DecodeFixture(bad); !issueIs(e, InvalidFixture) {
			t.Fatalf("bad fixture admitted: %v", e)
		}
	}
}
