package automationruns

import (
	"sort"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/observability"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func observationDecision(value Decision) (obs.TraceDecision, error) {
	switch value {
	case DecisionPreparing:
		return obs.TraceDecision_TRACE_DECISION_PREPARING, nil
	case DecisionAdmitted:
		return obs.TraceDecision_TRACE_DECISION_ADMITTED, nil
	case DecisionQueued:
		return obs.TraceDecision_TRACE_DECISION_QUEUED, nil
	case DecisionRestartPending:
		return obs.TraceDecision_TRACE_DECISION_RESTART_PENDING, nil
	case DecisionSingleBusy:
		return obs.TraceDecision_TRACE_DECISION_SINGLE_BUSY, nil
	case DecisionMaxExceeded:
		return obs.TraceDecision_TRACE_DECISION_MAX_EXCEEDED, nil
	case DecisionPreparationSaturated:
		return obs.TraceDecision_TRACE_DECISION_PREPARATION_SATURATED, nil
	case DecisionMemorySaturated:
		return obs.TraceDecision_TRACE_DECISION_MEMORY_SATURATED, nil
	case DecisionConditionFalse:
		return obs.TraceDecision_TRACE_DECISION_CONDITION_FALSE, nil
	case DecisionPreparationError:
		return obs.TraceDecision_TRACE_DECISION_PREPARATION_ERROR, nil
	case DecisionExecutionError:
		return obs.TraceDecision_TRACE_DECISION_EXECUTION_ERROR, nil
	case DecisionFinished:
		return obs.TraceDecision_TRACE_DECISION_FINISHED, nil
	case DecisionCancelled:
		return obs.TraceDecision_TRACE_DECISION_CANCELLED, nil
	case DecisionSuperseded:
		return obs.TraceDecision_TRACE_DECISION_SUPERSEDED, nil
	case DecisionClosed:
		return obs.TraceDecision_TRACE_DECISION_CLOSED, nil
	case DecisionUnknownAutomation:
		return obs.TraceDecision_TRACE_DECISION_UNKNOWN_AUTOMATION, nil
	case DecisionTriggerMissed:
		return obs.TraceDecision_TRACE_DECISION_TRIGGER_MISSED, nil
	case DecisionTriggerDegraded:
		return obs.TraceDecision_TRACE_DECISION_TRIGGER_DEGRADED, nil
	default:
		return 0, ErrInvalidPlan
	}
}
func observationAction(value ir.ActionKind) (obs.TraceActionKind, error) {
	switch value {
	case ir.ActionVariables:
		return obs.TraceActionKind_TRACE_ACTION_KIND_VARIABLES, nil
	case ir.ActionSequence:
		return obs.TraceActionKind_TRACE_ACTION_KIND_SEQUENCE, nil
	case ir.ActionChoose:
		return obs.TraceActionKind_TRACE_ACTION_KIND_CHOOSE, nil
	case ir.ActionIf:
		return obs.TraceActionKind_TRACE_ACTION_KIND_IF, nil
	case ir.ActionRepeat:
		return obs.TraceActionKind_TRACE_ACTION_KIND_REPEAT, nil
	case ir.ActionParallel:
		return obs.TraceActionKind_TRACE_ACTION_KIND_PARALLEL, nil
	case ir.ActionWait:
		return obs.TraceActionKind_TRACE_ACTION_KIND_WAIT, nil
	case ir.ActionStop:
		return obs.TraceActionKind_TRACE_ACTION_KIND_STOP, nil
	case ir.ActionCall:
		return obs.TraceActionKind_TRACE_ACTION_KIND_CALL, nil
	case ir.ActionDelay:
		return obs.TraceActionKind_TRACE_ACTION_KIND_DELAY, nil
	case ir.ActionWaitTemplate:
		return obs.TraceActionKind_TRACE_ACTION_KIND_WAIT_TEMPLATE, nil
	default:
		return 0, ErrInvalidPlan
	}
}
func observationNodeStage(value NodeStage) (obs.TraceNodeStage, error) {
	switch value {
	case NodeEntered:
		return obs.TraceNodeStage_TRACE_NODE_STAGE_ENTERED, nil
	case NodeCompleted:
		return obs.TraceNodeStage_TRACE_NODE_STAGE_COMPLETED, nil
	case NodeStopped:
		return obs.TraceNodeStage_TRACE_NODE_STAGE_STOPPED, nil
	case NodeCancelled:
		return obs.TraceNodeStage_TRACE_NODE_STAGE_CANCELLED, nil
	case NodeFailed:
		return obs.TraceNodeStage_TRACE_NODE_STAGE_FAILED, nil
	case NodeWaiting:
		return obs.TraceNodeStage_TRACE_NODE_STAGE_WAITING, nil
	default:
		return 0, ErrInvalidPlan
	}
}
func observationExecutionFailure(value ExecutionErrorKind) (obs.TraceExecutionErrorKind, error) {
	switch value {
	case ExecutionInvalidProgram:
		return obs.TraceExecutionErrorKind_TRACE_EXECUTION_ERROR_KIND_INVALID_PROGRAM, nil
	case ExecutionWorkBudget:
		return obs.TraceExecutionErrorKind_TRACE_EXECUTION_ERROR_KIND_WORK_BUDGET, nil
	case ExecutionRecursion:
		return obs.TraceExecutionErrorKind_TRACE_EXECUTION_ERROR_KIND_RECURSION, nil
	case ExecutionWaitTimeout:
		return obs.TraceExecutionErrorKind_TRACE_EXECUTION_ERROR_KIND_WAIT_TIMEOUT, nil
	case ExecutionSourceUnavailable:
		return obs.TraceExecutionErrorKind_TRACE_EXECUTION_ERROR_KIND_SOURCE_UNAVAILABLE, nil
	case ExecutionConditionError:
		return obs.TraceExecutionErrorKind_TRACE_EXECUTION_ERROR_KIND_CONDITION_ERROR, nil
	case ExecutionCommandError:
		return obs.TraceExecutionErrorKind_TRACE_EXECUTION_ERROR_KIND_COMMAND_ERROR, nil
	default:
		return 0, ErrInvalidPlan
	}
}
func observationOrdinal(value TraceOrdinal, order TraceOrderState) (*obs.TraceOrdinal, error) {
	if native, valid := value.Value(); valid {
		return &obs.TraceOrdinal{Result: &obs.TraceOrdinal_Native{Native: native}}, nil
	}
	if order != TraceOrderExhausted {
		return nil, ErrInvalidPlan
	}
	return &obs.TraceOrdinal{Result: &obs.TraceOrdinal_Exhausted{Exhausted: &emptypb.Empty{}}}, nil
}
func observationControllerRow(trace Trace, order TraceOrderState) (observability.TraceRow, error) {
	decision, err := observationDecision(trace.Decision)
	if err != nil {
		return observability.TraceRow{}, err
	}
	ordinal, err := observationOrdinal(trace.Ordinal(), order)
	if err != nil {
		return observability.TraceRow{}, err
	}
	record := &obs.ControllerTraceRecord{Decision: decision, MaxExceeded: &obs.ControllerTraceRecord_NotApplicable{NotApplicable: &emptypb.Empty{}}}
	if trace.Decision == DecisionSingleBusy || trace.Decision == DecisionMaxExceeded {
		var mode obs.TraceMaxExceededMode
		switch trace.MaxExceeded {
		case ir.MaxExceededSilent:
			mode = obs.TraceMaxExceededMode_TRACE_MAX_EXCEEDED_MODE_SILENT
		case ir.MaxExceededWarning:
			mode = obs.TraceMaxExceededMode_TRACE_MAX_EXCEEDED_MODE_WARNING
		case ir.MaxExceededError:
			mode = obs.TraceMaxExceededMode_TRACE_MAX_EXCEEDED_MODE_ERROR
		default:
			return observability.TraceRow{}, ErrInvalidPlan
		}
		record.MaxExceeded = &obs.ControllerTraceRecord_Mode{Mode: mode}
	}
	return observability.NewTraceRow(&obs.TraceRow{RunId: uint64(trace.RunID), Ordinal: ordinal, Record: &obs.TraceRow_Controller{Controller: record}})
}
func observationNodeRow(trace NodeTrace, order TraceOrderState) (observability.TraceRow, error) {
	action, err := observationAction(trace.kind)
	if err != nil {
		return observability.TraceRow{}, err
	}
	stage, err := observationNodeStage(trace.stage)
	if err != nil {
		return observability.TraceRow{}, err
	}
	ordinal, err := observationOrdinal(trace.Ordinal(), order)
	if err != nil {
		return observability.TraceRow{}, err
	}
	failure := &obs.TraceFailure{Result: &obs.TraceFailure_NoFailure{NoFailure: &emptypb.Empty{}}}
	if trace.failure != ExecutionErrorInvalid {
		kind, err := observationExecutionFailure(trace.failure)
		if err != nil {
			return observability.TraceRow{}, err
		}
		failure.Result = &obs.TraceFailure_Known{Known: kind}
	} else if trace.stage == NodeFailed {
		failure.Result = &obs.TraceFailure_NotRecorded{NotRecorded: &emptypb.Empty{}}
	}
	record := &obs.NodeTraceRecord{ActionKind: action, Stage: stage, OccurredAt: timestamppb.New(trace.at), Failure: failure}
	return observability.NewTraceRow(&obs.TraceRow{RunId: uint64(trace.run), Ordinal: ordinal, Record: &obs.TraceRow_Node{Node: record}})
}

