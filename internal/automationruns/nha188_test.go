package automationruns

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/integrations"
)

// These tests reuse the real admitted IR, interpreter, Controller and joined
// fake services from 041/042. No observer of a different wait is an order oracle.
type nha188Observation struct {
	controller *Controller
	decisions  []Trace
	nodes      []NodeTrace
}

// Each final-wait fixture deliberately contains exactly this one admitted
// action. Script.Waits lists trigger registrations, not template-wait nodes.
func nha188OnlyActionSource(t *testing.T, program ir.Script, expected ir.ActionKind) ir.Source {
	t.Helper()
	actions := program.Actions()
	if len(actions) != 1 || actions[0].Kind() != expected {
		t.Fatalf("final-wait fixture must contain exactly one action of kind %v", expected)
	}
	return actions[0].Source()
}

func nha188Value(t *testing.T, ordinal TraceOrdinal) uint64 {
	t.Helper()
	value, admitted := ordinal.Value()
	if !admitted || value == 0 || value == exhaustedTraceOrdinal {
		t.Fatalf("native observation has no admitted ordinal: %d admitted=%v", value, admitted)
	}
	return value
}

func nha188Snapshot(t *testing.T, c *Controller, i *Interpreter) nha188Observation {
	t.Helper()
	decisions, decisionLoss := c.Traces()
	nodes, nodeLoss := i.NodeTraces()
	if c.TraceOrderState() != TraceOrderHealthy || decisionLoss != 0 || nodeLoss != 0 {
		t.Fatalf("incomplete observation: state=%v decision loss=%d node loss=%d", c.TraceOrderState(), decisionLoss, nodeLoss)
	}
	seen := make(map[uint64]bool, len(decisions)+len(nodes))
	check := func(ordinal TraceOrdinal, previous *uint64) {
		value := nha188Value(t, ordinal)
		if seen[value] || value <= *previous {
			t.Fatalf("ordinal reused or ring reordered: previous=%d value=%d", *previous, value)
		}
		seen[value], *previous = true, value
	}
	var lastDecision, lastNode uint64
	for _, trace := range decisions {
		check(trace.Ordinal(), &lastDecision)
	}
	for _, trace := range nodes {
		check(trace.Ordinal(), &lastNode)
	}
	return nha188Observation{c, decisions, nodes}
}

func nha188Close(t *testing.T, c *Controller) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("Close returned before the real Controller join")
	}
}

// Observation owns an ordinary image reference until the post-Close native
// assertions finish. The actual ring stays charged until its final release.
func nha188RetainImageForObservation(t *testing.T, c *Controller, i *Interpreter) func() {
	t.Helper()
	c.mu.Lock()
	image := c.active
	if image == nil || image.interpreter != i || !c.retainImageLocked(image) {
		c.mu.Unlock()
		t.Fatal("observation could not retain the actual active image")
	}
	c.mu.Unlock()
	var once sync.Once
	release := func() { once.Do(func() { c.releaseImage(image) }) }
	// Registered after the rig: a failed assertion drops this reference before
	// the rig's existing shutdown joins its own active/run references.
	t.Cleanup(release)
	return func() {
		t.Helper()
		select {
		case <-c.Done():
		default:
			t.Fatal("observation release preceded the real Controller join")
		}
		c.mu.Lock()
		lastOwner := c.closed && c.active == nil && c.pending == nil && c.live == 0 && image.refs == 1
		charge := image.charge
		c.mu.Unlock()
		i.mu.Lock()
		retained := i.controller == c && i.image == image && i.charge > 0 && cap(i.traces) == nodeTraceCapacity
		i.mu.Unlock()
		c.pool.mu.Lock()
		before := c.pool.used
		c.pool.mu.Unlock()
		if !lastOwner || !retained || before != controllerCharge+charge {
			t.Fatalf("observation did not retain a real charged image: last=%v retained=%v pool=%d image=%d", lastOwner, retained, before, charge)
		}
		decisions, decisionLoss := c.Traces()
		nodes, nodeLoss := i.NodeTraces()
		nodeCopy := append([]NodeTrace(nil), nodes...)
		previous, order := c.traceOrdinal.Load(), c.TraceOrderState()
		release()
		afterDecisions, afterDecisionLoss := c.Traces()
		afterNodes, afterNodeLoss := i.NodeTraces()
		c.mu.Lock()
		noOwners := image.refs == 0
		c.mu.Unlock()
		i.mu.Lock()
		released := i.controller == c && i.image == nil && i.waitPlan == nil && i.charge == 0 && cap(i.traces) == 0 && i.traceNext == 0
		i.mu.Unlock()
		c.pool.mu.Lock()
		after := c.pool.used
		c.pool.mu.Unlock()
		if !noOwners || !released || after != controllerCharge || before-after != charge {
			t.Fatalf("last observation reference retained memory: refs=%v released=%v before=%d image=%d after=%d", noOwners, released, before, charge, after)
		}
		if c.traceOrdinal.Load() != previous || c.TraceOrderState() != order || !reflect.DeepEqual(decisions, afterDecisions) || afterDecisionLoss != decisionLoss || len(afterNodes) != 0 || afterNodeLoss != nodeLoss || !reflect.DeepEqual(nodes, nodeCopy) {
			t.Fatal("image release changed native observations, ordering, or loss")
		}
	}
}

func nha188Invoke(t *testing.T, c *Controller, id ir.ScriptID) *ScriptHandle {
	t.Helper()
	handle, err := c.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDirect)
	if err != nil {
		t.Fatal(err)
	}
	return handle
}

func nha188Suspended(t *testing.T, handle *ScriptHandle) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	startup, err := handle.Startup(ctx)
	if err != nil || startup.Kind() != StartupSuspended {
		t.Fatalf("real wait was not suspended: %v %v", startup.Kind(), err)
	}
}

