package nha045

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/automationruns"
)

func nativeOrderValue(t *testing.T, ordinal automationruns.TraceOrdinal) uint64 {
	t.Helper()
	value, admitted := ordinal.Value()
	if !admitted {
		t.Fatal("precondition: no admitted native ordinal")
	}
	return value
}

func requireNativeOrderSnapshot(t *testing.T, observation nativeObservation, joined bool) {
	t.Helper()
	if !observation.valid || observation.controller == nil || observation.interpreter == nil || observation.state != automationruns.TraceOrderHealthy || observation.controller.TraceOrderState() != automationruns.TraceOrderHealthy || observation.decisionEvicted != 0 || observation.nodeEvicted != 0 || joined && !observation.joined {
		t.Fatal("precondition: native producer identity, health, loss or join invalid")
	}
	if len(observation.decisions) == 0 || len(observation.nodes) == 0 {
		t.Fatal("precondition: native observations absent")
	}
	seen := make(map[uint64]bool, len(observation.decisions)+len(observation.nodes))
	check := func(ordinal automationruns.TraceOrdinal, previous *uint64) {
		value := nativeOrderValue(t, ordinal)
		if seen[value] || value <= *previous {
			t.Fatalf("precondition: duplicate or nonmonotone native position %d after %d", value, *previous)
		}
		seen[value], *previous = true, value
	}
	var decisionPosition, nodePosition uint64
	for _, decision := range observation.decisions {
		check(decision.Ordinal(), &decisionPosition)
	}
	for _, node := range observation.nodes {
		check(node.Ordinal(), &nodePosition)
	}
}

// The real semantic terminals must already exist before cleanup. Only after
// joining the actual producers may the separate rings prove complete order.
func joinedNativeOrderFixture(t *testing.T, index int) (*rig, nativeObservation) {
	t.Helper()
	all, err := Corpus()
	if err != nil || len(all) != 10 {
		t.Fatalf("corpus precondition: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	r, err := buildRig(ctx, all[index])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.dispose(); err != nil {
			t.Error(err)
		}
	})
	for _, phase := range r.f.phases {
		if err := r.step(ctx, phase); err != nil {
			t.Fatalf("phase %s: %v", phase.id.value, err)
		}
	}
	if err := r.syncIdentities(); err != nil {
		t.Fatal(err)
	}
	for _, handle := range r.handles {
		if handle != nil && !channelJoined(handle.Done()) {
			t.Fatal("precondition: cleanup would cancel an unjoined script")
		}
	}
	before, err := r.observeNative()
	if err != nil {
		t.Fatal(err)
	}
	requireNativeOrderSnapshot(t, before, false)
	for _, identity := range r.identities {
		terminals := 0
		for _, decision := range before.decisions {
			if decision.RunID == identity.id && (decision.Decision == automationruns.DecisionFinished || decision.Decision == automationruns.DecisionCancelled || decision.Decision == automationruns.DecisionExecutionError) {
				terminals++
			}
		}
		if terminals != 1 {
			t.Fatalf("precondition: run %s has %d semantic terminals before cleanup", identity.key, terminals)
		}
	}
	elapsed := r.clock.MonoNow()
	if err := r.close(); err != nil {
		t.Fatal(err)
	}
	if !channelJoined(r.controller.Done()) || !channelJoined(r.triggers.Done()) || !channelJoined(r.reactive.Done()) || !channelJoined(r.session.Done()) || r.clock.MonoNow() != elapsed {
		t.Fatal("precondition: native producers not joined at the original fake tick")
	}
	after, err := r.observeNative()
	if err != nil {
		t.Fatal(err)
	}
	requireNativeOrderSnapshot(t, after, true)
	if after.controller != before.controller || after.interpreter != before.interpreter || len(after.decisions) < len(before.decisions) || len(after.nodes) < len(before.nodes) || !reflect.DeepEqual(before.decisions, after.decisions[:len(before.decisions)]) || !reflect.DeepEqual(before.nodes, after.nodes[:len(before.nodes)]) {
		t.Fatal("precondition: final native snapshots do not retain the observed prefixes")
	}
	// These fully terminal fixtures require no late cleanup observation at all.
	if len(after.decisions) != len(before.decisions) || len(after.nodes) != len(before.nodes) {
		t.Fatal("precondition: cleanup manufactured an observation after semantic completion")
	}
	for _, timer := range r.clock.records() {
		if !timer.Fired && !timer.Stopped {
			t.Fatal("precondition: joined fixture retained a live fake timer")
		}
	}
	return r, after
}

