package server

import (
	"context"
	"errors"
	"io"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
)

func coreActionError(ctx context.Context, err error, response *actions.CommandResponse) error {
	if terminal := authorizationTerminationError(ctx); terminal != nil {
		return terminal
	}
	if errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
		return coreRPCError(ctx, err)
	}
	kind := actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_UNSPECIFIED
	code := connect.CodeInternal
	switch {
	case errors.Is(err, integrations.ErrDurableCommitUnknown):
		kind, code = actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_COMMIT_OUTCOME_UNKNOWN, connect.CodeUnknown
	case errors.Is(err, integrations.ErrCommandOutcomeIndeterminate):
		kind, code = actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_OUTCOME_INDETERMINATE, connect.CodeUnknown
	case errors.Is(err, integrations.ErrDurableLedgerUnavailable):
		kind, code = actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_LEDGER_UNAVAILABLE, connect.CodeUnavailable
	case errors.Is(err, integrations.ErrCommandIDConflict):
		kind, code = actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_ID_CONFLICT, connect.CodeAlreadyExists
	case errors.Is(err, integrations.ErrCommandPolicyConflict):
		kind, code = actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_POLICY_CONFLICT, connect.CodeAlreadyExists
	case errors.Is(err, integrations.ErrCommandRouteBusy):
		kind, code = actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_ROUTE_BUSY, connect.CodeResourceExhausted
	case errors.Is(err, integrations.ErrCommandRouteUnavailable):
		kind, code = actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_ROUTE_UNAVAILABLE, connect.CodeUnavailable
	case errors.Is(err, integrations.ErrCommandPolicyLedgerFull), errors.Is(err, integrations.ErrDurableAdmissionCapacity), errors.Is(err, integrations.ErrActionProgressFull), errors.Is(err, integrations.ErrActionSubscribersFull):
		kind, code = actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_CAPACITY, connect.CodeResourceExhausted
	case errors.Is(err, integrations.ErrCommandIntentTooLarge):
		kind, code = actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_INTENT_TOO_LARGE, connect.CodeResourceExhausted
	case errors.Is(err, integrations.ErrActionSubscriberSlow):
		kind, code = actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_CLIENT_SLOW, connect.CodeResourceExhausted
	case errors.Is(err, integrations.ErrCommandResultUnavailable):
		kind, code = actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_RESULT_UNAVAILABLE, connect.CodeInternal
	case errors.Is(err, context.Canceled):
		kind, code = actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_CANCELLED, connect.CodeCanceled
	case errors.Is(err, context.DeadlineExceeded):
		kind, code = actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_DEADLINE, connect.CodeDeadlineExceeded
	case errors.Is(err, integrations.ErrInvalidInput):
		return coreRPCError(ctx, contracts.ErrInvalidActionRPC)
	}
	if kind == actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_UNSPECIFIED {
		return coreRPCError(ctx, err)
	}
	detail := &actions.ActionOperationFailure{Kind: kind, Response: contracts.ActionResponseCopy(response)}
	if contracts.AdmitActionOperationFailure(detail) != nil {
		return coreRPCError(ctx, contracts.ErrInvalidActionRPC)
	}
	public := connect.NewError(code, errors.New("action operation could not complete; consult the same command ID"))
	wire, detailErr := connect.NewErrorDetail(detail)
	if detailErr != nil {
		return coreRPCError(ctx, detailErr)
	}
	public.AddDetail(wire)
	return public
}

func (h *coreRPCHandler) SubmitAction(ctx context.Context, request *connect.Request[actions.SubmitActionRequest]) (*connect.Response[actions.SubmitActionResponse], error) {
	actor, err := coreRPCActor(ctx)
	if err != nil {
		return nil, err
	}
	if h == nil || h.commands == nil || request == nil || request.Msg == nil || contracts.AdmitSubmitActionRequest(request.Msg) != nil {
		return nil, coreRPCError(ctx, contracts.ErrInvalidActionRPC)
	}
	// This is the sole dispatch call. No reconnect, error mapping, status read
	// or stream close re-enters the command admission path.
	response, err := h.commands.Dispatch(ctx, actor, request.Msg.Command)
	if err != nil {
		return nil, coreActionError(ctx, err, response)
	}
	if terminal := authorizationTerminationError(ctx); terminal != nil {
		return nil, terminal
	}
	if response == nil {
		return nil, coreRPCError(ctx, contracts.ErrInvalidActionRPC)
	}
	return connect.NewResponse(&actions.SubmitActionResponse{Result: contracts.ActionResponseCopy(response)}), nil
}