func (o nha188Observation) terminal(t *testing.T, id RunID, decision Decision) Trace {
	t.Helper()
	var result Trace
	found := false
	for _, trace := range o.decisions {
		if trace.RunID != id || trace.Decision != decision {
			continue
		}
		if found {
			t.Fatal("ambiguous run terminal")
		}
		result, found = trace, true
	}
	if !found {
		t.Fatalf("no joined terminal for run=%d decision=%v", id, decision)
	}
	return result
}

func (o nha188Observation) finalWait(t *testing.T, id RunID, source ir.Source, exit WaitExit, stage NodeStage, terminal Decision) (NodeTrace, NodeTrace, Trace) {
	t.Helper()
	var waiting, ended NodeTrace
	waitingCount, exitCount := 0, 0
	for _, trace := range o.nodes {
		if trace.RunID() != id || trace.Source().Object() != source.Object() || trace.Source().Path() != source.Path() || trace.Source().At() != source.At() {
			continue
		}
		if trace.Stage() == NodeWaiting {
			waiting, waitingCount = trace, waitingCount+1
		}
		if trace.WaitExit() == exit {
			ended, exitCount = trace, exitCount+1
		}
	}
	if waitingCount != 1 || exitCount != 1 || ended.Stage() != stage {
		t.Fatalf("wrong exact wait observations: waiting=%d exits=%d exit stage=%v", waitingCount, exitCount, ended.Stage())
	}
	_, present, err := ended.WaitValue()
	if err != nil || !present {
		t.Fatalf("terminal wait value absent: %v %v", present, err)
	}
	_, present, err = waiting.WaitValue()
	if err != nil || present || waiting.WaitExit() != WaitExitInvalid {
		t.Fatal("waiting observation invented a terminal value")
	}
	finished := o.terminal(t, id, terminal)
	if waiting.Generation() == 0 || waiting.Generation() != ended.Generation() || ended.Generation() != finished.Generation {
		t.Fatal("wait/run terminal lost their causal generation provenance")
	}
	if !(nha188Value(t, waiting.Ordinal()) < nha188Value(t, ended.Ordinal()) && nha188Value(t, ended.Ordinal()) < nha188Value(t, finished.Ordinal())) {
		t.Fatal("native final-wait emission did not precede its run terminal")
	}
	return waiting, ended, finished
}

func TestNHA188FinalWaitTimeoutEmissionPrecedesTerminal(t *testing.T) {
	for _, test := range []struct {
		name, wait        string
		continueOnTimeout bool
		kind              ir.ActionKind
	}{
		{"trigger_stopped", "- wait_for_trigger:\n  - trigger: event\n    event_type: nha042_release\n", false, ir.ActionWait},
		{"trigger_continued", "- wait_for_trigger:\n  - trigger: event\n    event_type: nha042_release\n", true, ir.ActionWait},
		{"template_stopped", "- wait_template: '{{ false }}'\n", false, ir.ActionWaitTemplate},
		{"template_continued", "- wait_template: '{{ false }}'\n", true, ir.ActionWaitTemplate},
	} {
		t.Run(test.name, func(t *testing.T) {
			id, _ := ir.ParseScriptID("script.nha188_timeout")
			continuation := "false"
			if test.continueOnTimeout {
				continuation = "true"
			}
			program := nha042Compile(t, "script.nha188_timeout", "sequence:\n"+test.wait+"  timeout: {seconds: 60}\n  continue_on_timeout: "+continuation+"\n", []ir.ScriptID{id})
			rig := nha042FullRig(t, []ir.Script{program}, &nha042Commands{})
			releaseObservation := nha188RetainImageForObservation(t, rig.controller, rig.interpreter)
			handle := nha188Invoke(t, rig.controller, id)
			nha188Suspended(t, handle)
			rig.clock.advance(60 * time.Second)
			if err := nha042Joined(t, handle); err != nil {
				t.Fatal(err)
			}
			// The run has already reached its semantic terminal before cleanup.
			beforeClose := nha188Snapshot(t, rig.controller, rig.interpreter)
			exit, stage := WaitStoppedByTimeout, NodeStopped
			if test.continueOnTimeout {
				exit, stage = WaitContinuedAfterTimeout, NodeCompleted
			}
			_, ended, _ := beforeClose.finalWait(t, handle.ID(), nha188OnlyActionSource(t, program, test.kind), exit, stage, DecisionFinished)
			if !ended.At().Equal(rig.clock.Now()) || len(rig.effects()) != 0 {
				t.Fatal("final wait changed fake tick or executed an extra action")
			}
			nha188Close(t, rig.controller)
			afterClose := nha188Snapshot(t, rig.controller, rig.interpreter)
			if !reflect.DeepEqual(beforeClose, afterClose) {
				t.Fatal("cleanup manufactured an observation missing at the semantic terminal")
			}
			rig.clock.mu.Lock()
			for _, timer := range rig.clock.timers {
				if !timer.stopped.Load() {
					rig.clock.mu.Unlock()
					t.Fatal("joined wait retained a timer")
				}
			}
			rig.clock.mu.Unlock()
			releaseObservation()
		})
	}
}

