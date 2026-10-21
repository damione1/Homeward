package server

import (
	history "github.com/damione1/Homeward/gen/home/history/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/damione1/Homeward/internal/recordermaintenance"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func historyDecision(value uint8) (obs.TraceDecision, error) {
	switch value {
	case 1:
		return obs.TraceDecision_TRACE_DECISION_PREPARING, nil
	case 2:
		return obs.TraceDecision_TRACE_DECISION_ADMITTED, nil
	case 3:
		return obs.TraceDecision_TRACE_DECISION_QUEUED, nil
	case 4:
		return obs.TraceDecision_TRACE_DECISION_RESTART_PENDING, nil
	case 5:
		return obs.TraceDecision_TRACE_DECISION_SINGLE_BUSY, nil
	case 6:
		return obs.TraceDecision_TRACE_DECISION_MAX_EXCEEDED, nil
	case 7:
		return obs.TraceDecision_TRACE_DECISION_PREPARATION_SATURATED, nil
	case 8:
		return obs.TraceDecision_TRACE_DECISION_MEMORY_SATURATED, nil
	case 9:
		return obs.TraceDecision_TRACE_DECISION_CONDITION_FALSE, nil
	case 10:
		return obs.TraceDecision_TRACE_DECISION_PREPARATION_ERROR, nil
	case 11:
		return obs.TraceDecision_TRACE_DECISION_EXECUTION_ERROR, nil
	case 12:
		return obs.TraceDecision_TRACE_DECISION_FINISHED, nil
	case 13:
		return obs.TraceDecision_TRACE_DECISION_CANCELLED, nil
	case 14:
		return obs.TraceDecision_TRACE_DECISION_SUPERSEDED, nil
	case 15:
		return obs.TraceDecision_TRACE_DECISION_CLOSED, nil
	case 16:
		return obs.TraceDecision_TRACE_DECISION_UNKNOWN_AUTOMATION, nil
	case 17:
		return obs.TraceDecision_TRACE_DECISION_TRIGGER_MISSED, nil
	case 18:
		return obs.TraceDecision_TRACE_DECISION_TRIGGER_DEGRADED, nil
	default:
		return 0, historyquery.ErrIntegrity
	}
}
func historyNode(value uint8) (obs.TraceNodeStage, error) {
	switch value {
	case 1:
		return obs.TraceNodeStage_TRACE_NODE_STAGE_ENTERED, nil
	case 2:
		return obs.TraceNodeStage_TRACE_NODE_STAGE_COMPLETED, nil
	case 3:
		return obs.TraceNodeStage_TRACE_NODE_STAGE_STOPPED, nil
	case 4:
		return obs.TraceNodeStage_TRACE_NODE_STAGE_CANCELLED, nil
	case 5:
		return obs.TraceNodeStage_TRACE_NODE_STAGE_FAILED, nil
	case 6:
		return obs.TraceNodeStage_TRACE_NODE_STAGE_WAITING, nil
	default:
		return 0, historyquery.ErrIntegrity
	}
}
func historyCommand(value uint8) (history.CommandOutcome, error) {
	switch value {
	case 1:
		return history.CommandOutcome_COMMAND_OUTCOME_ACCEPTED, nil
	case 2:
		return history.CommandOutcome_COMMAND_OUTCOME_DISPATCHED, nil
	case 3:
		return history.CommandOutcome_COMMAND_OUTCOME_ACKNOWLEDGED, nil
	case 4:
		return history.CommandOutcome_COMMAND_OUTCOME_CONFIRMED, nil
	case 5:
		return history.CommandOutcome_COMMAND_OUTCOME_INDETERMINATE, nil
	case 6:
		return history.CommandOutcome_COMMAND_OUTCOME_REFUSED, nil
	case 7:
		return history.CommandOutcome_COMMAND_OUTCOME_NOT_ATTEMPTED, nil
	default:
		return 0, historyquery.ErrIntegrity
	}
}
func historyMember(value uint8) (history.SceneMemberOutcome, error) {
	switch value {
	case 1:
		return history.SceneMemberOutcome_SCENE_MEMBER_OUTCOME_PREPARED, nil
	case 2:
		return history.SceneMemberOutcome_SCENE_MEMBER_OUTCOME_ALREADY_SATISFIED, nil
	case 3:
		return history.SceneMemberOutcome_SCENE_MEMBER_OUTCOME_REFUSED, nil
	case 4:
		return history.SceneMemberOutcome_SCENE_MEMBER_OUTCOME_ACCEPTED, nil
	case 5:
		return history.SceneMemberOutcome_SCENE_MEMBER_OUTCOME_DISPATCHED, nil
	case 6:
		return history.SceneMemberOutcome_SCENE_MEMBER_OUTCOME_ACKNOWLEDGED, nil
	case 7:
		return history.SceneMemberOutcome_SCENE_MEMBER_OUTCOME_CONFIRMED, nil
	case 8:
		return history.SceneMemberOutcome_SCENE_MEMBER_OUTCOME_INDETERMINATE, nil
	default:
		return 0, historyquery.ErrIntegrity
	}
}
func historyScene(value uint8) (history.SceneOutcome, error) {
	switch value {
	case 1:
		return history.SceneOutcome_SCENE_OUTCOME_COMPLETE, nil
	case 2:
		return history.SceneOutcome_SCENE_OUTCOME_PARTIAL, nil
	case 3:
		return history.SceneOutcome_SCENE_OUTCOME_NONE, nil
	case 4:
		return history.SceneOutcome_SCENE_OUTCOME_INDETERMINATE, nil
	case 5:
		return history.SceneOutcome_SCENE_OUTCOME_NOT_ATTEMPTED, nil
	default:
		return 0, historyquery.ErrIntegrity
	}
}
func historyScript(value uint8) (history.ScriptMode, error) {
	switch value {
	case 1:
		return history.ScriptMode_SCRIPT_MODE_WAIT, nil
	case 2:
		return history.ScriptMode_SCRIPT_MODE_DETACHED, nil
	default:
		return 0, historyquery.ErrIntegrity
	}
}
func historyAction(value uint8) (obs.TraceActionKind, error) {
	switch value {
	case 1:
		return obs.TraceActionKind_TRACE_ACTION_KIND_VARIABLES, nil
	case 2:
		return obs.TraceActionKind_TRACE_ACTION_KIND_SEQUENCE, nil
	case 3:
		return obs.TraceActionKind_TRACE_ACTION_KIND_CHOOSE, nil
	case 4:
		return obs.TraceActionKind_TRACE_ACTION_KIND_IF, nil
	case 5:
		return obs.TraceActionKind_TRACE_ACTION_KIND_REPEAT, nil
	case 6:
		return obs.TraceActionKind_TRACE_ACTION_KIND_PARALLEL, nil
	case 7:
		return obs.TraceActionKind_TRACE_ACTION_KIND_WAIT, nil
	case 8:
		return obs.TraceActionKind_TRACE_ACTION_KIND_STOP, nil
	case 9:
		return obs.TraceActionKind_TRACE_ACTION_KIND_CALL, nil
	case 10:
		return obs.TraceActionKind_TRACE_ACTION_KIND_DELAY, nil
	case 11:
		return obs.TraceActionKind_TRACE_ACTION_KIND_WAIT_TEMPLATE, nil
	default:
		return 0, historyquery.ErrIntegrity
	}
}
func historyFailureKind(value uint8) (obs.TraceExecutionErrorKind, error) {
	switch value {
	case 1:
		return obs.TraceExecutionErrorKind_TRACE_EXECUTION_ERROR_KIND_INVALID_PROGRAM, nil
	case 2:
		return obs.TraceExecutionErrorKind_TRACE_EXECUTION_ERROR_KIND_WORK_BUDGET, nil
	case 3:
		return obs.TraceExecutionErrorKind_TRACE_EXECUTION_ERROR_KIND_RECURSION, nil
	case 4:
		return obs.TraceExecutionErrorKind_TRACE_EXECUTION_ERROR_KIND_WAIT_TIMEOUT, nil
	case 5:
		return obs.TraceExecutionErrorKind_TRACE_EXECUTION_ERROR_KIND_SOURCE_UNAVAILABLE, nil
	case 6:
		return obs.TraceExecutionErrorKind_TRACE_EXECUTION_ERROR_KIND_CONDITION_ERROR, nil
	case 7:
		return obs.TraceExecutionErrorKind_TRACE_EXECUTION_ERROR_KIND_COMMAND_ERROR, nil
	default:
		return 0, historyquery.ErrIntegrity
	}
}
func historyReset(value recordermaintenance.Reset) (history.Reset, error) {
	switch value {
	case recordermaintenance.ResetNotApplicable:
		return history.Reset_RESET_NOT_APPLICABLE, nil
	case recordermaintenance.ResetUnknown:
		return history.Reset_RESET_UNKNOWN, nil
	case recordermaintenance.ResetNotDetected:
		return history.Reset_RESET_NOT_DETECTED, nil
	case recordermaintenance.ResetInferred:
		return history.Reset_RESET_INFERRED, nil
	default:
		return 0, historyquery.ErrIntegrity
	}
}
func historyReasons(value recordermaintenance.Reason) ([]history.AggregateReason, error) {
	if !value.Valid() {
		return nil, historyquery.ErrIntegrity
	}
	var out []history.AggregateReason
	if value&recordermaintenance.Gap != 0 {
		out = append(out, history.AggregateReason_AGGREGATE_REASON_GAP)
	}
	if value&recordermaintenance.EpochBoundary != 0 {
		out = append(out, history.AggregateReason_AGGREGATE_REASON_EPOCH_BOUNDARY)
	}
	if value&recordermaintenance.RetentionPruned != 0 {
		out = append(out, history.AggregateReason_AGGREGATE_REASON_RETENTION_PRUNED)
	}
	if value&recordermaintenance.QualityBreak != 0 {
		out = append(out, history.AggregateReason_AGGREGATE_REASON_QUALITY_BREAK)
	}
	if value&recordermaintenance.UnitBreak != 0 {
		out = append(out, history.AggregateReason_AGGREGATE_REASON_UNIT_BREAK)
	}
	if value&recordermaintenance.ClockRegression != 0 {
		out = append(out, history.AggregateReason_AGGREGATE_REASON_CLOCK_REGRESSION)
	}
	if value&recordermaintenance.CounterDip != 0 {
		out = append(out, history.AggregateReason_AGGREGATE_REASON_COUNTER_DIP)
	}
	if value&recordermaintenance.UnknownBaseline != 0 {
		out = append(out, history.AggregateReason_AGGREGATE_REASON_UNKNOWN_BASELINE)
	}
	if value&recordermaintenance.BudgetLimit != 0 {
		out = append(out, history.AggregateReason_AGGREGATE_REASON_BUDGET_LIMIT)
	}
	if value&recordermaintenance.UnprovenTail != 0 {
		out = append(out, history.AggregateReason_AGGREGATE_REASON_UNPROVEN_TAIL)
	}
	if value&recordermaintenance.NonNumeric != 0 {
		out = append(out, history.AggregateReason_AGGREGATE_REASON_NON_NUMERIC)
	}
	if value&recordermaintenance.UnsupportedNumeric != 0 {
		out = append(out, history.AggregateReason_AGGREGATE_REASON_UNSUPPORTED_NUMERIC)
	}
	return out, nil
}
func historyLossReason(value storage.HistoryLossReason) (history.TraceLossReason, error) {
	switch value {
	case storage.HistoryLossQueueFull:
		return history.TraceLossReason_TRACE_LOSS_REASON_QUEUE_FULL, nil
	case storage.HistoryLossTooLarge:
		return history.TraceLossReason_TRACE_LOSS_REASON_TOO_LARGE, nil
	case storage.HistoryLossContention:
		return history.TraceLossReason_TRACE_LOSS_REASON_CONTENTION, nil
	case storage.HistoryLossInvalidMetadata:
		return history.TraceLossReason_TRACE_LOSS_REASON_INVALID_METADATA, nil
	default:
		return 0, historyquery.ErrIntegrity
	}
}
func historyTrace(record storage.HistoryRecord) (*history.ExecutionTrace, error) {
	if !record.Valid() {
		return nil, historyquery.ErrIntegrity
	}
	in := record.Input()
	point := &history.ExecutionTrace{Ordinal: in.ID.Ordinal(), EmissionTime: timestamppb.New(in.EmissionTime)}
	switch in.Kind {
	case storage.HistoryDecision:
		value, err := historyDecision(in.Decision)
		if err != nil {
			return nil, err
		}
		point.Outcome = &history.ExecutionTrace_Decision{Decision: value}
	case storage.HistoryNode:
		value, err := historyNode(in.NodeStage)
		if err != nil {
			return nil, err
		}
		point.Outcome = &history.ExecutionTrace_NodeStage{NodeStage: value}
	case storage.HistoryCommand:
		value, err := historyCommand(in.CommandOutcome)
		if err != nil {
			return nil, err
		}
		point.Outcome = &history.ExecutionTrace_CommandOutcome{CommandOutcome: value}
	case storage.HistorySceneMember:
		value, err := historyMember(in.SceneOutcome)
		if err != nil {
			return nil, err
		}
		point.Outcome = &history.ExecutionTrace_SceneMemberOutcome{SceneMemberOutcome: value}
	case storage.HistorySceneInvocation:
		value, err := historyScene(in.SceneAggregate)
		if err != nil {
			return nil, err
		}
		point.Outcome = &history.ExecutionTrace_SceneOutcome{SceneOutcome: value}
	case storage.HistoryScriptLink:
		value, err := historyScript(in.ScriptMode)
		if err != nil {
			return nil, err
		}
		point.Outcome = &history.ExecutionTrace_ScriptMode{ScriptMode: value}
	default:
		return nil, historyquery.ErrIntegrity
	}
	if in.RunID != 0 {
		point.RunId = proto.Uint64(in.RunID)
	}
	if in.ParentRunID != 0 {
		point.ParentRunId = proto.Uint64(in.ParentRunID)
	}
	if in.ControllerGeneration != 0 {
		point.ControllerGeneration = proto.Uint64(in.ControllerGeneration)
	}
	switch in.Provenance {
	case storage.HistoryUnboundFixture:
	case storage.HistoryBoundProgram:
		point.ConfigRevision = proto.Uint64(uint64(in.ConfigRevision))
		point.ManifestSha256 = append([]byte(nil), in.ManifestSHA256[:]...)
	default:
		return nil, historyquery.ErrIntegrity
	}
	if in.ActionKind != 0 {
		value, err := historyAction(in.ActionKind)
		if err != nil {
			return nil, err
		}
		point.ActionKind = &value
	}
	if in.FailureKind != 0 {
		value, err := historyFailureKind(in.FailureKind)
		if err != nil {
			return nil, err
		}
		point.FailureKind = &value
	}
	return point, nil
}