func coreDurableResult(status integrations.DurableCommandStatus) (*actions.DurableActionResult, error) {
	result := &actions.DurableActionResult{Response: contracts.ActionResponseCopy(status.Response)}
	switch status.State {
	case storage.IntentAccepted:
		result.State = actions.DurableActionState_DURABLE_ACTION_STATE_ACCEPTED
	case storage.IntentDispatching:
		result.State = actions.DurableActionState_DURABLE_ACTION_STATE_DISPATCHING
	case storage.IntentResult:
		result.State = actions.DurableActionState_DURABLE_ACTION_STATE_RESULT
	case storage.IntentIndeterminate:
		result.State = actions.DurableActionState_DURABLE_ACTION_STATE_INDETERMINATE
	case storage.IntentUnknown:
		return nil, integrations.ErrDurableLedgerUnavailable
	default:
		return nil, integrations.ErrDurableLedgerUnavailable
	}
	switch status.ErrorKind {
	case storage.IntentNoError:
		result.Error = actions.DurableActionError_DURABLE_ACTION_ERROR_NONE
	case storage.IntentRouteBusy:
		result.Error = actions.DurableActionError_DURABLE_ACTION_ERROR_ROUTE_BUSY
	case storage.IntentRouteUnavailable:
		result.Error = actions.DurableActionError_DURABLE_ACTION_ERROR_ROUTE_UNAVAILABLE
	case storage.IntentCanceled:
		result.Error = actions.DurableActionError_DURABLE_ACTION_ERROR_CANCELLED
	case storage.IntentDeadline:
		result.Error = actions.DurableActionError_DURABLE_ACTION_ERROR_DEADLINE
	case storage.IntentOtherError:
		result.Error = actions.DurableActionError_DURABLE_ACTION_ERROR_OTHER
	case storage.IntentOutcomeIndeterminate:
		result.Error = actions.DurableActionError_DURABLE_ACTION_ERROR_OUTCOME_INDETERMINATE
	case storage.IntentErrorUnknown:
		return nil, integrations.ErrDurableLedgerUnavailable
	default:
		return nil, integrations.ErrDurableLedgerUnavailable
	}
	return result, nil
}

func (h *coreRPCHandler) progressWire(snapshot integrations.ActionSnapshot) (*actions.ActionProgress, error) {
	epoch, err := h.epoch.CurrentAdmissionEpoch()
	if err != nil {
		return nil, err
	}
	return &actions.ActionProgress{AdmissionEpoch: epoch, Status: snapshot.Status, Sequence: snapshot.Sequence, ConfigRevision: snapshot.ConfigRevision}, nil
}

func (h *coreRPCHandler) GetAction(ctx context.Context, request *connect.Request[actions.GetActionRequest]) (*connect.Response[actions.GetActionResponse], error) {
	if _, err := coreRPCActor(ctx); err != nil {
		return nil, err
	}
	if h == nil || h.commands == nil || request == nil {
		return nil, coreRPCError(ctx, contracts.ErrInvalidActionRPC)
	}
	id, err := contracts.AdmitGetActionRequest(request.Msg)
	if err != nil {
		return nil, coreRPCError(ctx, err)
	}
	read, err := h.commands.AuthorizeActionRead(ctx, id)
	if err != nil {
		return nil, coreActionError(ctx, err, nil)
	}
	defer read.Close()
	status, err := read.Durable()
	if err != nil {
		return nil, coreActionError(ctx, err, nil)
	}
	durable, err := coreDurableResult(status)
	if err != nil {
		return nil, coreActionError(ctx, err, nil)
	}
	result := &actions.GetActionResponse{Durable: durable}
	snapshot, err := read.Live()
	if errors.Is(err, integrations.ErrActionUnknown) {
		result.Live = &actions.GetActionResponse_Unavailable{Unavailable: &actions.NoLiveActionProgress{}}
	} else if err != nil {
		return nil, coreActionError(ctx, err, nil)
	} else {
		progress, err := h.progressWire(snapshot)
		if err != nil {
			return nil, coreRPCError(ctx, err)
		}
		result.Live = &actions.GetActionResponse_Progress{Progress: progress}
	}
	if !read.Ready() {
		return nil, coreActionError(ctx, integrations.ErrCommandAuthorizationDenied, nil)
	}
	if err := contracts.AdmitGetActionResponse(result); err != nil {
		return nil, coreRPCError(ctx, err)
	}
	return connect.NewResponse(result), nil
}

func (h *coreRPCHandler) WatchAction(ctx context.Context, request *connect.Request[actions.WatchActionRequest], stream *connect.ServerStream[actions.WatchActionResponse]) error {
	if _, err := coreRPCActor(ctx); err != nil {
		return err
	}
	if h == nil || h.commands == nil || request == nil || stream == nil {
		return coreRPCError(ctx, contracts.ErrInvalidActionRPC)
	}
	id, err := contracts.AdmitWatchActionRequest(request.Msg)
	if err != nil {
		return coreRPCError(ctx, err)
	}
	read, err := h.commands.AuthorizeActionRead(ctx, id)
	if err != nil {
		return coreActionError(ctx, err, nil)
	}
	defer read.Close()
	defer interruptCoreRPCWriteOnCancel(ctx, read.Context())()
	subscription, err := read.Subscribe()
	if errors.Is(err, integrations.ErrActionUnknown) {
		if !read.Ready() {
			return coreActionError(ctx, integrations.ErrCommandAuthorizationDenied, nil)
		}
		return stream.Send(&actions.WatchActionResponse{StreamSequence: 1, Frame: &actions.WatchActionResponse_Unavailable{Unavailable: &actions.NoLiveActionProgress{}}})
	}
	if err != nil {
		return coreActionError(ctx, err, nil)
	}
	defer subscription.Close()
	var sequence uint64
	for {
		snapshot, err := subscription.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return coreActionError(ctx, err, nil)
		}
		progress, err := h.progressWire(snapshot)
		if err != nil {
			return coreRPCError(ctx, err)
		}
		sequence++
		frame := &actions.WatchActionResponse{StreamSequence: sequence, Frame: &actions.WatchActionResponse_Progress{Progress: progress}}
		if err := contracts.AdmitWatchActionResponse(frame); err != nil {
			return coreRPCError(ctx, err)
		}
		if !read.Ready() {
			return coreActionError(ctx, integrations.ErrCommandAuthorizationDenied, nil)
		}
		if err := stream.Send(frame); err != nil {
			return coreActionError(ctx, err, nil)
		}
	}
}
