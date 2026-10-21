package contracts

import (
	"encoding/hex"
	"errors"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

var ErrInvalidActionRPC = errors.New("invalid action RPC contract")

func AdmitSubmitActionRequest(request *actions.SubmitActionRequest) error {
	if request == nil || request.Command == nil || !knownFieldsOnly(request.ProtoReflect()) {
		return ErrInvalidActionRPC
	}
	return nil
}

func AdmitGetActionRequest(request *actions.GetActionRequest) (domain.CommandID, error) {
	if request == nil || !knownFieldsOnly(request.ProtoReflect()) {
		return domain.CommandID{}, ErrInvalidActionRPC
	}
	id, err := domain.ParseCommandID(request.CommandId)
	if err != nil {
		return domain.CommandID{}, ErrInvalidActionRPC
	}
	return id, nil
}
func AdmitWatchActionRequest(request *actions.WatchActionRequest) (domain.CommandID, error) {
	if request == nil || !knownFieldsOnly(request.ProtoReflect()) {
		return domain.CommandID{}, ErrInvalidActionRPC
	}
	id, err := domain.ParseCommandID(request.CommandId)
	if err != nil {
		return domain.CommandID{}, ErrInvalidActionRPC
	}
	return id, nil
}

func validActionStatus(status actions.ActionStatus) bool {
	switch status {
	case actions.ActionStatus_ACTION_STATUS_ACCEPTED, actions.ActionStatus_ACTION_STATUS_DISPATCHED,
		actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED, actions.ActionStatus_ACTION_STATUS_CONFIRMED,
		actions.ActionStatus_ACTION_STATUS_FAILED, actions.ActionStatus_ACTION_STATUS_CANCELLED,
		actions.ActionStatus_ACTION_STATUS_INDETERMINATE:
		return true
	case actions.ActionStatus_ACTION_STATUS_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func validActionProgress(progress *actions.ActionProgress) bool {
	return progress != nil && knownFieldsOnly(progress.ProtoReflect()) && progress.Sequence > 0 &&
		progress.ConfigRevision > 0 && validActionStatus(progress.Status) && validActionEpoch(progress.AdmissionEpoch)
}
func validActionEpoch(text string) bool {
	if len(text) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(text)
	if err != nil {
		return false
	}
	for _, value := range decoded {
		if value != 0 {
			return true
		}
	}
	return false
}
func validDurableAction(result *actions.DurableActionResult) bool {
	if result == nil || !knownFieldsOnly(result.ProtoReflect()) || result.Response == nil || !validActionStatus(result.Response.Status) {
		return false
	}
	if _, err := domain.ParseCommandID(result.Response.CommandId); err != nil {
		return false
	}
	switch result.Error {
	case actions.DurableActionError_DURABLE_ACTION_ERROR_NONE,
		actions.DurableActionError_DURABLE_ACTION_ERROR_ROUTE_BUSY,
		actions.DurableActionError_DURABLE_ACTION_ERROR_ROUTE_UNAVAILABLE,
		actions.DurableActionError_DURABLE_ACTION_ERROR_CANCELLED,
		actions.DurableActionError_DURABLE_ACTION_ERROR_DEADLINE,
		actions.DurableActionError_DURABLE_ACTION_ERROR_OTHER,
		actions.DurableActionError_DURABLE_ACTION_ERROR_OUTCOME_INDETERMINATE:
	case actions.DurableActionError_DURABLE_ACTION_ERROR_UNSPECIFIED:
		return false
	default:
		return false
	}
	switch result.State {
	case actions.DurableActionState_DURABLE_ACTION_STATE_ACCEPTED:
		return result.Response.Status == actions.ActionStatus_ACTION_STATUS_ACCEPTED && result.Error == actions.DurableActionError_DURABLE_ACTION_ERROR_NONE
	case actions.DurableActionState_DURABLE_ACTION_STATE_DISPATCHING:
		return result.Response.Status == actions.ActionStatus_ACTION_STATUS_INDETERMINATE && result.Error == actions.DurableActionError_DURABLE_ACTION_ERROR_NONE
	case actions.DurableActionState_DURABLE_ACTION_STATE_RESULT:
		return result.Response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE
	case actions.DurableActionState_DURABLE_ACTION_STATE_INDETERMINATE:
		return result.Response.Status == actions.ActionStatus_ACTION_STATUS_INDETERMINATE
	case actions.DurableActionState_DURABLE_ACTION_STATE_UNSPECIFIED:
		return false
	default:
		return false
	}
}

// Unknown wire enum values remain decodable, but do not become admitted facts.
func AdmitGetActionResponse(response *actions.GetActionResponse) error {
	if response == nil || !knownFieldsOnly(response.ProtoReflect()) || !validDurableAction(response.Durable) {
		return ErrInvalidActionRPC
	}
	switch live := response.Live.(type) {
	case *actions.GetActionResponse_Progress:
		if live != nil && validActionProgress(live.Progress) {
			return nil
		}
	case *actions.GetActionResponse_Unavailable:
		if live != nil && live.Unavailable != nil {
			return nil
		}
	}
	return ErrInvalidActionRPC
}
func AdmitWatchActionResponse(response *actions.WatchActionResponse) error {
	if response == nil || !knownFieldsOnly(response.ProtoReflect()) || response.StreamSequence == 0 {
		return ErrInvalidActionRPC
	}
	switch frame := response.Frame.(type) {
	case *actions.WatchActionResponse_Progress:
		if frame != nil && validActionProgress(frame.Progress) {
			return nil
		}
	case *actions.WatchActionResponse_Unavailable:
		if frame != nil && frame.Unavailable != nil && response.StreamSequence == 1 {
			return nil
		}
	}
	return ErrInvalidActionRPC
}
func AdmitActionOperationFailure(detail *actions.ActionOperationFailure) error {
	if detail == nil || !knownFieldsOnly(detail.ProtoReflect()) {
		return ErrInvalidActionRPC
	}
	if detail.Response != nil && !validActionStatus(detail.Response.Status) {
		return ErrInvalidActionRPC
	}
	switch detail.Kind {
	case actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_LEDGER_UNAVAILABLE,
		actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_COMMIT_OUTCOME_UNKNOWN,
		actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_OUTCOME_INDETERMINATE,
		actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_ID_CONFLICT,
		actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_POLICY_CONFLICT,
		actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_ROUTE_BUSY,
		actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_ROUTE_UNAVAILABLE,
		actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_CAPACITY,
		actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_CANCELLED,
		actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_DEADLINE,
		actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_RESULT_UNAVAILABLE,
		actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_INTENT_TOO_LARGE,
		actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_CLIENT_SLOW:
		return nil
	case actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_UNSPECIFIED:
		return ErrInvalidActionRPC
	default:
		return ErrInvalidActionRPC
	}
}

// ActionResponseCopy owns the response before it crosses the public boundary.
func ActionResponseCopy(response *actions.CommandResponse) *actions.CommandResponse {
	if response == nil {
		return nil
	}
	return proto.Clone(response).(*actions.CommandResponse)
}