func TestNHA188ParentChildFinalWaitCancellationSameTick(t *testing.T) {
	for _, directChild := range []bool{false, true} {
		name := "single_wait"
		if directChild {
			name = "direct_child"
		}
		t.Run(name, func(t *testing.T) {
			parentID, _ := ir.ParseScriptID("script.nha188_parent")
			childID, _ := ir.ParseScriptID("script.nha188_child")
			ids := []ir.ScriptID{parentID, childID}
			parentBody := "sequence:\n- wait_template: '{{ false }}'\n"
			child := nha042Compile(t, "script.nha188_child", "sequence:\n- wait_template: '{{ false }}'\n", ids)
			programs := []ir.Script{child}
			if directChild {
				parentBody = "sequence:\n- action: script.nha188_child\n"
			}
			parent := nha042Compile(t, "script.nha188_parent", parentBody, ids)
			programs = append(programs, parent)
			rig := nha042FullRig(t, programs, &nha042Commands{})
			releaseObservation := nha188RetainImageForObservation(t, rig.controller, rig.interpreter)
			handle := nha188Invoke(t, rig.controller, parentID)
			nha188Suspended(t, handle)
			waitHandle, waitProgram := handle, parent
			if directChild {
				waitHandle, waitProgram = rig.activeHandle(childID), child
				if waitHandle == nil {
					t.Fatal("parent suspension did not correspond to its real child")
				}
			}
			<-handle.Cancel()
			if err := nha042Joined(t, handle); err != nil && (!directChild || !errors.Is(err, context.Canceled)) {
				t.Fatalf("parent cancellation result changed: %v", err)
			}
			if err := nha042Joined(t, waitHandle); err != nil {
				t.Fatalf("final cancelled wait no longer returns its existing stop result: %v", err)
			}
			beforeClose := nha188Snapshot(t, rig.controller, rig.interpreter)
			waiting, ended, childTerminal := beforeClose.finalWait(t, waitHandle.ID(), nha188OnlyActionSource(t, waitProgram, ir.ActionWaitTemplate), WaitCancelled, NodeCancelled, DecisionCancelled)
			if !waiting.At().Equal(ended.At()) || !ended.At().Equal(rig.clock.Now()) {
				t.Fatal("same-tick cancellation test accidentally used time as order")
			}
			if directChild {
				parentTerminal := beforeClose.terminal(t, handle.ID(), DecisionCancelled)
				if nha188Value(t, childTerminal.Ordinal()) >= nha188Value(t, parentTerminal.Ordinal()) {
					t.Fatal("parent terminal preceded the joined child terminal")
				}
				returned := false
				for _, trace := range beforeClose.nodes {
					if trace.RunID() == handle.ID() && trace.Kind() == ir.ActionCall && trace.Stage() == NodeCancelled {
						returned = nha188Value(t, childTerminal.Ordinal()) < nha188Value(t, trace.Ordinal()) && nha188Value(t, trace.Ordinal()) < nha188Value(t, parentTerminal.Ordinal())
					}
				}
				if !returned {
					t.Fatal("direct-call return did not retain child join order")
				}
			}
			nha188Close(t, rig.controller)
			if !reflect.DeepEqual(beforeClose, nha188Snapshot(t, rig.controller, rig.interpreter)) || len(rig.effects()) != 0 {
				t.Fatal("cancellation cleanup added late observations or actions")
			}
			releaseObservation()
		})
	}
}

func TestNHA188RealRunInterleavingAndParallelJoin(t *testing.T) {
	waitID, _ := ir.ParseScriptID("script.nha188_waiting")
	parallelID, _ := ir.ParseScriptID("script.nha188_parallel")
	ids := []ir.ScriptID{waitID, parallelID}
	waiter := nha042Compile(t, "script.nha188_waiting", "sequence:\n- wait_template: '{{ false }}'\n", ids)
	parallel := nha042Compile(t, "script.nha188_parallel", "sequence:\n- parallel:\n  - sequence:\n    - variables: {left: 1}\n  - sequence:\n    - variables: {right: 2}\n", ids)
	rig := nha042FullRig(t, []ir.Script{waiter, parallel}, &nha042Commands{})
	releaseObservation := nha188RetainImageForObservation(t, rig.controller, rig.interpreter)
	first := nha188Invoke(t, rig.controller, waitID)
	nha188Suspended(t, first)
	second := nha188Invoke(t, rig.controller, parallelID)
	if err := nha042Joined(t, second); err != nil {
		t.Fatal(err)
	}
	live := nha188Snapshot(t, rig.controller, rig.interpreter)
	secondTerminal := live.terminal(t, second.ID(), DecisionFinished)
	// Positive live observations are valid; no cross-ring absence is inferred.
	<-first.Cancel()
	if err := nha042Joined(t, first); err != nil {
		t.Fatal(err)
	}
	nha188Close(t, rig.controller)
	observation := nha188Snapshot(t, rig.controller, rig.interpreter)
	waiting, ended, _ := observation.finalWait(t, first.ID(), nha188OnlyActionSource(t, waiter, ir.ActionWaitTemplate), WaitCancelled, NodeCancelled, DecisionCancelled)
	if !(nha188Value(t, waiting.Ordinal()) < nha188Value(t, secondTerminal.Ordinal()) && nha188Value(t, secondTerminal.Ordinal()) < nha188Value(t, ended.Ordinal())) {
		t.Fatal("deterministic real interleaving was not retained between rings")
	}
	var joined NodeTrace
	branches := make(map[int]NodeTrace)
	for _, trace := range observation.nodes {
		if trace.RunID() != second.ID() || trace.Stage() != NodeCompleted {
			continue
		}
		if trace.Kind() == ir.ActionParallel {
			joined = trace
		}
		if trace.Kind() == ir.ActionVariables {
			path := trace.Branches()
			if len(path) != 1 {
				t.Fatal("parallel leaf lost branch identity")
			}
			branches[path[0]] = trace
			path[0] = 99
			if trace.Branches()[0] == 99 {
				t.Fatal("branch snapshot exposed mutable path storage")
			}
		}
	}
	if len(branches) != 2 {
		t.Fatalf("joined parallel lost a branch: %d", len(branches))
	}
	for _, branch := range branches {
		if !(nha188Value(t, branch.Ordinal()) < nha188Value(t, joined.Ordinal()) && nha188Value(t, joined.Ordinal()) < nha188Value(t, secondTerminal.Ordinal())) {
			t.Fatal("branch completion did not precede parallel join and terminal")
		}
	}
	// No sibling-to-sibling relation is asserted: their scheduling is independent.
	releaseObservation()
}

