package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
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
	name, _ := apitypes.ParseOperationName("homes/synthetic/operations/cmd-nha147-recovered")
	capability, _ := apitypes.ParseCapabilityName("homes/synthetic/entities/relay/capabilities/power")
	metadata, _ := storage.NewCommandAPIMetadata(capability, nil, sha256.Sum256([]byte("nha147-recovered")))
	status := integrations.DurableCommandStatus{State: storage.IntentIndeterminate, ErrorKind: storage.IntentOutcomeIndeterminate,
		Response: &actions.CommandResponse{CommandId: "nha147-recovered", Status: actions.ActionStatus_ACTION_STATUS_INDETERMINATE},
		Metadata: metadata, HasMetadata: true, AcceptedAt: time.Now()}
	operation, err := physicalOperation(name, status)
	if err != nil || !operation.Done {
		t.Fatal(operation, err)
	}
	cmdMeta := &api.CommandMetadata{}
	if err := operation.Metadata.UnmarshalTo(cmdMeta); err != nil || cmdMeta.Phase != api.CommandPhase_COMMAND_PHASE_INDETERMINATE || cmdMeta.LiveProgressAvailable {
		t.Fatal("restart erased persisted uncertainty or invented live progress", cmdMeta, err)
	}
	fault, err := apierrors.AdmitStatus(operation.GetError())
	if err != nil || fault.Reason() != api.ApiErrorReason_API_ERROR_REASON_EFFECT_OUTCOME_UNKNOWN {
		t.Fatal("restart lost uncertain effect reason", fault, err)
	}
	for _, bad := range []integrations.DurableCommandStatus{
		{State: storage.IntentUnknown, Metadata: metadata, HasMetadata: true, AcceptedAt: time.Now(), Response: status.Response},
		{State: storage.IntentState(255), Metadata: metadata, HasMetadata: true, AcceptedAt: time.Now(), Response: status.Response},
		{State: storage.IntentResult, ErrorKind: storage.IntentError(255), Metadata: metadata, HasMetadata: true, AcceptedAt: time.Now(), Response: status.Response},
	} {
		if _, err := physicalOperation(name, bad); err == nil {
			t.Fatal("unknown storage state cast into wire", bad)
		}
	}
}

func TestNHA147ProgressionRequiresNativeAuthority(t *testing.T) {
	if _, err := (*integrations.DurableCommandService)(nil).AuthorizeActionRead(context.Background(), nha147ID(t, "nha147-denied")); !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
		t.Fatal("empty service admitted public progression", err)
	}
}
