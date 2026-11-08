package server

import (
	"context"
	"errors"
	"strings"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (h *coreRPCHandler) GetOperation(ctx context.Context, name apitypes.OperationName) (*longrunningpb.Operation, error) {
	if h == nil || h.home == nil || h.commands == nil || !name.Valid() {
		return nil, authorization.ErrDenied
	}
	home, err := h.home.owner.HomeName(ctx)
	if err != nil || name.Home() != home {
		return nil, authorization.ErrDenied
	}
	id, err := domain.ParseCommandID(strings.TrimPrefix(name.ID(), "cmd-"))
	if err != nil || !strings.HasPrefix(name.ID(), "cmd-") {
		return nil, authorization.ErrDenied
	}
	read, err := h.commands.AuthorizeActionRead(ctx, id)
	if err != nil {
		return nil, err
	}
	retained := false
	defer func() {
		if !retained {
			read.Close()
		}
	}()
	status, err := read.Durable()
	if err != nil {
		return nil, err
	}
	if !status.HasMetadata || status.Metadata.Capability().Entity().Home() != home {
		return nil, authorization.ErrDenied
	}
	// The same permit covers an object-scoped reconciliation fault as well as
	// a successful operation response, through serialization and buffered send.
	if !read.Ready() {
		return nil, authorization.ErrDenied
	}
	retained, err = retainPublicResponse(ctx, read.Context(), read.Ready, read.Close)
	if err != nil {
		return nil, err
	}
	operation, err := physicalOperation(name, status)
	if err != nil {
		return nil, err
	}
	if !operation.Done {
		snapshot, liveErr := read.Live()
		if liveErr == nil {
			metadata := &api.CommandMetadata{}
			if operation.Metadata.UnmarshalTo(metadata) != nil {
				return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
			}
			metadata.LiveProgressAvailable = true
			// Live observations refine nonterminal progress only. A terminal durable
			// ACK is never rewritten into a confirmation from an ephemeral cache.
			if snapshot.Status == actions.ActionStatus_ACTION_STATUS_DISPATCHED && metadata.Phase != api.CommandPhase_COMMAND_PHASE_ACKNOWLEDGED {
				metadata.Phase = api.CommandPhase_COMMAND_PHASE_DISPATCHED
			}
			if snapshot.Status == actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED {
				metadata.Phase = api.CommandPhase_COMMAND_PHASE_ACKNOWLEDGED
			}
			operation.Metadata, err = anypb.New(metadata)
			if err != nil {
				return nil, err
			}
		} else if !errors.Is(liveErr, integrations.ErrActionUnknown) {
			return nil, liveErr
		}
	}
	if !read.Ready() {
		return nil, authorization.ErrDenied
	}
	return operation, nil
}

func physicalOperation(name apitypes.OperationName, status integrations.DurableCommandStatus) (*longrunningpb.Operation, error) {
	if !status.HasMetadata || status.Response == nil || status.AcceptedAt.IsZero() {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
	}
	metadata := &api.CommandMetadata{Capability: status.Metadata.Capability().String(), CreateTime: timestamppb.New(status.AcceptedAt)}
	if metadata.CreateTime.CheckValid() != nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
	}
	if key, present := status.Metadata.RequestID(); present {
		metadata.RequestId = key.String()
	}
	operation := &longrunningpb.Operation{Name: name.String()}
	var failure error
	var outcome api.CommandOutcome
	switch status.State {
	case storage.IntentAccepted:
		metadata.Phase = api.CommandPhase_COMMAND_PHASE_ACCEPTED
	case storage.IntentDispatching:
		metadata.Phase = api.CommandPhase_COMMAND_PHASE_DISPATCHED
	case storage.IntentIndeterminate:
		operation.Done = true
		metadata.Phase = api.CommandPhase_COMMAND_PHASE_INDETERMINATE
		failure = apierrors.New(api.ApiErrorReason_API_ERROR_REASON_EFFECT_OUTCOME_UNKNOWN).WithOperation(name)
	case storage.IntentResult:
		response := status.Response
		if status.APIAcknowledged != nil {
			response = status.APIAcknowledged
		}
		if status.APITerminal != nil {
			response = status.APITerminal
		}
		if status.APITerminal == nil && (response.Status == actions.ActionStatus_ACTION_STATUS_ACCEPTED || response.Status == actions.ActionStatus_ACTION_STATUS_DISPATCHED || response.Status == actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED) {
			metadata.Phase = api.CommandPhase_COMMAND_PHASE_DISPATCHED
			if response.Status == actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED {
				metadata.Phase = api.CommandPhase_COMMAND_PHASE_ACKNOWLEDGED
			}
			break
		}
		operation.Done = true
		switch response.Status {
		case actions.ActionStatus_ACTION_STATUS_SENT_UNVERIFIED:
			metadata.Phase = api.CommandPhase_COMMAND_PHASE_DISPATCHED
			outcome = api.CommandOutcome_COMMAND_OUTCOME_SENT_UNVERIFIED
		case actions.ActionStatus_ACTION_STATUS_CONFIRMED:
			metadata.Phase = api.CommandPhase_COMMAND_PHASE_CONFIRMED
			outcome = api.CommandOutcome_COMMAND_OUTCOME_CONFIRMED
		case actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED:
			metadata.Phase = api.CommandPhase_COMMAND_PHASE_ACKNOWLEDGED
			outcome = api.CommandOutcome_COMMAND_OUTCOME_ACKNOWLEDGED_UNVERIFIED
		case actions.ActionStatus_ACTION_STATUS_CANCELLED:
			metadata.Phase = api.CommandPhase_COMMAND_PHASE_CANCELLED
			failure = apierrors.New(api.ApiErrorReason_API_ERROR_REASON_REQUEST_CANCELLED)
		case actions.ActionStatus_ACTION_STATUS_FAILED:
			metadata.Phase = api.CommandPhase_COMMAND_PHASE_FAILED
			failure = nativeContractFault(response.Error)
		case actions.ActionStatus_ACTION_STATUS_INDETERMINATE, actions.ActionStatus_ACTION_STATUS_DISPATCHED, actions.ActionStatus_ACTION_STATUS_ACCEPTED:
			metadata.Phase = api.CommandPhase_COMMAND_PHASE_INDETERMINATE
			failure = apierrors.New(api.ApiErrorReason_API_ERROR_REASON_EFFECT_OUTCOME_UNKNOWN).WithOperation(name)
		default:
			return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
		}
		if status.ErrorKind != storage.IntentNoError && failure == nil {
			return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
		}
	default:
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
	}
	// A finished native occurrence whose final SQL attempts failed is no
	// longer progressing. This read fails honestly; it does not manufacture a
	// terminal Operation from volatile status. A persisted terminal above wins.
	if !operation.Done && status.APIReconciliationUnavailable {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_EFFECT_OUTCOME_UNKNOWN).WithOperation(name)
	}
	if failure != nil {
		operation.Result = &longrunningpb.Operation_Error{Error: apiFault(failure).Status()}
	}
	if outcome != 0 {
		result, err := anypb.New(&api.ExecuteCapabilityResponse{Capability: metadata.Capability, Outcome: outcome})
		if err != nil {
			return nil, err
		}
		operation.Result = &longrunningpb.Operation_Response{Response: result}
	}
	var err error
	operation.Metadata, err = anypb.New(metadata)
	return operation, err
}