// Direction is read only from native observations and their Controller scope.
// There is deliberately no expected graph or expected final-wait edge here.
type nha188Direction uint8

const (
	nha188OrderInvalid nha188Direction = iota
	nha188Before
	nha188After
)

func nha188ObservedDirection(left, right nha188Observation, a, b TraceOrdinal) nha188Direction {
	if left.controller == nil || left.controller != right.controller || left.controller.TraceOrderState() != TraceOrderHealthy {
		return nha188OrderInvalid
	}
	av, aok := a.Value()
	bv, bok := b.Value()
	if !aok || !bok || av == bv {
		return nha188OrderInvalid
	}
	if av < bv {
		return nha188Before
	}
	return nha188After
}

func TestNHA188SyntheticInversionPreservesNativeDirection(t *testing.T) {
	id, _ := ir.ParseScriptID("script.nha188_negative")
	program := nha042Compile(t, "script.nha188_negative", "sequence:\n- wait_template: '{{ false }}'\n", []ir.ScriptID{id})
	rig := nha042FullRig(t, []ir.Script{program}, &nha042Commands{})
	releaseObservation := nha188RetainImageForObservation(t, rig.controller, rig.interpreter)
	handle := nha188Invoke(t, rig.controller, id)
	nha188Suspended(t, handle)
	<-handle.Cancel()
	if err := nha042Joined(t, handle); err != nil {
		t.Fatal(err)
	}
	nha188Close(t, rig.controller)
	observed := nha188Snapshot(t, rig.controller, rig.interpreter)
	_, exit, terminal := observed.finalWait(t, handle.ID(), nha188OnlyActionSource(t, program, ir.ActionWaitTemplate), WaitCancelled, NodeCancelled, DecisionCancelled)
	if nha188ObservedDirection(observed, observed, exit.Ordinal(), terminal.Ordinal()) != nha188Before {
		t.Fatal("actual emitted order was not read from native ordinals")
	}
	// Synthetic upstream observations retain the exact facts, tick and streams.
	// Only two existing admitted ordinal values exchange their observed order.
	reversedExit, reversedTerminal := exit, terminal
	reversedExit.ordinal, reversedTerminal.ordinal = terminal.Ordinal(), exit.Ordinal()
	if !reversedExit.At().Equal(exit.At()) || reversedExit.RunID() != exit.RunID() || reversedExit.WaitExit() != exit.WaitExit() || reversedTerminal.Decision != terminal.Decision {
		t.Fatal("negative fixture altered semantic facts")
	}
	if nha188ObservedDirection(observed, observed, reversedExit.Ordinal(), reversedTerminal.Ordinal()) != nha188After {
		t.Fatal("same-tick terminal-before-exit was hidden by an inferred expected edge")
	}
	if nha188ObservedDirection(observed, observed, TraceOrdinal{}, terminal.Ordinal()) != nha188OrderInvalid || nha188ObservedDirection(observed, observed, exit.Ordinal(), exit.Ordinal()) != nha188OrderInvalid {
		t.Fatal("absent or equal ordinals manufactured an ordering proof")
	}
	releaseObservation()
}

func TestNHA188OrdinalAdmissionStatesAndSaturatingCAS(t *testing.T) {
	var absent *Controller
	var typedNil interface{ TraceOrderState() TraceOrderState } = absent
	for _, state := range []TraceOrderState{typedNil.TraceOrderState(), (&Controller{}).TraceOrderState()} {
		if state != TraceOrderInvalid {
			t.Fatal("nil/zero Controller was admitted as an ordering authority")
		}
	}
	for _, ordinal := range []TraceOrdinal{{}, absent.nextTraceOrdinal(), (&Controller{}).nextTraceOrdinal()} {
		if value, valid := ordinal.Value(); valid || value != 0 {
			t.Fatal("absent ordinal became admitted")
		}
	}
	if value, valid := (Trace{}).Ordinal().Value(); valid || value != 0 {
		t.Fatal("zero decision trace has an ordinal")
	}
	if value, valid := (NodeTrace{}).Ordinal().Value(); valid || value != 0 {
		t.Fatal("zero node trace has an ordinal")
	}
	typ := reflect.TypeOf(TraceOrdinal{})
	if typ.NumField() != 1 || typ.Field(0).PkgPath == "" || typ.Field(0).Type.Kind() != reflect.Uint64 || unsafe.Sizeof(TraceOrdinal{}) != unsafe.Sizeof(uint64(0)) {
		t.Fatal("ordinal admission/storage is no longer a single private word")
	}
	c := &Controller{done: make(chan struct{})}
	if c.TraceOrderState() != TraceOrderHealthy {
		t.Fatal("admitted empty Controller is not healthy")
	}
	// This private injection is deliberately confined to the package's test.
	c.traceOrdinal.Store(exhaustedTraceOrdinal - 2)
	if value := nha188Value(t, c.nextTraceOrdinal()); value != exhaustedTraceOrdinal-1 || c.TraceOrderState() != TraceOrderHealthy {
		t.Fatalf("last valid emission lost: %d state=%v", value, c.TraceOrderState())
	}
	for count := 0; count < 3; count++ {
		if value, valid := c.nextTraceOrdinal().Value(); valid || value != 0 || c.TraceOrderState() != TraceOrderExhausted || c.traceOrdinal.Load() != exhaustedTraceOrdinal {
			t.Fatalf("saturation rotated or reused ordinal: %d %v state=%v", value, valid, c.TraceOrderState())
		}
	}
	// Every returned state has a named meaning; no catch-all conversion hides it.
	for _, authority := range []*Controller{nil, {done: make(chan struct{})}, c} {
		switch authority.TraceOrderState() {
		case TraceOrderInvalid:
			if authority != nil {
				t.Fatal("unexpected invalid state")
			}
		case TraceOrderHealthy:
			if authority.traceOrdinal.Load() == exhaustedTraceOrdinal {
				t.Fatal("exhausted Controller reported healthy")
			}
		case TraceOrderExhausted:
			if authority != c {
				t.Fatal("wrong exhausted authority")
			}
		default:
			t.Fatal("new trace-order state needs an explicit test/consumer decision")
		}
	}
	allocationAuthority := &Controller{done: make(chan struct{})}
	if allocations := testing.AllocsPerRun(100, func() { allocationAuthority.nextTraceOrdinal() }); allocations != 0 {
		t.Fatalf("ordinal emission allocated: %f", allocations)
	}
}

