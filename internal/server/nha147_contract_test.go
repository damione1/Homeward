package server

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
)

func nha147ID(t *testing.T, text string) domain.CommandID {
	t.Helper()
	id, err := domain.ParseCommandID(text)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestNHA147ConnectFailurePreservesIndeterminateResponse(t *testing.T) {
	for _, tc := range []struct {
		cause    error
		kind     actions.ActionOperationFailureKind
		response bool
	}{
		{integrations.ErrDurableCommitUnknown, actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_COMMIT_OUTCOME_UNKNOWN, false},
		{integrations.ErrCommandOutcomeIndeterminate, actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_OUTCOME_INDETERMINATE, true},
		{integrations.ErrDurableLedgerUnavailable, actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_LEDGER_UNAVAILABLE, false},
		{integrations.ErrCommandIDConflict, actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_ID_CONFLICT, false},
	} {
		var response *actions.CommandResponse
		if tc.response {
			response = &actions.CommandResponse{CommandId: "nha147-kept-result", Status: actions.ActionStatus_ACTION_STATUS_INDETERMINATE}
		}
		public := coreActionError(context.Background(), tc.cause, response)
		var connectErr *connect.Error
		if !errors.As(public, &connectErr) || len(connectErr.Details()) != 1 {
			t.Fatal("machine detail missing", public)
		}
		message, err := connectErr.Details()[0].Value()
		if err != nil {
			t.Fatal(err)
		}
		detail, ok := message.(*actions.ActionOperationFailure)
		if !ok || detail.Kind != tc.kind || (detail.Response != nil) != tc.response || contracts.AdmitActionOperationFailure(detail) != nil {
			t.Fatal("incorrect durable failure", message)
		}
		if response != nil {
			response.Status = actions.ActionStatus_ACTION_STATUS_CONFIRMED
			if detail.Response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
				t.Fatal("response aliases mutable caller")
			}
		}
	}
}

func TestNHA147StoredAndLiveUnknownAreNotConflated(t *testing.T) {
	status := integrations.DurableCommandStatus{State: storage.IntentIndeterminate, ErrorKind: storage.IntentOutcomeIndeterminate,
		Response: &actions.CommandResponse{CommandId: "nha147-recovered", Status: actions.ActionStatus_ACTION_STATUS_INDETERMINATE}}
	durable, err := coreDurableResult(status)
	if err != nil {
		t.Fatal(err)
	}
	result := &actions.GetActionResponse{Durable: durable, Live: &actions.GetActionResponse_Unavailable{Unavailable: &actions.NoLiveActionProgress{}}}
	if contracts.AdmitGetActionResponse(result) != nil || result.Durable.State != actions.DurableActionState_DURABLE_ACTION_STATE_INDETERMINATE {
		t.Fatal("restart erased persisted uncertainty", result)
	}
	for _, bad := range []integrations.DurableCommandStatus{{State: storage.IntentUnknown}, {State: storage.IntentState(255)}, {State: storage.IntentResult, ErrorKind: storage.IntentError(255)}} {
		if _, err := coreDurableResult(bad); err == nil {
			t.Fatal("unknown storage state cast into wire", bad)
		}
	}
}

func TestNHA147ProgressionRequiresNativeAuthority(t *testing.T) {
	if _, err := (*integrations.DurableCommandService)(nil).AuthorizeActionRead(context.Background(), nha147ID(t, "nha147-denied")); !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
		t.Fatal("empty service admitted public progression", err)
	}
}
