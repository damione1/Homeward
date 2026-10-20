package nha045

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/damione1/Homeward/internal/automationruns"
)

func TestNHA045NativeSnapshotAdmissionAndJoins(t *testing.T) {
	r, normal := joinedNativeOrderFixture(t, 5)
	foreign, foreignSnapshot := joinedNativeOrderFixture(t, 9)
	if err := r.qualifyNative(normal, true); err != nil {
		t.Fatalf("positive joined native snapshot: %v", err)
	}
	if err := r.businessTerminals(normal); err != nil {
		t.Fatalf("positive business terminals: %v", err)
	}
	if err := stableNativePrefix(normal, normal.copy()); err != nil {
		t.Fatal(err)
	}
	if err := stableNativePrefix(nativeObservation{}, nativeObservation{}); !issueIs(err, InvalidObservation) {
		t.Fatalf("zero prefixes admitted: %v", err)
	}
	cases := []struct {
		name   string
		change func(*nativeObservation)
	}{
		{"zero", func(o *nativeObservation) { *o = nativeObservation{} }},
		{"nil-controller", func(o *nativeObservation) { o.controller = nil }},
		{"foreign-controller", func(o *nativeObservation) { o.controller = foreign.controller }},
		{"nil-interpreter", func(o *nativeObservation) { o.interpreter = nil }},
		{"foreign-interpreter", func(o *nativeObservation) { o.interpreter = foreign.interpreter }},
		{"unhealthy", func(o *nativeObservation) { o.state = automationruns.TraceOrderExhausted }},
		{"decision-loss", func(o *nativeObservation) { o.decisionEvicted = 1 }},
		{"node-loss", func(o *nativeObservation) { o.nodeEvicted = 1 }},
		{"not-joined", func(o *nativeObservation) { o.joined = false }},
		{"zero-node-position", func(o *nativeObservation) { o.nodes[0].position = automationruns.TraceOrdinal{} }},
		{"zero-decision-position", func(o *nativeObservation) { o.decisions[0].position = automationruns.TraceOrdinal{} }},
		{"duplicate-position", func(o *nativeObservation) { o.nodes[0].position = o.decisions[0].position }},
		{"node-ring-reordered", func(o *nativeObservation) {
			o.nodes[0].position, o.nodes[1].position = o.nodes[1].position, o.nodes[0].position
		}},
		{"decision-ring-reordered", func(o *nativeObservation) {
			o.decisions[0].position, o.decisions[1].position = o.decisions[1].position, o.decisions[0].position
		}},
		{"foreign-node-payload", func(o *nativeObservation) { o.nodes[0].NodeTrace = foreignSnapshot.nodes[0].NodeTrace }},
		{"missing-final-node", func(o *nativeObservation) { o.nodes = o.nodes[:len(o.nodes)-1] }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			o := normal.copy()
			test.change(&o)
			if err := r.qualifyNative(o, true); !issueIs(err, InvalidObservation) {
				t.Fatalf("invalid native snapshot admitted: %v", err)
			}
		})
	}
	var absent *rig
	if err := absent.qualifyNative(normal, true); !issueIs(err, InvalidObservation) {
		t.Fatalf("typed nil owner admitted: %v", err)
	}
	changed := normal.copy()
	changed.nodes = changed.nodes[:len(changed.nodes)-1]
	if err := stableNativePrefix(normal, changed); !issueIs(err, InvalidObservation) {
		t.Fatalf("cleanup changed prefix: %v", err)
	}
	changed = normal.copy()
	changed.decisions = changed.decisions[:len(changed.decisions)-1]
	if err := r.businessTerminals(changed); !issueIs(err, InvalidObservation) {
		t.Fatalf("terminal manufactured only by cleanup admitted: %v", err)
	}
}