func TestNHA188ConcurrentNativeRingsUseOneOrdinalSpace(t *testing.T) {
	id, _ := ir.ParseScriptID("script.nha188_contention")
	program := nha042Compile(t, "script.nha188_contention", "sequence:\n- stop: done\n", []ir.ScriptID{id})
	controller, interpreter := nha042Runtime(t, []ir.Script{program}, &nha042Commands{})
	releaseObservation := nha188RetainImageForObservation(t, controller, interpreter)
	// Both producers use their real bounded emitters and existing ring locks.
	// A start barrier creates contention without sleep or per-emission hooks.
	start := make(chan struct{})
	var joined sync.WaitGroup
	joined.Add(3)
	const emissions = 200
	nodeGeneration := controller.generation
	go func() {
		defer joined.Done()
		<-start
		for n := 0; n < emissions; n++ {
			controller.mu.Lock()
			controller.recordLocked(Trace{Decision: DecisionTriggerMissed, Generation: controller.generation})
			controller.mu.Unlock()
		}
	}()
	go func() {
		defer joined.Done()
		<-start
		for n := 0; n < emissions; n++ {
			interpreter.appendNodeTrace(NodeTrace{generation: nodeGeneration, stage: NodeEntered})
		}
	}()
	// Publishing a causal image can change provenance under c.mu while the
	// immutable Controller owner and its ordering admission remain unchanged.
	go func() {
		defer joined.Done()
		<-start
		for n := 0; n < emissions; n++ {
			controller.mu.Lock()
			controller.generation++
			controller.mu.Unlock()
			if controller.TraceOrderState() != TraceOrderHealthy {
				t.Error("program generation publication invalidated Controller order")
			}
		}
	}()
	close(start)
	joined.Wait()
	nha188Close(t, controller)
	observation := nha188Snapshot(t, controller, interpreter)
	if len(observation.decisions) != emissions || len(observation.nodes) != emissions || controller.traceOrdinal.Load() != 2*emissions {
		t.Fatal("native rings did not share one allocation space")
	}
	releaseObservation()
}

func TestNHA188ExhaustionDoesNotChangeFinalWaitResultOrCleanup(t *testing.T) {
	for _, cancelRun := range []bool{false, true} {
		name := "normal_timeout"
		if cancelRun {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			id, _ := ir.ParseScriptID("script.nha188_exhaustion")
			program := nha042Compile(t, "script.nha188_exhaustion", "sequence:\n- wait_template: '{{ false }}'\n  timeout: {seconds: 60}\n  continue_on_timeout: false\n", []ir.ScriptID{id})
			rig := nha042FullRig(t, []ir.Script{program}, &nha042Commands{})
			releaseObservation := nha188RetainImageForObservation(t, rig.controller, rig.interpreter)
			rig.controller.pool.mu.Lock()
			baseline := rig.controller.pool.used
			rig.controller.pool.mu.Unlock()
			handle := nha188Invoke(t, rig.controller, id)
			nha188Suspended(t, handle)
			// The suspended run has emitted its real NodeWaiting. Next emission
			// receives the last valid position; its terminal exhausts the space.
			rig.controller.traceOrdinal.Store(exhaustedTraceOrdinal - 2)
			if cancelRun {
				<-handle.Cancel()
			} else {
				rig.clock.advance(60 * time.Second)
			}
			result := nha042Joined(t, handle)
			if result != nil {
				t.Fatalf("trace exhaustion changed business result: %v", result)
			}
			nha188Close(t, rig.controller)
			decisions, decisionLoss := rig.controller.Traces()
			nodes, nodeLoss := rig.interpreter.NodeTraces()
			if rig.controller.TraceOrderState() != TraceOrderExhausted || decisionLoss != 0 || nodeLoss != 0 {
				t.Fatal("exhaustion was hidden or mislabeled as eviction")
			}
			lastNode := nodes[len(nodes)-1]
			if nha188Value(t, lastNode.Ordinal()) != exhaustedTraceOrdinal-1 || lastNode.WaitExit() == WaitExitInvalid {
				t.Fatal("last admitted position did not belong to the actual wait exit")
			}
			terminal := decisions[len(decisions)-1]
			want := DecisionFinished
			if cancelRun {
				want = DecisionCancelled
			}
			if terminal.Decision != want {
				t.Fatal("saturation changed terminal decision")
			}
			if value, valid := terminal.Ordinal().Value(); valid || value != 0 {
				t.Fatal("exhausted terminal claimed a reused ordinal")
			}
			// Further native emissions remain inspectable and explicitly unorderable.
			rig.controller.mu.Lock()
			rig.controller.recordLocked(Trace{Decision: DecisionClosed})
			rig.controller.mu.Unlock()
			rig.interpreter.appendNodeTrace(lastNode)
			laterDecisions, laterLoss := rig.controller.Traces()
			laterNodes, laterNodeLoss := rig.interpreter.NodeTraces()
			_, decisionValid := laterDecisions[len(laterDecisions)-1].Ordinal().Value()
			_, nodeValid := laterNodes[len(laterNodes)-1].Ordinal().Value()
			if decisionValid || nodeValid || laterLoss != 0 || laterNodeLoss != 0 || rig.controller.traceOrdinal.Load() != exhaustedTraceOrdinal {
				t.Fatal("post-exhaustion producer reused an ordinal or invented eviction")
			}
			rig.controller.pool.mu.Lock()
			after := rig.controller.pool.used
			rig.controller.pool.mu.Unlock()
			if after != baseline || rig.controller.live != 0 || len(rig.effects()) != 0 {
				t.Fatalf("exhaustion prevented joined resource cleanup: before=%d after=%d live=%d", baseline, after, rig.controller.live)
			}
			releaseObservation()
		})
	}
}