func requireOnlyNativePositionsChanged(t *testing.T, original, mutant nativeObservation) {
	t.Helper()
	if original.controller != mutant.controller || original.interpreter != mutant.interpreter || original.state != mutant.state || original.joined != mutant.joined || original.valid != mutant.valid || original.decisionEvicted != mutant.decisionEvicted || original.nodeEvicted != mutant.nodeEvicted || len(original.decisions) != len(mutant.decisions) || len(original.nodes) != len(mutant.nodes) {
		t.Fatal("mutation altered native observation ownership or completeness")
	}
	positions := make(map[uint64]int)
	for index, decision := range original.decisions {
		if !reflect.DeepEqual(decision.Trace, mutant.decisions[index].Trace) {
			t.Fatal("mutation altered a Controller payload or its ring sequence")
		}
		positions[nativeOrderValue(t, decision.Ordinal())]++
		positions[nativeOrderValue(t, mutant.decisions[index].Ordinal())]--
	}
	// The payload's immutable original native position is used only as a record
	// identity here. Ordering always reads the boundary record's Ordinal method.
	payloads := make(map[uint64]automationruns.NodeTrace, len(original.nodes))
	for _, node := range original.nodes {
		payloads[nativeOrderValue(t, node.NodeTrace.Ordinal())] = node.NodeTrace
		positions[nativeOrderValue(t, node.Ordinal())]++
	}
	for _, node := range mutant.nodes {
		identity := nativeOrderValue(t, node.NodeTrace.Ordinal())
		payload, present := payloads[identity]
		if !present || !reflect.DeepEqual(payload, node.NodeTrace) {
			t.Fatal("mutation altered or duplicated a native node payload")
		}
		delete(payloads, identity)
		positions[nativeOrderValue(t, node.Ordinal())]--
	}
	if len(payloads) != 0 {
		t.Fatal("mutation lost a native node payload")
	}
	for _, balance := range positions {
		if balance != 0 {
			t.Fatal("mutation forged or lost a native ordinal")
		}
	}
	requireNativeOrderSnapshot(t, mutant, true)
}

func projectedNativeOrder(t *testing.T, r *rig, observation nativeObservation) Capture {
	t.Helper()
	capture, err := r.captureNative(NetworkEvidence{}, observation)
	if err != nil {
		t.Fatalf("native boundary projection must remain admitted: %v", err)
	}
	if err := ValidateExpected(r.f, capture); err != nil {
		t.Fatalf("native boundary changed fixture semantics: %v", err)
	}
	return capture
}

// This metadata-only reference is synthetic. It exercises the real Compare
// authority with normal native Go facts/order; it claims no HA execution.
func syntheticNativeOrderReference(t *testing.T, normal Capture) Capture {
	t.Helper()
	return changed(t, normal, func(w *captureWire) {
		w.Runtime = runtimeWire{Name: "ha", Version: HAVersion, Python: ptr(HAPython), Jinja: ptr(HAJinja), SourceSHA256: append([]sourceWire(nil), haSources...)}
	})
}

func nativeOrderReachability(t *testing.T, capture Capture) map[string]map[string]bool {
	t.Helper()
	w, err := capture.boundary()
	if err != nil {
		t.Fatal(err)
	}
	facts := make(map[string]bool, len(w.Facts))
	for _, fact := range w.Facts {
		facts[fact.Key] = true
	}
	reach, err := reachability(w.Edges, facts)
	if err != nil {
		t.Fatal(err)
	}
	return reach
}

