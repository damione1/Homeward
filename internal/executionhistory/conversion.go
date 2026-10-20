package executionhistory

import (
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/scenes"
	"github.com/damione1/Homeward/internal/scriptcommands"
	"github.com/damione1/Homeward/internal/storage"
)

// These switches are the sole conversion from producer enums to migration
// 20's admitted numeric sets. Unknown Go values never become history rows.
func decisionCode(value automationruns.Decision) (uint8, bool) {
	switch value {
	case automationruns.DecisionPreparing:
		return 1, true
	case automationruns.DecisionAdmitted:
		return 2, true
	case automationruns.DecisionQueued:
		return 3, true
	case automationruns.DecisionRestartPending:
		return 4, true
	case automationruns.DecisionSingleBusy:
		return 5, true
	case automationruns.DecisionMaxExceeded:
		return 6, true
	case automationruns.DecisionPreparationSaturated:
		return 7, true
	case automationruns.DecisionMemorySaturated:
		return 8, true
	case automationruns.DecisionConditionFalse:
		return 9, true
	case automationruns.DecisionPreparationError:
		return 10, true
	case automationruns.DecisionExecutionError:
		return 11, true
	case automationruns.DecisionFinished:
		return 12, true
	case automationruns.DecisionCancelled:
		return 13, true
	case automationruns.DecisionSuperseded:
		return 14, true
	case automationruns.DecisionClosed:
		return 15, true
	case automationruns.DecisionUnknownAutomation:
		return 16, true
	case automationruns.DecisionTriggerMissed:
		return 17, true
	case automationruns.DecisionTriggerDegraded:
		return 18, true
	default:
		return 0, false
	}
}

func nodeStageCode(value automationruns.NodeStage) (uint8, bool) {
	switch value {
	case automationruns.NodeEntered:
		return 1, true
	case automationruns.NodeCompleted:
		return 2, true
	case automationruns.NodeStopped:
		return 3, true
	case automationruns.NodeCancelled:
		return 4, true
	case automationruns.NodeFailed:
		return 5, true
	case automationruns.NodeWaiting:
		return 6, true
	default:
		return 0, false
	}
}

func actionCode(value ir.ActionKind) (uint8, bool) {
	switch value {
	case ir.ActionVariables:
		return 1, true
	case ir.ActionSequence:
		return 2, true
	case ir.ActionChoose:
		return 3, true
	case ir.ActionIf:
		return 4, true
	case ir.ActionRepeat:
		return 5, true
	case ir.ActionParallel:
		return 6, true
	case ir.ActionWait:
		return 7, true
	case ir.ActionStop:
		return 8, true
	case ir.ActionCall:
		return 9, true
	case ir.ActionDelay:
		return 10, true
	case ir.ActionWaitTemplate:
		return 11, true
	default:
		return 0, false
	}
}

func failureCode(value automationruns.ExecutionErrorKind) (uint8, bool) {
	switch value {
	case automationruns.ExecutionErrorInvalid:
		return 0, true
	case automationruns.ExecutionInvalidProgram:
		return 1, true
	case automationruns.ExecutionWorkBudget:
		return 2, true
	case automationruns.ExecutionRecursion:
		return 3, true
	case automationruns.ExecutionWaitTimeout:
		return 4, true
	case automationruns.ExecutionSourceUnavailable:
		return 5, true
	case automationruns.ExecutionConditionError:
		return 6, true
	case automationruns.ExecutionCommandError:
		return 7, true
	default:
		return 0, false
	}
}

func waitCode(value automationruns.WaitExit) (uint8, bool) {
	switch value {
	case automationruns.WaitExitInvalid:
		return 0, true
	case automationruns.WaitCompleted:
		return 1, true
	case automationruns.WaitContinuedAfterTimeout:
		return 2, true
	case automationruns.WaitStoppedByTimeout:
		return 3, true
	case automationruns.WaitCancelled:
		return 4, true
	case automationruns.WaitSourceFailed:
		return 5, true
	default:
		return 0, false
	}
}

func scriptModeCode(value ir.ScriptCallMode) (uint8, bool) {
	switch value {
	case ir.ScriptCallDirect:
		return 1, true
	case ir.ScriptCallDetached:
		return 2, true
	default:
		return 0, false
	}
}

func commandOutcomeCode(value scriptcommands.CommandOutcome) (uint8, bool) {
	switch value {
	case scriptcommands.CommandAccepted:
		return 1, true
	case scriptcommands.CommandDispatched:
		return 2, true
	case scriptcommands.CommandAcknowledged:
		return 3, true
	case scriptcommands.CommandConfirmed:
		return 4, true
	case scriptcommands.CommandIndeterminate:
		return 5, true
	case scriptcommands.CommandRefused:
		return 6, true
	case scriptcommands.CommandNotAttempted:
		return 7, true
	default:
		return 0, false
	}
}

func sceneMemberCode(value scenes.MemberOutcome) (uint8, bool) {
	switch value {
	case scenes.OutcomePrepared:
		return 1, true
	case scenes.OutcomeAlreadySatisfied:
		return 2, true
	case scenes.OutcomeRefused:
		return 3, true
	case scenes.OutcomeAccepted:
		return 4, true
	case scenes.OutcomeDispatched:
		return 5, true
	case scenes.OutcomeAcknowledged:
		return 6, true
	case scenes.OutcomeConfirmed:
		return 7, true
	case scenes.OutcomeIndeterminate:
		return 8, true
	default:
		return 0, false
	}
}

func sceneInvocationCode(value scenes.SceneInvocationOutcome) (uint8, bool) {
	switch value {
	case scenes.SceneInvocationComplete:
		return 1, true
	case scenes.SceneInvocationPartial:
		return 2, true
	case scenes.SceneInvocationNone:
		return 3, true
	case scenes.SceneInvocationIndeterminate:
		return 4, true
	case scenes.SceneInvocationNotAttempted:
		return 5, true
	default:
		return 0, false
	}
}

func provenance(stamp automationruns.DefinitionProvenance) (storage.HistoryProvenanceKind, int64, [32]byte, bool) {
	if !stamp.Valid() {
		return 0, 0, [32]byte{}, false
	}
	switch stamp.Kind() {
	case automationruns.UnboundFixturePlan:
		return storage.HistoryUnboundFixture, 0, [32]byte{}, true
	case automationruns.BoundProgram:
		revision, hash, ok := stamp.Bound()
		if !ok {
			return 0, 0, [32]byte{}, false
		}
		return storage.HistoryBoundProgram, revision.Number(), hash, true
	default:
		return 0, 0, [32]byte{}, false
	}
}

func runFields(in *storage.HistoryRecordInput, run automationruns.HistoryRunRef) bool {
	kind, revision, hash, ok := provenance(run.Provenance)
	if !ok {
		return false
	}
	in.ControllerGeneration = uint64(run.Generation)
	in.RunID = uint64(run.RunID)
	in.ParentRunID = uint64(run.ParentID)
	in.Provenance, in.ConfigRevision, in.ManifestSHA256 = kind, revision, hash
	return true
}

func sourceFields(in *storage.HistoryRecordInput, source automationruns.HistorySource) {
	in.ObjectID, in.SourceFile, in.SourcePath = source.Object, source.File, source.Path
	in.SourceLine, in.SourceColumn = source.Line, source.Column
}

func validTime(t time.Time) bool { return !t.IsZero() && t.Year() >= 2000 && t.Year() <= 9999 }