func TestNHA188SnapshotsBindIdentityAndPlanLifetime(t *testing.T) {
	id, _ := ir.ParseScriptID("script.nha188_owner")
	program := nha042Compile(t, "script.nha188_owner", "sequence:\n- stop: done\n", []ir.ScriptID{id})
	first, interpreter := nha042Runtime(t, []ir.Script{program}, &nha042Commands{})
	second, otherInterpreter := nha042Runtime(t, []ir.Script{program}, &nha042Commands{})
	if err := nha042Joined(t, nha188Invoke(t, first, id)); err != nil {
		t.Fatal(err)
	}
	if err := nha042Joined(t, nha188Invoke(t, second, id)); err != nil {
		t.Fatal(err)
	}
	one, two := nha188Snapshot(t, first, interpreter), nha188Snapshot(t, second, otherInterpreter)
	if one.controller == two.controller || nha188Value(t, one.decisions[0].Ordinal()) != nha188Value(t, two.decisions[0].Ordinal()) {
		t.Fatal("fixture did not produce distinct Controller spaces with equal numbers")
	}
	if nha188ObservedDirection(one, two, one.decisions[0].Ordinal(), two.nodes[0].Ordinal()) != nha188OrderInvalid {
		t.Fatal("equal numbers in different Controllers were treated as comparable")
	}
	if err := interpreter.bind(first); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("second bind to the same Controller accepted: %v", err)
	}
	if _, err := NewController(first.plan, nil, interpreter, interpreter); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("second Controller accepted the bound Interpreter: %v", err)
	}
	var typedNil *Interpreter
	if _, err := NewController(first.plan, nil, typedNil, typedNil); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("typed-nil Interpreter admitted: %v", err)
	}
	oldDecisions := append([]Trace(nil), one.decisions...)
	oldNodes := append([]NodeTrace(nil), one.nodes...)
	one.decisions[0].Decision, one.decisions[0].ordinal = DecisionInvalid, TraceOrdinal{}
	one.nodes[0].stage, one.nodes[0].ordinal = NodeStageInvalid, TraceOrdinal{}
	one = nha188Snapshot(t, first, interpreter)
	if !reflect.DeepEqual(oldDecisions, one.decisions) || !reflect.DeepEqual(oldNodes, one.nodes) {
		t.Fatal("snapshots shared mutable ring backing")
	}
	// Publish the same scripts as a new causal image on this Controller. Keep
	// copied A observations: its released interpreter need not retain its ring.
	replacement, err := NewPlanWithScripts(nil, []ir.Script{program})
	if err != nil {
		t.Fatal(err)
	}
	input := interpreter.input
	input.Plan = replacement
	replacementInterpreter, err := NewInterpreter(input)
	if err != nil {
		t.Fatal(err)
	}
	previous := first.traceOrdinal.Load()
	reserved, err := first.ReserveImage(replacement, replacementInterpreter, replacementInterpreter)
	if err != nil {
		t.Fatal(err)
	}
	if err := reserved.Publish(); err != nil {
		t.Fatal(err)
	}
	if err := nha042Joined(t, nha188Invoke(t, first, id)); err != nil {
		t.Fatal(err)
	}
	fresh := nha188Snapshot(t, first, replacementInterpreter)
	if len(fresh.decisions) <= len(oldDecisions) || len(fresh.nodes) == 0 {
		t.Fatal("published B image produced no native observations")
	}
	if fresh.decisions[len(oldDecisions)].Generation == oldDecisions[0].Generation || nha188Value(t, fresh.decisions[len(oldDecisions)].Ordinal()) <= previous || !reflect.DeepEqual(oldDecisions, fresh.decisions[:len(oldDecisions)]) || !reflect.DeepEqual(oldDecisions, one.decisions) || !reflect.DeepEqual(oldNodes, one.nodes) {
		t.Fatal("image publication reset ordering or mutated previously copied observations")
	}
	for _, node := range fresh.nodes {
		if node.Generation() != fresh.decisions[len(oldDecisions)].Generation || nha188Value(t, node.Ordinal()) <= previous {
			t.Fatal("B node observations lost the shared Controller counter or causal image")
		}
	}
	if nha188ObservedDirection(one, fresh, oldDecisions[0].Ordinal(), fresh.decisions[len(oldDecisions)].Ordinal()) != nha188Before {
		t.Fatal("causal image generation was mistaken for Controller ordering identity")
	}
	nha188Close(t, first)
	nha188Close(t, second)
}