// Call only after reserving the process capture slot and byte budget. Projection
// happens in each native ring under its own lock: no payload/Source/Error copy
// escapes, and at most 512 small safe rows are allocated across both cuts.
func (c *Controller) ObservationTrace(run RunID) ([]observability.TraceRow, observability.TraceCutMetadata, bool, error) {
	if c == nil || c.done == nil || run == 0 || c.observation == nil {
		return nil, observability.TraceCutMetadata{}, false, ErrInvalidPlan
	}
	rows := make([]observability.TraceRow, 0, 512)
	truncated := false
	order := c.TraceOrderState()
	if order == TraceOrderInvalid {
		return nil, observability.TraceCutMetadata{}, false, ErrInvalidPlan
	}
	c.mu.Lock()
	var generation Generation
	for _, trace := range c.traces {
		if trace.RunID != run {
			continue
		}
		if len(rows) == 512 {
			truncated = true
			continue
		}
		if trace.Generation != 0 {
			generation = trace.Generation
		}
		row, err := observationControllerRow(trace, c.TraceOrderState())
		if err != nil {
			c.mu.Unlock()
			return nil, observability.TraceCutMetadata{}, false, err
		}
		rows = append(rows, row)
	}
	controllerEvicted := c.evicted
	// The bounded controller ring may have evicted every decision for this
	// run. Its image can still be owned by the run or a causal descendant.
	// Snapshot only images already owned by the active plan or live lanes;
	// explicit post-Close native observers do not extend this product reader.
	var images [MaxLiveRuns + 1]*programImage
	imageCount := 0
	var snapshotErr error
	retain := func(candidate *programImage) {
		if snapshotErr != nil || candidate == nil || candidate.interpreter == nil || c.closed {
			return
		}
		for index := 0; index < imageCount; index++ {
			if images[index] == candidate {
				return
			}
		}
		if imageCount == len(images) || !c.retainImageLocked(candidate) {
			snapshotErr = ErrInvalidPlan
			return
		}
		images[imageCount] = candidate
		imageCount++
	}
	retain(c.active)
	for _, lane := range c.lanes {
		for _, entry := range lane.active {
			retain(entry.image)
		}
		for _, entry := range lane.queue {
			retain(entry.image)
		}
		if lane.replacement != nil {
			retain(lane.replacement.image)
		}
	}
	if imageCount != 0 {
		c.wg.Add(1)
	}
	c.mu.Unlock()
	if imageCount != 0 {
		defer c.wg.Done()
		defer func() {
			for index := 0; index < imageCount; index++ {
				c.releaseImage(images[index])
			}
		}()
	}
	if snapshotErr != nil {
		return nil, observability.TraceCutMetadata{}, false, snapshotErr
	}
	controllerAt := time.Now().UTC()
	var nodeEvicted, historicalEvicted uint64
	var nodeImage *programImage
	var historicalAt time.Time
	nodeAt := time.Now().UTC()
	for index := 0; index < imageCount; index++ {
		image := images[index]
		interpreter := image.interpreter
		interpreter.mu.Lock()
		matched := false
		for index := 0; index < len(interpreter.traces); index++ {
			trace := interpreter.traces[(interpreter.traceNext+index)%len(interpreter.traces)]
			if trace.run != run {
				continue
			}
			if nodeImage != nil && nodeImage != image || generation != 0 && generation != image.generation {
				interpreter.mu.Unlock()
				return nil, observability.TraceCutMetadata{}, false, ErrInvalidPlan
			}
			nodeImage, matched = image, true
			if len(rows) == 512 {
				truncated = true
				continue
			}
			row, err := observationNodeRow(trace, c.TraceOrderState())
			if err != nil {
				interpreter.mu.Unlock()
				return nil, observability.TraceCutMetadata{}, false, err
			}
			rows = append(rows, row)
		}
		evicted := interpreter.evicted
		interpreter.mu.Unlock()
		capturedAt := time.Now().UTC()
		if matched {
			nodeEvicted, nodeAt = evicted, capturedAt
		} else if generation != 0 && generation == image.generation {
			historicalEvicted, historicalAt = evicted, capturedAt
		}
	}
	if nodeImage == nil && !historicalAt.IsZero() {
		// A retained controller decision can still identify its ring when
		// every node of this run has been evicted from that same image.
		nodeEvicted, nodeAt = historicalEvicted, historicalAt
	}
	if c.TraceOrderState() == TraceOrderExhausted {
		order = TraceOrderExhausted
	}
	wireOrder := obs.TraceOrderState_TRACE_ORDER_STATE_HEALTHY
	if order == TraceOrderExhausted {
		wireOrder = obs.TraceOrderState_TRACE_ORDER_STATE_EXHAUSTED
	} else {
		sort.SliceStable(rows, func(i, j int) bool {
			left, _ := rows[i].NativeOrdinal()
			right, _ := rows[j].NativeOrdinal()
			return left < right
		})
	}
	cuts, err := observability.NewTraceCutMetadata(&obs.TraceCutMetadata{Coherence: obs.TraceCutCoherence_TRACE_CUT_COHERENCE_SEPARATE_OWNER_CUTS, Order: wireOrder, ControllerEvicted: controllerEvicted, NodeEvicted: nodeEvicted, ControllerCapturedAt: timestamppb.New(controllerAt), NodeCapturedAt: timestamppb.New(nodeAt)})
	return rows, cuts, truncated, err
}