func nativeProofBoundary(t *testing.T, c Capture) orderProofWire {
	t.Helper()
	var envelope struct {
		Order orderProofWire `json:"order"`
	}
	if err := json.Unmarshal(c.wire.Raw, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Order
}

func nativeProofJSON(t *testing.T, w orderProofWire) []byte {
	t.Helper()
	encoded, err := json.Marshal(struct {
		Producer string         `json:"producer"`
		Order    orderProofWire `json:"order"`
	}{w.Producer, w})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func rawObject(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	return object
}
func rawJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestNHA045NativeOrderProofStrictBoundary(t *testing.T) {
	r, observation := joinedNativeOrderFixture(t, 9)
	capture := projectedNativeOrder(t, r, observation)
	wire := nativeProofBoundary(t, capture)
	if len(wire.Points) < 4 || len(wire.StateCauses) != 1 {
		t.Fatal("precondition: real branch/join/effect witnesses absent")
	}
	encoded := nativeProofJSON(t, wire)
	if proof, err := decodeOrderProof(encoded, capture); err != nil || !proof.valid {
		t.Fatalf("positive native proof: %v", err)
	}
	duplicateRoot := append([]byte(`{"producer":"foreign",`), encoded[1:]...)
	if _, err := decodeOrderProof(duplicateRoot, capture); !issueIs(err, InvalidObservation) {
		t.Fatalf("duplicate producer root admitted: %v", err)
	}
	type mutation struct {
		name   string
		change func(map[string]json.RawMessage)
	}
	cases := []mutation{
		{"absent-order", func(root map[string]json.RawMessage) { delete(root, "order") }},
		{"null-order", func(root map[string]json.RawMessage) { root["order"] = json.RawMessage("null") }},
		{"foreign-producer", func(root map[string]json.RawMessage) { root["producer"] = json.RawMessage(`"another-controller"`) }},
	}
	for _, field := range []string{"producer", "healthy", "joined", "decision_evicted", "node_evicted", "points", "state_causes"} {
		for _, absent := range []bool{false, true} {
			name := field + "-null"
			if absent {
				name = field + "-absent"
			}
			cases = append(cases, mutation{name, func(root map[string]json.RawMessage) {
				order := rawObject(t, root["order"])
				if absent {
					delete(order, field)
				} else {
					order[field] = json.RawMessage("null")
				}
				root["order"] = rawJSON(t, order)
			}})
		}
	}
	orderChange := func(name string, change func(map[string]json.RawMessage)) {
		cases = append(cases, mutation{name, func(root map[string]json.RawMessage) {
			order := rawObject(t, root["order"])
			change(order)
			root["order"] = rawJSON(t, order)
		}})
	}
	orderChange("unknown-order-field", func(order map[string]json.RawMessage) { order["invented"] = json.RawMessage("true") })
	for _, field := range []string{"healthy", "joined"} {
		orderChange(field+"-false", func(order map[string]json.RawMessage) { order[field] = json.RawMessage("false") })
	}
	for _, field := range []string{"decision_evicted", "node_evicted"} {
		orderChange(field+"-loss", func(order map[string]json.RawMessage) { order[field] = json.RawMessage("1") })
	}
	pointChange := func(name string, change func([]map[string]json.RawMessage)) {
		orderChange(name, func(order map[string]json.RawMessage) {
			var points []map[string]json.RawMessage
			if err := json.Unmarshal(order["points"], &points); err != nil {
				t.Fatal(err)
			}
			change(points)
			order["points"] = rawJSON(t, points)
		})
	}
	for _, field := range []string{"key", "ordinal"} {
		pointChange("point-"+field+"-null", func(points []map[string]json.RawMessage) { points[0][field] = json.RawMessage("null") })
		pointChange("point-"+field+"-absent", func(points []map[string]json.RawMessage) { delete(points[0], field) })
	}
	pointChange("point-zero", func(points []map[string]json.RawMessage) { points[0]["ordinal"] = json.RawMessage("0") })
	pointChange("point-reserved", func(points []map[string]json.RawMessage) {
		points[0]["ordinal"] = json.RawMessage("18446744073709551615")
	})
	pointChange("point-unknown-field", func(points []map[string]json.RawMessage) { points[0]["guessed"] = json.RawMessage("true") })
	pointChange("point-foreign-key", func(points []map[string]json.RawMessage) { points[0]["key"] = json.RawMessage(`"another-run/start"`) })
	pointChange("point-duplicate-key", func(points []map[string]json.RawMessage) { points[1]["key"] = points[0]["key"] })
	pointChange("point-duplicate-position", func(points []map[string]json.RawMessage) { points[1]["ordinal"] = points[0]["ordinal"] })
	orderChange("missing-native-point", func(order map[string]json.RawMessage) { order["points"] = rawJSON(t, wire.Points[1:]) })
	orderChange("ambiguous-effect-cause", func(order map[string]json.RawMessage) {
		order["state_causes"] = rawJSON(t, append(append([]stateCauseWire{}, wire.StateCauses...), wire.StateCauses[0]))
	})
	orderChange("missing-effect-cause", func(order map[string]json.RawMessage) { order["state_causes"] = json.RawMessage("[]") })
	for _, field := range []string{"state", "action"} {
		orderChange("cause-"+field+"-null", func(order map[string]json.RawMessage) {
			cause := map[string]json.RawMessage{"state": rawJSON(t, wire.StateCauses[0].State), "action": rawJSON(t, wire.StateCauses[0].Action)}
			cause[field] = json.RawMessage("null")
			order["state_causes"] = rawJSON(t, []map[string]json.RawMessage{cause})
		})
		orderChange("cause-"+field+"-absent", func(order map[string]json.RawMessage) {
			cause := map[string]json.RawMessage{"state": rawJSON(t, wire.StateCauses[0].State), "action": rawJSON(t, wire.StateCauses[0].Action)}
			delete(cause, field)
			order["state_causes"] = rawJSON(t, []map[string]json.RawMessage{cause})
		})
	}
	orderChange("cause-unknown-field", func(order map[string]json.RawMessage) {
		cause := map[string]json.RawMessage{"state": rawJSON(t, wire.StateCauses[0].State), "action": rawJSON(t, wire.StateCauses[0].Action), "guess": json.RawMessage("true")}
		order["state_causes"] = rawJSON(t, []map[string]json.RawMessage{cause})
	})
	orderChange("foreign-effect-cause", func(order map[string]json.RawMessage) {
		cause := wire.StateCauses[0]
		cause.Action = "another-run/action/1"
		order["state_causes"] = rawJSON(t, []stateCauseWire{cause})
	})
	orderChange("failed-action-as-cause", func(order map[string]json.RawMessage) {
		cause := wire.StateCauses[0]
		cause.Action = "a/action/switch.nha045_fail/1"
		order["state_causes"] = rawJSON(t, []stateCauseWire{cause})
	})
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			root := rawObject(t, encoded)
			test.change(root)
			if _, err := decodeOrderProof(rawJSON(t, root), capture); !issueIs(err, InvalidObservation) {
				t.Fatalf("invalid native raw proof admitted: %v", err)
			}
		})
	}
	// Neither stream sequence nor the selected emission positions may overwrite
	// the other at admission. This changes two admitted positions, not a graph.
	reordered := nativeProofBoundary(t, capture)
	left, right := -1, -1
	for index, point := range reordered.Points {
		if point.Key == "a/wait/1" {
			left = index
		}
		if point.Key == "a/action/switch.nha045_other/1" {
			right = index
		}
	}
	if left < 0 || right < 0 {
		t.Fatal("precondition: sequential branch witnesses absent")
	}
	reordered.Points[left].Ordinal, reordered.Points[right].Ordinal = reordered.Points[right].Ordinal, reordered.Points[left].Ordinal
	if _, err := decodeOrderProof(nativeProofJSON(t, reordered), capture); !issueIs(err, InvalidObservation) {
		t.Fatalf("nonmonotone native branch admitted: %v", err)
	}
	if _, err := observedCausalEdges(capture.wire, r.f, nativeOrderProof{}); !issueIs(err, InvalidObservation) {
		t.Fatalf("zero admitted proof projected: %v", err)
	}
	// Metadata-only HA admission control; it claims no HA execution. A callback
	// producer supplies its original emission list, not a post-capture sort.
	ha := syntheticNativeOrderReference(t, capture)
	callback := nativeProofBoundary(t, capture)
	sort.Slice(callback.Points, func(i, j int) bool { return callback.Points[i].Ordinal < callback.Points[j].Ordinal })
	if _, err := decodeOrderProof(nativeProofJSON(t, callback), ha); err != nil {
		t.Fatalf("positive synthetic callback proof: %v", err)
	}
	callback.Points[0], callback.Points[1] = callback.Points[1], callback.Points[0]
	if _, err := decodeOrderProof(nativeProofJSON(t, callback), ha); !issueIs(err, InvalidObservation) {
		t.Fatalf("reordered HA callback list admitted: %v", err)
	}
	// A failing call cannot explain an observed effect even when entity/run are
	// identical. This isolates typed outcome admission from foreign-ID checks.
	contradictory := changed(t, capture, func(w *captureWire) {
		for i := range w.Facts {
			if w.Facts[i].Key == wire.StateCauses[0].Action {
				w.Facts[i].Data.Outcome = ptr(outcomeName(CommandFailed))
			}
		}
	})
	if _, err := decodeOrderProof(encoded, contradictory); !issueIs(err, InvalidObservation) {
		t.Fatalf("failed call accepted as actual publication cause: %v", err)
	}
}