func TestNHA188RingBoundsEvictionsAndMeasuredReservations(t *testing.T) {
	id, _ := ir.ParseScriptID("script.nha188_budget")
	program := nha042Compile(t, "script.nha188_budget", "sequence:\n- stop: done\n", []ir.ScriptID{id})
	controller, interpreter := nha042Runtime(t, []ir.Script{program}, &nha042Commands{})
	releaseObservation := nha188RetainImageForObservation(t, controller, interpreter)
	if cap(controller.traces) != MaxTraces || cap(interpreter.traces) != nodeTraceCapacity || nodeTraceCapacity != min(MaxNodeTraces, nodeTraceCharge/(int(unsafe.Sizeof(NodeTrace{}))+512)) || nodeTraceCapacity < 1 {
		t.Fatal("native trace backing escaped measured reservation/capacity")
	}
	// Keep the exact pre-188 layouts as the accounting baseline. NHA-186 adds
	// emission time, immutable image provenance, ancestry and a sink port;
	// NHA-056 adds process cells and a Controller handle; NHA-054 retains the
	// admitted clock port. The decision ordinal must still fit in the original
	// Trace padding. The Controller baseline includes 029 image ownership/close
	// state, excluding the measured sink, ordinal, process observation and clock
	// fields.
	type originalTraceLayout struct {
		Decision    Decision
		Automation  string
		Script      ir.ScriptID
		TriggerKey  string
		RunID       RunID
		Generation  Generation
		MaxExceeded ir.MaxExceededMode
		Error       string
	}
	type controllerLayoutWithoutOrdinal struct {
		mu                            sync.Mutex
		active                        *programImage
		pending                       *ImageReservation
		admission                     *imageAdmission
		plan                          Plan
		reader                        ResultReader
		pool                          memoryPool
		ctx                           context.Context
		cancel                        context.CancelFunc
		done                          chan struct{}
		doneOnce                      sync.Once
		wg                            sync.WaitGroup
		lanes                         map[laneKey]*lane
		traces                        []Trace
		evicted                       uint64
		next                          RunID
		generation                    Generation
		preparing, live               int
		started, closed, closeStarted bool
		err                           error
	}
	historyTraceBytes := unsafe.Sizeof(Trace{}.At) + unsafe.Sizeof(Trace{}.Provenance) + unsafe.Sizeof(Trace{}.ParentID)
	historyControllerBytes := unsafe.Sizeof(controller.sink)
	observationControllerBytes := unsafe.Sizeof(controller.observation) + unsafe.Sizeof(controller.observationHandle)
	clockControllerBytes := unsafe.Sizeof(controller.traceClock)
	expectedTraceBytes := unsafe.Sizeof(originalTraceLayout{}) + historyTraceBytes
	expectedControllerBytes := unsafe.Sizeof(controllerLayoutWithoutOrdinal{}) + unsafe.Sizeof(controller.traceOrdinal) + historyControllerBytes + observationControllerBytes + clockControllerBytes
	if unsafe.Sizeof(Trace{}) != expectedTraceBytes || unsafe.Sizeof(Controller{}) != expectedControllerBytes {
		t.Fatalf("trace layout grew beyond explicit history/observation/clock fields: Trace=%d want%d Controller=%d want%d", unsafe.Sizeof(Trace{}), expectedTraceBytes, unsafe.Sizeof(Controller{}), expectedControllerBytes)
	}
	t.Logf("NHA186/056/054 measured fields: time=%d provenance=%d parent=%d Trace=%d/%d; sink=%d observation=%d clock=%d ordinal=%d Controller=%d/%d", unsafe.Sizeof(Trace{}.At), unsafe.Sizeof(Trace{}.Provenance), unsafe.Sizeof(Trace{}.ParentID), unsafe.Sizeof(Trace{}), expectedTraceBytes, historyControllerBytes, observationControllerBytes, clockControllerBytes, unsafe.Sizeof(controller.traceOrdinal), unsafe.Sizeof(Controller{}), expectedControllerBytes)
	// The trace ring retains bounded cloned errors. Sources and identity
	// strings still reference their existing admitted owners; this patch does
	// not add any identity/payload copy. Error allocation gets allocator margin.
	traceBytes := MaxTraces * (int(unsafe.Sizeof(Trace{})) + 256 + 32)
	nodeBytes := nodeTraceCapacity * (int(unsafe.Sizeof(NodeTrace{})) + 512)
	if int(unsafe.Sizeof(Controller{}))+traceBytes > controllerCharge || nodeBytes > nodeTraceCharge || controllerCharge+nodeTraceCharge > MaxMemoryBytes {
		t.Fatalf("trace reservations exceeded: decision ring=%d/%d nodes=%d/%d pool=%d", traceBytes, controllerCharge, nodeBytes, nodeTraceCharge, MaxMemoryBytes)
	}
	// Inventory the complete public constructor's lane domain, including ports
	// other than Interpreter. Sizeof(map) does NOT bound its allocated tables.
	// Runtime map churn/high-water backing and externally retained plan metadata
	// are baseline accounting limits, not a new proven bound for this patch.
	const maxPublicLanes = automationtriggers.MaxTriggers + 64 // NewPlanWithScripts
	publicLaneStructs := maxPublicLanes * (int(unsafe.Sizeof(laneKey{})) + int(unsafe.Sizeof(lane{})))
	t.Logf("measured Trace=%d (base=%d), NodeTrace=%d Controller=%d (base=%d), counter=%d; ring+error allowance=%d/%d node image=%d/%d capacity=%d pool=%d; public lane structs=%d across %d lanes EXCLUDING runtime map backing/retained-plan metadata (baseline limit)", unsafe.Sizeof(Trace{}), unsafe.Sizeof(originalTraceLayout{}), unsafe.Sizeof(NodeTrace{}), unsafe.Sizeof(Controller{}), unsafe.Sizeof(controllerLayoutWithoutOrdinal{}), unsafe.Sizeof(controller.traceOrdinal), traceBytes, controllerCharge, nodeBytes, nodeTraceCharge, nodeTraceCapacity, MaxMemoryBytes, publicLaneStructs, maxPublicLanes)
	// Existing bind rejects an unavailable reservation before making copies/maps.
	unbound, err := NewInterpreter(interpreter.input)
	if err != nil {
		t.Fatal(err)
	}
	full := &Controller{plan: interpreter.input.Plan, generation: controller.generation, done: make(chan struct{})}
	full.active = &programImage{plan: full.plan, generation: full.generation, refs: 1, lifecycle: programImagePublished}
	if !full.pool.adjust(0, MaxMemoryBytes) {
		t.Fatal("failed to fill the test-only pool")
	}
	if err := unbound.bind(full); !errors.Is(err, ErrCapacity) || unbound.controller != nil || unbound.traces != nil || unbound.automations != nil || unbound.scripts != nil || full.traceOrdinal.Load() != 0 {
		t.Fatalf("bind allocated/copied before reservation: %v", err)
	}
	controller.pool.mu.Lock()
	baseline := controller.pool.used
	controller.pool.mu.Unlock()
	// Fill both real rings. Interleaving leaves intentional gaps in each ring;
	// snapshots must keep the actual positions instead of silently renumbering.
	errorBacking := strings.Repeat("e", 1<<20)
	count := max(MaxTraces, nodeTraceCapacity) + 3
	for index := 0; index < count; index++ {
		controller.mu.Lock()
		controller.recordLocked(Trace{Decision: DecisionPreparationError, Generation: controller.generation, Error: errorBacking[:512]})
		controller.mu.Unlock()
		interpreter.appendNodeTrace(NodeTrace{generation: controller.generation, stage: NodeEntered})
	}
	nha188Close(t, controller)
	decisions, decisionLoss := controller.Traces()
	nodes, nodeLoss := interpreter.NodeTraces()
	if len(decisions) != MaxTraces || cap(controller.traces) != MaxTraces || len(nodes) != nodeTraceCapacity || cap(interpreter.traces) != nodeTraceCapacity || decisionLoss != uint64(count-MaxTraces) || nodeLoss != uint64(count-nodeTraceCapacity) || controller.TraceOrderState() != TraceOrderHealthy {
		t.Fatalf("ring bound/loss changed: decisions=%d loss=%d nodes=%d loss=%d", len(decisions), decisionLoss, len(nodes), nodeLoss)
	}
	for index, trace := range decisions {
		want := uint64(2*(count-MaxTraces+index) + 1)
		if nha188Value(t, trace.Ordinal()) != want || len(trace.Error) != 256 || unsafe.StringData(trace.Error) == unsafe.StringData(errorBacking) {
			t.Fatal("decision eviction renumbered positions or retained large backing")
		}
	}
	for index, trace := range nodes {
		want := uint64(2*(count-nodeTraceCapacity+index) + 2)
		if nha188Value(t, trace.Ordinal()) != want {
			t.Fatal("node eviction hid ordinal gaps")
		}
	}
	controller.pool.mu.Lock()
	after := controller.pool.used
	controller.pool.mu.Unlock()
	if baseline != after || after > MaxMemoryBytes {
		t.Fatalf("trace production grew the reserved pool: before=%d after=%d", baseline, after)
	}
	releaseObservation()
}

