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

func TestNHA147CanonicalActionFailuresDoNotFabricateResponse(t *testing.T) {
	for _, tc := range []struct {
		cause  error
		reason string
	}{
		{integrations.ErrDurableCommitUnknown, "COMMIT_OUTCOME_UNKNOWN"},
		{integrations.ErrCommandOutcomeIndeterminate, "EFFECT_OUTCOME_UNKNOWN"},
		{integrations.ErrDurableLedgerUnavailable, "SERVICE_UNAVAILABLE"},
		{integrations.ErrCommandIDConflict, "REQUEST_ID_REUSED"},
	} {
		public := apiRPCError(context.Background(), tc.cause)
		if info := nha200Info(t, public); info.Reason != tc.reason {
			t.Fatal(info)
		}
		var failure *connect.Error
		if !errors.As(public, &failure) {
			t.Fatal(public)
		}
		for _, detail := range failure.Details() {
			if detail.Type() == "home.actions.v1.ActionOperationFailure" {
				t.Fatal("obsolete detail emitted")
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