func requireSameNativeFacts(t *testing.T, normal, mutant Capture) {
	t.Helper()
	left, err := normal.boundary()
	if err != nil {
		t.Fatal(err)
	}
	right, err := mutant.boundary()
	if err != nil {
		t.Fatal(err)
	}
	rows := func(facts []factWire) map[string]string {
		result := make(map[string]string, len(facts))
		for _, fact := range facts {
			data, err := json.Marshal(fact)
			if err != nil {
				t.Fatal(err)
			}
			result[fact.Key] = string(data)
		}
		return result
	}
	if !reflect.DeepEqual(left.Runs, right.Runs) || !reflect.DeepEqual(rows(left.Facts), rows(right.Facts)) || left.FixtureSHA256 != right.FixtureSHA256 {
		t.Fatal("upstream order mutation changed a fact, tick, run binding or fixture")
	}
}

func requireNativeOrderDivergence(t *testing.T, normal, mutant Capture, before, after string, tick int64) {
	t.Helper()
	requireSameNativeFacts(t, normal, mutant)
	for _, capture := range []Capture{normal, mutant} {
		found := 0
		for _, fact := range capture.facts {
			if fact.key.value == before || fact.key.value == after {
				if fact.tick != tick {
					t.Fatal("precondition: compared order endpoints changed the shared fake tick")
				}
				found++
			}
		}
		if found != 2 {
			t.Fatal("precondition: both compared facts must exist exactly once")
		}
	}
	normalReach := nativeOrderReachability(t, normal)
	if !normalReach[before][after] || normalReach[after][before] {
		t.Fatal("precondition: normal projection lacks the native forward relation")
	}
	mutantReach := nativeOrderReachability(t, mutant)
	if !mutantReach[after][before] || mutantReach[before][after] {
		t.Errorf("native boundary inversion was overwritten: expected %s -> %s only", after, before)
	}
	result, err := Compare(syntheticNativeOrderReference(t, normal), mutant)
	if !issueIs(err, SemanticDivergence) || result.Equivalent || len(result.Differences) == 0 {
		t.Errorf("same-tick native inversion was hidden by projection: %+v %v", result, err)
	}
	for _, difference := range result.Differences {
		if difference.Field != "order" {
			t.Errorf("native order mutation also changed %s: %+v", difference.Field, difference)
		}
	}
}

func TestNHA045NativeFinalWaitInversionBeforeProjection(t *testing.T) {
	r, original := joinedNativeOrderFixture(t, 5)
	handle := r.handles["a"]
	waitIndex, terminalIndex := -1, -1
	for index, node := range original.nodes {
		if node.RunID() == handle.ID() && node.WaitExit() == automationruns.WaitStoppedByTimeout {
			if waitIndex != -1 {
				t.Fatal("precondition: ambiguous final wait")
			}
			waitIndex = index
			if node.Generation() == 0 || node.Source().Object() != r.f.wire.Root || node.At().Sub(r.f.origin) != 5*time.Second {
				t.Fatal("precondition: wrong final-wait source, generation or tick")
			}
			if _, present, err := node.WaitValue(); err != nil || !present {
				t.Fatal("precondition: final native wait value absent")
			}
		}
	}
	for index, decision := range original.decisions {
		if decision.RunID == handle.ID() && decision.Decision == automationruns.DecisionFinished {
			if terminalIndex != -1 {
				t.Fatal("precondition: ambiguous native terminal")
			}
			terminalIndex = index
		}
	}
	if waitIndex < 0 || terminalIndex < 0 || original.nodes[waitIndex].Generation() != original.decisions[terminalIndex].Generation {
		t.Fatal("precondition: exact final-wait/terminal pair absent")
	}
	terminalPosition := nativeOrderValue(t, original.decisions[terminalIndex].Ordinal())
	if nativeOrderValue(t, original.nodes[waitIndex].Ordinal()) >= terminalPosition {
		t.Fatal("precondition: real final wait did not precede its terminal")
	}
	mutant := original.copy()
	// Rotate the suffix [final wait, any following native nodes, terminal].
	// Neither ring's payload sequence changes, even if a later node is emitted.
	mutant.decisions[terminalIndex].position = original.nodes[waitIndex].Ordinal()
	for index := waitIndex; index < len(original.nodes); index++ {
		if nativeOrderValue(t, original.nodes[index].Ordinal()) >= terminalPosition {
			t.Fatal("precondition: suffix contains observations after the terminal")
		}
		if index+1 < len(original.nodes) {
			mutant.nodes[index].position = original.nodes[index+1].Ordinal()
		} else {
			mutant.nodes[index].position = original.decisions[terminalIndex].Ordinal()
		}
	}
	requireOnlyNativePositionsChanged(t, original, mutant)
	for index, node := range original.nodes {
		if !reflect.DeepEqual(node.NodeTrace, mutant.nodes[index].NodeTrace) {
			t.Fatal("mutation changed the native node payload sequence")
		}
	}
	if nativeOrderValue(t, mutant.decisions[terminalIndex].Ordinal()) >= nativeOrderValue(t, mutant.nodes[waitIndex].Ordinal()) {
		t.Fatal("precondition: mutation did not put terminal before final wait")
	}
	requireNativeOrderDivergence(t, projectedNativeOrder(t, r, original), projectedNativeOrder(t, r, mutant), "a/wait/1", "a/terminal", 5000)
}