func TestNHA045NativeInputCutAndEffectOwnerAreProved(t *testing.T) {
	r, native := joinedNativeOrderFixture(t, 8)
	capture := projectedNativeOrder(t, r, native)
	wire := nativeProofBoundary(t, capture)
	if len(capture.runs) != 2 || len(wire.StateCauses) < 2 {
		t.Fatal("precondition: actual parent and detached child effects absent")
	}
	cause := wire.StateCauses[0]
	wrongOwner := changed(t, capture, func(w *captureWire) {
		for i := range w.Facts {
			if w.Facts[i].Key != cause.State {
				continue
			}
			if w.Facts[i].Run == nil {
				t.Fatal("effect owner absent")
			}
			for _, run := range w.Runs {
				if run.Raw != *w.Facts[i].Run {
					w.Facts[i].Run = ptr(run.Raw)
					return
				}
			}
		}
		t.Fatal("precondition: no distinct actual owner")
	})
	if _, err := decodeOrderProof(nativeProofJSON(t, wire), wrongOwner); !issueIs(err, InvalidObservation) {
		t.Fatalf("effect associated with a different actual run: %v", err)
	}
	// The same real snapshot is retained. Only the local input cuts are made
	// ambiguous around actual native terminals; projection must refuse them.
	r.mu.Lock()
	original := append([]observedState{}, r.states...)
	found := false
	for i := range r.states {
		if r.states[i].Raw == "" && r.states[i].Entity == "switch.nha045_gate" && r.states[i].State == "on" {
			r.states[i].ControlBefore = 0
			r.states[i].ControlAfter = len(native.decisions)
			found = true
		}
	}
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.states = original; r.mu.Unlock() }()
	if !found {
		t.Fatal("precondition: actual gate publication absent")
	}
	if _, err := r.captureNative(NetworkEvidence{}, native); !issueIs(err, InvalidObservation) {
		t.Fatalf("ambiguous external input cut was converted into order: %v", err)
	}
}