func TestNHA188FinalWaitSourceCompletionSameTick(t *testing.T) {
	for _, templateWait := range []bool{false, true} {
		name := "trigger_event"
		if templateWait {
			name = "template_state"
		}
		t.Run(name, func(t *testing.T) {
			id, _ := ir.ParseScriptID("script.nha188_completed")
			body := "sequence:\n- wait_for_trigger:\n  - trigger: event\n    event_type: nha042_release\n"
			waitKind := ir.ActionWait
			if templateWait {
				waitKind = ir.ActionWaitTemplate
				body = "sequence:\n- wait_template: \"{{ is_state('switch.fixture', 'on') }}\"\n"
			}
			program := nha042Compile(t, "script.nha188_completed", body, []ir.ScriptID{id})
			rig := nha042FullRig(t, []ir.Script{program}, &nha042Commands{})
			releaseObservation := nha188RetainImageForObservation(t, rig.controller, rig.interpreter)
			rig.state(t, false)
			handle := nha188Invoke(t, rig.controller, id)
			nha188Suspended(t, handle)
			if templateWait {
				rig.state(t, true)
			} else {
				event, err := integrations.NewDeviceEvent("nha042_release", rig.clock.Now(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := rig.session.Emit(context.Background(), rig.binding, event); err != nil {
					t.Fatal(err)
				}
			}
			if err := nha042Joined(t, handle); err != nil {
				t.Fatal(err)
			}
			beforeClose := nha188Snapshot(t, rig.controller, rig.interpreter)
			waiting, ended, _ := beforeClose.finalWait(t, handle.ID(), nha188OnlyActionSource(t, program, waitKind), WaitCompleted, NodeCompleted, DecisionFinished)
			if !waiting.At().Equal(ended.At()) || !ended.At().Equal(rig.clock.Now()) {
				t.Fatal("source completion order depended on different timestamps")
			}
			nha188Close(t, rig.controller)
			if !reflect.DeepEqual(beforeClose, nha188Snapshot(t, rig.controller, rig.interpreter)) || len(rig.effects()) != 0 {
				t.Fatal("source completion appeared only through cleanup or an extra action")
			}
			releaseObservation()
		})
	}
}