func (h *coreRPCHandler) ListOperationNames(ctx context.Context, home apitypes.HomeName, after *apitypes.OperationName, pageSize uint32, cutoff time.Time) ([]apitypes.OperationName, *apitypes.OperationName, error) {
	if h == nil || h.home == nil || h.commands == nil {
		return nil, nil, authorization.ErrDenied
	}
	current, err := h.home.owner.HomeName(ctx)
	if err != nil || current != home {
		return nil, nil, authorization.ErrDenied
	}
	actor, err := coreRPCActor(ctx)
	if err != nil {
		return nil, nil, err
	}
	return h.commands.APICommandNames(ctx, actor, home, after, pageSize, cutoff)
}
func (h *coreRPCHandler) FindOperationsByRequest(ctx context.Context, home apitypes.HomeName, key apitypes.RequestID, cutoff time.Time) ([]apitypes.OperationName, error) {
	if h == nil || h.home == nil || h.commands == nil {
		return nil, authorization.ErrDenied
	}
	current, err := h.home.owner.HomeName(ctx)
	if err != nil || current != home {
		return nil, authorization.ErrDenied
	}
	actor, err := coreRPCActor(ctx)
	if err != nil {
		return nil, err
	}
	return h.commands.APICommandsByRequest(ctx, actor, home, key, cutoff)
}