func TestNHA045NativeStartInversionBeforeProjection(t *testing.T) {
	r, original := joinedNativeOrderFixture(t, 9)
	handle := r.handles["a"]
	startIndex, actionIndex := -1, -1
	for index, decision := range original.decisions {
		if decision.RunID == handle.ID() && decision.Decision == automationruns.DecisionAdmitted {
			if startIndex != -1 {
				t.Fatal("precondition: ambiguous native start")
			}
			startIndex = index
		}
	}
	r.mu.Lock()
	actions := append([]observedAction(nil), r.actions...)
	r.mu.Unlock()
	for _, action := range actions {
		if action.Entity != "switch.nha045_fail" || !action.Failed || action.AtMS != 0 {
			continue
		}
		for index, node := range original.nodes {
			if node.RunID() == handle.ID() && node.Source().Path() == action.Source && node.Kind() == ir.ActionCall && node.Stage() == automationruns.NodeFailed && node.Failure() == automationruns.ExecutionCommandError {
				if actionIndex != -1 {
					t.Fatal("precondition: ambiguous native action issue")
				}
				actionIndex = index
			}
		}
	}
	if startIndex < 0 || actionIndex < 0 || original.nodes[actionIndex].Generation() != original.decisions[startIndex].Generation || !original.nodes[actionIndex].At().Equal(r.f.origin) {
		t.Fatal("precondition: same-tick admitted start and real action result absent")
	}
	startPosition := nativeOrderValue(t, original.decisions[startIndex].Ordinal())
	actionPosition := nativeOrderValue(t, original.nodes[actionIndex].Ordinal())
	if startPosition >= actionPosition {
		t.Fatal("precondition: real action result did not follow start")
	}
	mutant := original.copy()
	// Rotate [Admitted, native node prefix through the actual Call result].
	// Preserve every node and intra-branch sequence; never sort a native ring.
	previous := original.decisions[startIndex].Ordinal()
	for index := 0; index <= actionIndex; index++ {
		if nativeOrderValue(t, original.nodes[index].Ordinal()) <= startPosition {
			t.Fatal("precondition: another run precedes this native start")
		}
		mutant.nodes[index].position, previous = previous, original.nodes[index].Ordinal()
	}
	for index, decision := range original.decisions {
		position := nativeOrderValue(t, decision.Ordinal())
		if index != startIndex && position > startPosition && position <= actionPosition {
			t.Fatal("precondition: another Controller event interrupts the rotated block")
		}
	}
	mutant.decisions[startIndex].position = previous
	requireOnlyNativePositionsChanged(t, original, mutant)
	for index, node := range original.nodes {
		if !reflect.DeepEqual(node.NodeTrace, mutant.nodes[index].NodeTrace) {
			t.Fatal("mutation changed the native node sequence")
		}
	}
	if nativeOrderValue(t, mutant.nodes[actionIndex].Ordinal()) >= nativeOrderValue(t, mutant.decisions[startIndex].Ordinal()) {
		t.Fatal("precondition: mutation did not put the real action result before start")
	}
	requireNativeOrderDivergence(t, projectedNativeOrder(t, r, original), projectedNativeOrder(t, r, mutant), "a/start", "a/action/switch.nha045_fail/1", 0)
}