func TestNHA045NativeActionIssueRequiresExactOccurrence(t *testing.T) {
	r, native := joinedNativeOrderFixture(t, 9)
	r.mu.Lock()
	actions := append([]observedAction{}, r.actions...)
	r.mu.Unlock()
	if len(actions) != 2 {
		t.Fatal("precondition: real successful and failed branch calls absent")
	}
	for _, action := range actions {
		used := make(map[nativeCallKey]bool)
		if _, err := actionIssue(action, native.nodes, used); err != nil {
			t.Fatalf("positive native call issue: %v", err)
		}
		if _, err := actionIssue(action, native.nodes, used); !issueIs(err, InvalidObservation) {
			t.Fatalf("native command occurrence reused: %v", err)
		}
		cases := []struct {
			name   string
			change func(*observedAction)
		}{
			{"absent", func(a *observedAction) { a.origin = nativeActionOrigin{} }},
			{"zero-entry", func(a *observedAction) { a.origin.entry = automationruns.TraceOrdinal{} }},
			{"foreign-run", func(a *observedAction) { a.Raw = "another-run" }},
			{"foreign-source", func(a *observedAction) { a.Source = "another-source" }},
			{"foreign-branch", func(a *observedAction) { a.origin.branches = []int{31} }},
			{"foreign-iteration", func(a *observedAction) { a.origin.iterations = []int{31} }},
			{"contradictory-result", func(a *observedAction) { a.Failed = !a.Failed }},
		}
		for _, test := range cases {
			t.Run(action.Entity+"/"+test.name, func(t *testing.T) {
				invalid := action
				test.change(&invalid)
				if _, err := actionIssue(invalid, native.nodes, make(map[nativeCallKey]bool)); !issueIs(err, InvalidObservation) {
					t.Fatalf("inexact native action result admitted: %v", err)
				}
			})
		}
		if _, err := actionIssue(action, native.nodes, nil); !issueIs(err, InvalidObservation) {
			t.Fatalf("nil occurrence owner admitted: %v", err)
		}
	}
}
