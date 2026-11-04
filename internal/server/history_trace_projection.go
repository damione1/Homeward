package server

import (
	"encoding/hex"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"strconv"
)

func publicHistoryDecision(value uint8) (api.RecorderTraceDecision, error) {
	switch value {
	case 1:
		return api.RecorderTraceDecision_RECORDER_TRACE_DECISION_PREPARING, nil
	case 2:
		return api.RecorderTraceDecision_RECORDER_TRACE_DECISION_ADMITTED, nil
	case 3:
		return api.RecorderTraceDecision_RECORDER_TRACE_DECISION_QUEUED, nil
	case 4:
		return api.RecorderTraceDecision_RECORDER_TRACE_DECISION_RESTART_PENDING, nil
	case 5:
		return api.RecorderTraceDecision_RECORDER_TRACE_DECISION_SINGLE_BUSY, nil
	case 6:
		return api.RecorderTraceDecision_RECORDER_TRACE_DECISION_MAX_EXCEEDED, nil
	case 7:
		return api.RecorderTraceDecision_RECORDER_TRACE_DECISION_PREPARATION_SATURATED, nil
	case 8:
		return api.RecorderTraceDecision_RECORDER_TRACE_DECISION_MEMORY_SATURATED, nil
	case 9:
		return api.RecorderTraceDecision_RECORDER_TRACE_DECISION_CONDITION_FALSE, nil
	case 10:
		return api.RecorderTraceDecision_RECORDER_TRACE_DECISION_PREPARATION_ERROR, nil
	case 11:
		return api.RecorderTraceDecision_RECORDER_TRACE_DECISION_EXECUTION_ERROR, nil
	case 12:
		return api.RecorderTraceDecision_RECORDER_TRACE_DECISION_FINISHED, nil
	case 13:
		return api.RecorderTraceDecision_RECORDER_TRACE_DECISION_CANCELLED, nil
	case 14:
		return api.RecorderTraceDecision_RECORDER_TRACE_DECISION_SUPERSEDED, nil
	case 15:
		return api.RecorderTraceDecision_RECORDER_TRACE_DECISION_CLOSED, nil
	case 16:
		return api.RecorderTraceDecision_RECORDER_TRACE_DECISION_UNKNOWN_AUTOMATION, nil
	case 17:
		return api.RecorderTraceDecision_RECORDER_TRACE_DECISION_TRIGGER_MISSED, nil
	case 18:
		return api.RecorderTraceDecision_RECORDER_TRACE_DECISION_TRIGGER_DEGRADED, nil
	default:
		return 0, historyquery.ErrIntegrity
	}
}
func publicHistoryNode(value uint8) (api.RecorderTraceNodeStage, error) {
	switch value {
	case 1:
		return api.RecorderTraceNodeStage_RECORDER_TRACE_NODE_STAGE_ENTERED, nil
	case 2:
		return api.RecorderTraceNodeStage_RECORDER_TRACE_NODE_STAGE_COMPLETED, nil
	case 3:
		return api.RecorderTraceNodeStage_RECORDER_TRACE_NODE_STAGE_STOPPED, nil
	case 4:
		return api.RecorderTraceNodeStage_RECORDER_TRACE_NODE_STAGE_CANCELLED, nil
	case 5:
		return api.RecorderTraceNodeStage_RECORDER_TRACE_NODE_STAGE_FAILED, nil
	case 6:
		return api.RecorderTraceNodeStage_RECORDER_TRACE_NODE_STAGE_WAITING, nil
	default:
		return 0, historyquery.ErrIntegrity
	}
}
func publicHistoryCommand(value uint8) (api.HistoryCommandOutcome, error) {
	switch value {
	case 1:
		return api.HistoryCommandOutcome_HISTORY_COMMAND_OUTCOME_ACCEPTED, nil
	case 2:
		return api.HistoryCommandOutcome_HISTORY_COMMAND_OUTCOME_DISPATCHED, nil
	case 3:
		return api.HistoryCommandOutcome_HISTORY_COMMAND_OUTCOME_ACKNOWLEDGED, nil
	case 4:
		return api.HistoryCommandOutcome_HISTORY_COMMAND_OUTCOME_CONFIRMED, nil
	case 5:
		return api.HistoryCommandOutcome_HISTORY_COMMAND_OUTCOME_INDETERMINATE, nil
	case 6:
		return api.HistoryCommandOutcome_HISTORY_COMMAND_OUTCOME_REFUSED, nil
	case 7:
		return api.HistoryCommandOutcome_HISTORY_COMMAND_OUTCOME_NOT_ATTEMPTED, nil
	default:
		return 0, historyquery.ErrIntegrity
	}
}
func publicHistoryMember(value uint8) (api.HistorySceneMemberOutcome, error) {
	switch value {
	case 1:
		return api.HistorySceneMemberOutcome_HISTORY_SCENE_MEMBER_OUTCOME_PREPARED, nil
	case 2:
		return api.HistorySceneMemberOutcome_HISTORY_SCENE_MEMBER_OUTCOME_ALREADY_SATISFIED, nil
	case 3:
		return api.HistorySceneMemberOutcome_HISTORY_SCENE_MEMBER_OUTCOME_REFUSED, nil
	case 4:
		return api.HistorySceneMemberOutcome_HISTORY_SCENE_MEMBER_OUTCOME_ACCEPTED, nil
	case 5:
		return api.HistorySceneMemberOutcome_HISTORY_SCENE_MEMBER_OUTCOME_DISPATCHED, nil
	case 6:
		return api.HistorySceneMemberOutcome_HISTORY_SCENE_MEMBER_OUTCOME_ACKNOWLEDGED, nil
	case 7:
		return api.HistorySceneMemberOutcome_HISTORY_SCENE_MEMBER_OUTCOME_CONFIRMED, nil
	case 8:
		return api.HistorySceneMemberOutcome_HISTORY_SCENE_MEMBER_OUTCOME_INDETERMINATE, nil
	default:
		return 0, historyquery.ErrIntegrity
	}
}
func publicHistoryScene(value uint8) (api.HistorySceneOutcome, error) {
	switch value {
	case 1:
		return api.HistorySceneOutcome_HISTORY_SCENE_OUTCOME_COMPLETE, nil
	case 2:
		return api.HistorySceneOutcome_HISTORY_SCENE_OUTCOME_PARTIAL, nil
	case 3:
		return api.HistorySceneOutcome_HISTORY_SCENE_OUTCOME_NONE, nil
	case 4:
		return api.HistorySceneOutcome_HISTORY_SCENE_OUTCOME_INDETERMINATE, nil
	case 5:
		return api.HistorySceneOutcome_HISTORY_SCENE_OUTCOME_NOT_ATTEMPTED, nil
	default:
		return 0, historyquery.ErrIntegrity
	}
}
func publicHistoryScript(value uint8) (api.HistoryScriptMode, error) {
	switch value {
	case 1:
		return api.HistoryScriptMode_HISTORY_SCRIPT_MODE_WAIT, nil
	case 2:
		return api.HistoryScriptMode_HISTORY_SCRIPT_MODE_DETACHED, nil
	default:
		return 0, historyquery.ErrIntegrity
	}
}
func publicHistoryAction(value uint8) (api.RecorderTraceActionKind, error) {
	switch value {
	case 1:
		return api.RecorderTraceActionKind_RECORDER_TRACE_ACTION_KIND_VARIABLES, nil
	case 2:
		return api.RecorderTraceActionKind_RECORDER_TRACE_ACTION_KIND_SEQUENCE, nil
	case 3:
		return api.RecorderTraceActionKind_RECORDER_TRACE_ACTION_KIND_CHOOSE, nil
	case 4:
		return api.RecorderTraceActionKind_RECORDER_TRACE_ACTION_KIND_IF, nil
	case 5:
		return api.RecorderTraceActionKind_RECORDER_TRACE_ACTION_KIND_REPEAT, nil
	case 6:
		return api.RecorderTraceActionKind_RECORDER_TRACE_ACTION_KIND_PARALLEL, nil
	case 7:
		return api.RecorderTraceActionKind_RECORDER_TRACE_ACTION_KIND_WAIT, nil
	case 8:
		return api.RecorderTraceActionKind_RECORDER_TRACE_ACTION_KIND_STOP, nil
	case 9:
		return api.RecorderTraceActionKind_RECORDER_TRACE_ACTION_KIND_CALL, nil
	case 10:
		return api.RecorderTraceActionKind_RECORDER_TRACE_ACTION_KIND_DELAY, nil
	case 11:
		return api.RecorderTraceActionKind_RECORDER_TRACE_ACTION_KIND_WAIT_TEMPLATE, nil
	default:
		return 0, historyquery.ErrIntegrity
	}
}
func publicHistoryFailureKind(value uint8) (api.RecorderTraceExecutionErrorKind, error) {
	switch value {
	case 1:
		return api.RecorderTraceExecutionErrorKind_RECORDER_TRACE_EXECUTION_ERROR_KIND_INVALID_PROGRAM, nil
	case 2:
		return api.RecorderTraceExecutionErrorKind_RECORDER_TRACE_EXECUTION_ERROR_KIND_WORK_BUDGET, nil
	case 3:
		return api.RecorderTraceExecutionErrorKind_RECORDER_TRACE_EXECUTION_ERROR_KIND_RECURSION, nil
	case 4:
		return api.RecorderTraceExecutionErrorKind_RECORDER_TRACE_EXECUTION_ERROR_KIND_WAIT_TIMEOUT, nil
	case 5:
		return api.RecorderTraceExecutionErrorKind_RECORDER_TRACE_EXECUTION_ERROR_KIND_SOURCE_UNAVAILABLE, nil
	case 6:
		return api.RecorderTraceExecutionErrorKind_RECORDER_TRACE_EXECUTION_ERROR_KIND_CONDITION_ERROR, nil
	case 7:
		return api.RecorderTraceExecutionErrorKind_RECORDER_TRACE_EXECUTION_ERROR_KIND_COMMAND_ERROR, nil
	default:
		return 0, historyquery.ErrIntegrity
	}
}
func publicHistoryLossReason(value storage.HistoryLossReason) (api.HistoryTraceLossReason, error) {
	switch value {
	case storage.HistoryLossQueueFull:
		return api.HistoryTraceLossReason_HISTORY_TRACE_LOSS_REASON_QUEUE_FULL, nil
	case storage.HistoryLossTooLarge:
		return api.HistoryTraceLossReason_HISTORY_TRACE_LOSS_REASON_TOO_LARGE, nil
	case storage.HistoryLossContention:
		return api.HistoryTraceLossReason_HISTORY_TRACE_LOSS_REASON_CONTENTION, nil
	case storage.HistoryLossInvalidMetadata:
		return api.HistoryTraceLossReason_HISTORY_TRACE_LOSS_REASON_INVALID_METADATA, nil
	default:
		return 0, historyquery.ErrIntegrity
	}
}
func publicHistoryTrace(home apitypes.HomeName, record storage.HistoryRecord) (*api.HistoryExecutionTrace, error) {
	if !record.Valid() {
		return nil, historyquery.ErrIntegrity
	}
	in := record.Input()
	point := &api.HistoryExecutionTrace{Ordinal: in.ID.Ordinal(), EmissionTime: timestamppb.New(in.EmissionTime)}
	switch in.Kind {
	case storage.HistoryDecision:
		value, err := publicHistoryDecision(in.Decision)
		if err != nil {
			return nil, err
		}
		point.Outcome = &api.HistoryExecutionTrace_Decision{Decision: value}
	case storage.HistoryNode:
		value, err := publicHistoryNode(in.NodeStage)
		if err != nil {
			return nil, err
		}
		point.Outcome = &api.HistoryExecutionTrace_NodeStage{NodeStage: value}
	case storage.HistoryCommand:
		value, err := publicHistoryCommand(in.CommandOutcome)
		if err != nil {
			return nil, err
		}
		point.Outcome = &api.HistoryExecutionTrace_CommandOutcome{CommandOutcome: value}
	case storage.HistorySceneMember:
		value, err := publicHistoryMember(in.SceneOutcome)
		if err != nil {
			return nil, err
		}
		point.Outcome = &api.HistoryExecutionTrace_SceneMemberOutcome{SceneMemberOutcome: value}
	case storage.HistorySceneInvocation:
		value, err := publicHistoryScene(in.SceneAggregate)
		if err != nil {
			return nil, err
		}
		point.Outcome = &api.HistoryExecutionTrace_SceneOutcome{SceneOutcome: value}
	case storage.HistoryScriptLink:
		value, err := publicHistoryScript(in.ScriptMode)
		if err != nil {
			return nil, err
		}
		point.Outcome = &api.HistoryExecutionTrace_ScriptMode{ScriptMode: value}
	default:
		return nil, historyquery.ErrIntegrity
	}
	rawEpoch, err := hex.DecodeString(in.ID.Epoch().String())
	if err != nil || len(rawEpoch) != 16 || !home.Valid() {
		return nil, historyquery.ErrIntegrity
	}
	var epoch [16]byte
	copy(epoch[:], rawEpoch)
	if in.RunID != 0 {
		name, err := apitypes.HistoryRunNameForIdentity(home, epoch, in.RunID)
		if err != nil {
			return nil, err
		}
		point.Run = proto.String(name.String())
	}
	if in.ParentRunID != 0 {
		name, err := apitypes.HistoryRunNameForIdentity(home, epoch, in.ParentRunID)
		if err != nil {
			return nil, err
		}
		point.ParentRun = proto.String(name.String())
	}
	if in.ControllerGeneration != 0 {
		name, err := apitypes.HistoryControllerNameForIdentity(home, epoch, in.ControllerGeneration)
		if err != nil {
			return nil, err
		}
		point.Controller = proto.String(name.String())
	}

	switch in.Provenance {
	case storage.HistoryUnboundFixture:
	case storage.HistoryBoundProgram:
		point.ConfigurationGeneration = proto.String(home.String() + "/configurationGenerations/g-" + strconv.FormatInt(int64(in.ConfigRevision), 10))
		point.ManifestSha256 = append([]byte(nil), in.ManifestSHA256[:]...)
	default:
		return nil, historyquery.ErrIntegrity
	}
	if in.ActionKind != 0 {
		value, err := publicHistoryAction(in.ActionKind)
		if err != nil {
			return nil, err
		}
		point.ActionKind = &value
	}
	if in.FailureKind != 0 {
		value, err := publicHistoryFailureKind(in.FailureKind)
		if err != nil {
			return nil, err
		}
		point.FailureKind = &value
	}
	return point, nil
}