func TestNHA045NativeIndependentBranchesBeforeProjection(t *testing.T) {
	r, original := joinedNativeOrderFixture(t, 9)
	slots := []int{}
	branches := [2][]int{}
	joinedPosition, terminalPosition := uint64(0), uint64(0)
	for index, node := range original.nodes {
		path := node.Branches()
		if len(path) == 1 {
			if path[0] < 0 || path[0] > 1 || !node.At().Equal(r.f.origin) {
				t.Fatal("precondition: unexpected parallel branch or tick")
			}
			slots = append(slots, index)
			branches[path[0]] = append(branches[path[0]], index)
		}
		if node.Kind() == ir.ActionParallel && node.Stage() == automationruns.NodeFailed {
			if joinedPosition != 0 {
				t.Fatal("precondition: ambiguous parallel join")
			}
			joinedPosition = nativeOrderValue(t, node.Ordinal())
		}
	}
	for _, decision := range original.decisions {
		if decision.RunID == r.handles["a"].ID() && decision.Decision == automationruns.DecisionExecutionError {
			if terminalPosition != 0 {
				t.Fatal("precondition: ambiguous parallel terminal")
			}
			terminalPosition = nativeOrderValue(t, decision.Ordinal())
		}
	}
	if len(branches[0]) == 0 || len(branches[1]) == 0 || joinedPosition == 0 || terminalPosition <= joinedPosition {
		t.Fatal("precondition: two real joined branches and terminal absent")
	}
	for _, index := range slots {
		if nativeOrderValue(t, original.nodes[index].Ordinal()) >= joinedPosition {
			t.Fatal("precondition: branch emission followed the native parallel join")
		}
	}
	first := original.nodes[slots[0]].Branches()[0]
	order := append(append([]int(nil), branches[1-first]...), branches[first]...)
	mutant := original.copy()
	for offset, slot := range slots {
		mutant.nodes[slot] = original.nodes[order[offset]]
		mutant.nodes[slot].position = original.nodes[slot].Ordinal()
	}
	requireOnlyNativePositionsChanged(t, original, mutant)
	for branch := range branches {
		var before, after []automationruns.NodeTrace
		for _, node := range original.nodes {
			if path := node.Branches(); len(path) == 1 && path[0] == branch {
				before = append(before, node.NodeTrace)
			}
		}
		for _, node := range mutant.nodes {
			if path := node.Branches(); len(path) == 1 && path[0] == branch {
				after = append(after, node.NodeTrace)
			}
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatal("mutation changed an intra-branch payload sequence")
		}
	}
	if mutant.nodes[slots[0]].Branches()[0] == first {
		t.Fatal("precondition: branch interleaving did not change")
	}
	normal, reordered := projectedNativeOrder(t, r, original), projectedNativeOrder(t, r, mutant)
	requireSameNativeFacts(t, normal, reordered)
	for _, capture := range []Capture{normal, reordered} {
		reach := nativeOrderReachability(t, capture)
		left, right := "a/action/switch.nha045_fail/1", "a/action/switch.nha045_other/1"
		if reach[left][right] || reach[right][left] {
			t.Error("independent sibling actions acquired false causality")
		}
		if !reach[left]["a/terminal"] || !reach[right]["a/terminal"] || !reach["a/wait/1"][right] {
			t.Error("native branch sequence or terminal join disappeared")
		}
	}
	result, err := Compare(syntheticNativeOrderReference(t, normal), reordered)
	if err != nil || !result.Equivalent {
		t.Fatalf("independent native interleaving diverged: %+v %v", result, err)
	}
}
