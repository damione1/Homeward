package automationruns

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/observability"
	"google.golang.org/protobuf/encoding/protojson"
)

func nha056ObservedController(t *testing.T, mode string) (*Controller, *nha042GatedCommands, ir.ScriptID, *observability.Cells, *observability.CursorCodec) {
	t.Helper()
	id, err := ir.ParseScriptID("script.nha056_lanes")
	if err != nil {
		t.Fatal(err)
	}
	program := nha042Compile(t, "script.nha056_lanes", "mode: "+mode+"\nmax: 3\nsequence:\n- action: switch.turn_on\n  target: {entity_id: switch.nha042_fixture}\n  data: {}\n", []ir.ScriptID{id})
	plan, err := NewPlanWithScripts(nil, []ir.Script{program})
	if err != nil {
		t.Fatal(err)
	}
	gate := nha042CommandGate()
	interpreter, err := NewInterpreter(InterpreterInput{Plan: plan, Conditions: nha042SharedConditions(t), Commands: gate, Clock: automationtriggers.RealClock{}, Zone: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewController(plan, nil, interpreter, interpreter)
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := observability.NewProcessObservationEpoch()
	if err != nil {
		t.Fatal(err)
	}
	cells, err := observability.NewCells(epoch)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := observability.NewCursorCodec(epoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.AttachObservation(cells); err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		gate.openAll()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := controller.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return controller, gate, id, cells, codec
}
func nha056RunCount(t *testing.T, c *Controller, cancelled, superseded uint64) {
	t.Helper()
	_, metrics, err := c.ObservationMetrics()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range metrics.GetInstalled().Kinds {
		if kind.Kind == obs.RunKind_RUN_KIND_SCRIPT && (kind.CancelledTotal.GetExact() != cancelled || kind.SupersededTotal.GetExact() != superseded) {
			t.Fatal("real terminal removals were recounted or lost")
		}
	}
}
func TestNHA056RealScriptPendingCancellationSurvivesNativeEviction(t *testing.T) {
	c, gate, id, cells, codec := nha056ObservedController(t, "queued")
	first := nha188Invoke(t, c, id)
	if got := nha041Receive(t, gate.entered); got != first.ID() {
		t.Fatal("wrong real run entered")
	}
	second := nha188Invoke(t, c, id)
	third := nha188Invoke(t, c, id)
	queues, metrics, err := c.ObservationMetrics()
	if err != nil {
		t.Fatal(err)
	}
	if queues[1].GetInstalled().QueuedEntries.GetValue() != 2 || queues[1].GetInstalled().CapacityEntries.GetValue() != 2 || metrics.GetInstalled().Kinds[1].Active.GetValue() != 1 {
		t.Fatal("real script FIFO/active gauges are not distinct")
	}
	select {
	case <-second.Cancel():
	case <-time.After(3 * time.Second):
		t.Fatal("pending script did not leave live")
	}
	nha056RunCount(t, c, 1, 0)
	handle, err := c.ObservationHandle()
	if err != nil {
		t.Fatal(err)
	}
	var digest [32]byte
	digest[0] = 1
	binding, err := observability.NewReadBinding(1, digest)
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := cells.Epoch()
	if err != nil {
		t.Fatal(err)
	}
	captures, err := observability.NewTraceCaptures(epoch, codec)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := captures.Reserve(binding, handle, uint64(first.ID()))
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.Release()
	// Place sensitive fields in both real native rings before the projection.
	// An unrelated interleaved run must not enter this targeted capture.
	c.mu.Lock()
	var firstDecision, thirdDecision Trace
	for _, trace := range c.traces {
		if trace.RunID == first.ID() && trace.Generation != 0 && trace.Provenance.Valid() {
			firstDecision = trace
		}
		if trace.RunID == third.ID() && trace.Generation != 0 && trace.Provenance.Valid() {
			thirdDecision = trace
		}
	}
	if firstDecision.RunID != first.ID() || thirdDecision.RunID != third.ID() {
		c.mu.Unlock()
		t.Fatal("real causal decisions absent before native pressure")
	}
	firstCanary := firstDecision
	firstCanary.Decision, firstCanary.Error, firstCanary.TriggerKey = DecisionExecutionError, "NHA056_TRACE_PRIVATE_ERROR_CANARY", "NHA056_TRACE_PRIVATE_SOURCE_CANARY"
	thirdCanary := thirdDecision
	thirdCanary.Decision, thirdCanary.Error = DecisionExecutionError, "NHA056_OTHER_RUN_CANARY"
	c.recordLocked(firstCanary)
	c.recordLocked(thirdCanary)
	c.mu.Unlock()
	c.mu.Lock()
	interpreter := c.active.interpreter
	c.mu.Unlock()
	nativeNodes, _ := interpreter.NodeTraces()
	if len(nativeNodes) == 0 {
		t.Fatal("real interpreter did not produce a native source")
	}
	sensitiveNode := nativeNodes[0]
	sensitiveNode.run, sensitiveNode.kind, sensitiveNode.stage = first.ID(), ir.ActionWaitTemplate, NodeCompleted
	sensitiveNode.wait = waitObservation{kind: ir.ActionWaitTemplate, completed: true, remaining: 987654 * time.Second, hasRemaining: true, exit: WaitCompleted}
	sensitiveNode.hasWait = true
	interpreter.appendNodeTrace(sensitiveNode)
	value, present, err := sensitiveNode.WaitValue()
	if err != nil || !present || sensitiveNode.Source().Object() == "" {
		t.Fatal("native sensitive trace precondition absent", err)
	}
	remaining, present := value.Field("remaining")
	if number, valid := remaining.Float(); !present || !valid || number != 987654 {
		t.Fatal("private wait value was not present at its native owner")
	}
	rows, cuts, truncated, err := c.ObservationTrace(first.ID())
	if err != nil || truncated || len(rows) == 0 {
		t.Fatal("real retained cuts unavailable", err)
	}
	capture, err := reservation.Commit(rows, cuts)
	if err != nil {
		t.Fatal(err)
	}
	size, _ := observability.NewPageSize(100)
	request, err := observability.NewTracePageRequest(size, capture)
	if err != nil {
		t.Fatal(err)
	}
	page, err := captures.Read(request, binding)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := page.Wire("")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := protojson.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{"switch.nha042_fixture", sensitiveNode.Source().Object(), "NHA056_TRACE_PRIVATE_ERROR_CANARY", "NHA056_TRACE_PRIVATE_SOURCE_CANARY", "NHA056_OTHER_RUN_CANARY", "987654"} {
		if bytes.Contains(encoded, []byte(canary)) {
			t.Fatal("native trace payload escaped safe projection")
		}
	}
	if wire.GetPage().Cuts.Coherence != obs.TraceCutCoherence_TRACE_CUT_COHERENCE_SEPARATE_OWNER_CUTS {
		t.Fatal("independent native cuts were presented as globally atomic")
	}
	var previous uint64
	for _, row := range wire.GetPage().Rows {
		if row.RunId != uint64(first.ID()) || row.Ordinal.GetNative() <= previous {
			t.Fatal("targeted native trace mixed runs or lost its shared native order")
		}
		previous = row.Ordinal.GetNative()
	}
	c.mu.Lock()
	// Pressure must retain the real run's causal image identity after every
	// earlier controller row has been evicted, so both native cuts still exist.
	pressure := firstDecision
	pressure.Decision, pressure.Error = DecisionFinished, "NHA056_NATIVE_EVICTED_ERROR_CANARY"
	for range MaxTraces {
		c.recordLocked(pressure)
	}
	c.mu.Unlock()
	limitedRows, limitedCuts, limited, err := c.ObservationTrace(first.ID())
	if err != nil || !limited || len(limitedRows) != 512 {
		t.Fatal("combined native cuts exceeded 512 rows without explicit truncation", err)
	}
	limitedReservation, err := captures.Reserve(binding, handle, uint64(first.ID()))
	if err != nil {
		t.Fatal(err)
	}
	defer limitedReservation.Release()
	limitedHandle, err := limitedReservation.CommitTruncated(limitedRows, limitedCuts, obs.TraceTruncationReason_TRACE_TRUNCATION_REASON_ROW_LIMIT)
	if err != nil {
		t.Fatal(err)
	}
	limitedRequest, err := observability.NewTracePageRequest(size, limitedHandle)
	if err != nil {
		t.Fatal(err)
	}
	limitedPage, err := captures.Read(limitedRequest, binding)
	if err != nil {
		t.Fatal(err)
	}
	limitedToken := ""
	if next, more := limitedPage.Next(); more {
		limitedToken, err = codec.EncodeTrace(next)
		if err != nil {
			t.Fatal(err)
		}
	}
	limitedWire, err := limitedPage.Wire(limitedToken)
	if err != nil || limitedWire.GetTruncated().Reason != obs.TraceTruncationReason_TRACE_TRUNCATION_REASON_ROW_LIMIT || limitedWire.GetTruncated().Page.Cuts.ControllerEvicted == 0 {
		t.Fatal("native eviction and 056 truncation were conflated", err)
	}
	c.BeginClose()
	c.BeginClose()
	gate.open(first.ID())
	select {
	case <-first.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("active cancellation did not actually join")
	}
	select {
	case <-third.Done():
	default:
		t.Fatal("pending close was not terminal")
	}
	nha056RunCount(t, c, 3, 0)
	c.mu.Lock()
	otherPressure := thirdDecision
	otherPressure.Decision, otherPressure.Error, otherPressure.TriggerKey = DecisionFinished, "NHA056_TRACE_PRIVATE_ERROR_CANARY", "NHA056_TRACE_PRIVATE_SOURCE_CANARY"
	for range 600 {
		c.recordLocked(otherPressure)
	}
	c.mu.Unlock()
	for range 10 {
		nha056RunCount(t, c, 3, 0)
	}
	after, err := c.ObservationHandle()
	if err != nil || !after.Equal(handle) {
		t.Fatal("controller handle changed with close/trace eviction", err)
	}
}
func TestNHA056RealRestartSupersessionCountsRemovalOnce(t *testing.T) {
	c, gate, id, _, _ := nha056ObservedController(t, "restart")
	first := nha188Invoke(t, c, id)
	if nha041Receive(t, gate.entered) != first.ID() {
		t.Fatal("wrong run")
	}
	second := nha188Invoke(t, c, id)
	if nha041Receive(t, gate.cancelled) != first.ID() {
		t.Fatal("restart cancelled wrong run")
	}
	third := nha188Invoke(t, c, id)
	select {
	case <-second.Done():
	default:
		t.Fatal("superseded replacement not removed")
	}
	nha056RunCount(t, c, 0, 1)
	c.BeginClose()
	c.BeginClose()
	gate.open(first.ID())
	select {
	case <-first.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("old restart operation did not join")
	}
	select {
	case <-third.Done():
	default:
		t.Fatal("replacement close incomplete")
	}
	nha056RunCount(t, c, 2, 1)
}
