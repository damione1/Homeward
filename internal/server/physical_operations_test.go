package server

import (
	"crypto/sha256"
	"testing"
	"time"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
)

func TestNHA201PhysicalOperationSeparatesACKFromDurableConfirmation(t *testing.T) {
	name, _ := apitypes.ParseOperationName("homes/synthetic/operations/cmd-example")
	capability, _ := apitypes.ParseCapabilityName("homes/synthetic/entities/relay/capabilities/power")
	metadata, _ := storage.NewCommandAPIMetadata(capability, nil, sha256.Sum256([]byte("synthetic intention")))
	for _, tc := range []struct {
		label     string
		terminal  *actions.CommandResponse
		done      bool
		phase     api.CommandPhase
		outcome   api.CommandOutcome
		uncertain bool
	}{
		{"pending ACK", nil, false, api.CommandPhase_COMMAND_PHASE_ACKNOWLEDGED, 0, false},
		{"durable confirmation", &actions.CommandResponse{Status: actions.ActionStatus_ACTION_STATUS_CONFIRMED}, true, api.CommandPhase_COMMAND_PHASE_CONFIRMED, api.CommandOutcome_COMMAND_OUTCOME_CONFIRMED, false},
		{"restart ACK", &actions.CommandResponse{Status: actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED}, true, api.CommandPhase_COMMAND_PHASE_ACKNOWLEDGED, api.CommandOutcome_COMMAND_OUTCOME_ACKNOWLEDGED_UNVERIFIED, false},
		{"restart dispatch", &actions.CommandResponse{Status: actions.ActionStatus_ACTION_STATUS_DISPATCHED}, true, api.CommandPhase_COMMAND_PHASE_INDETERMINATE, 0, true},
	} {
		t.Run(tc.label, func(t *testing.T) {
			status := integrations.DurableCommandStatus{State: storage.IntentResult, Response: &actions.CommandResponse{Status: actions.ActionStatus_ACTION_STATUS_DISPATCHED}, APIAcknowledged: &actions.CommandResponse{Status: actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED}, ErrorKind: storage.IntentNoError, Metadata: metadata, HasMetadata: true, AcceptedAt: time.Now(), APITerminal: tc.terminal}
			operation, err := physicalOperation(name, status)
			if err != nil || operation.Done != tc.done {
				t.Fatal(operation, err)
			}
			progress := &api.CommandMetadata{}
			if err := operation.Metadata.UnmarshalTo(progress); err != nil || progress.Phase != tc.phase {
				t.Fatal(progress, err)
			}
			if tc.outcome != 0 {
				response := &api.ExecuteCapabilityResponse{}
				if err := operation.GetResponse().UnmarshalTo(response); err != nil || response.Outcome != tc.outcome || response.ObservedTime != nil {
					t.Fatal("invented guarantee/time", response, err)
				}
			}
			if tc.uncertain {
				fault, err := apierrors.AdmitStatus(operation.GetError())
				if err != nil || fault.Reason() != api.ApiErrorReason_API_ERROR_REASON_EFFECT_OUTCOME_UNKNOWN {
					t.Fatal(fault, err)
				}
				handle, ok := fault.Operation()
				if !ok || handle != name {
					t.Fatal("lost reconciliation handle")
				}
			}
		})
	}
}

func TestNHA201PhysicalOperationOwnerFailureIsRPCFaultUntilSQLReconciles(t *testing.T) {
	name, _ := apitypes.ParseOperationName("homes/synthetic/operations/cmd-example")
	capability, _ := apitypes.ParseCapabilityName("homes/synthetic/entities/relay/capabilities/power")
	metadata, _ := storage.NewCommandAPIMetadata(capability, nil, sha256.Sum256([]byte("original intention")))
	status := integrations.DurableCommandStatus{State: storage.IntentDispatching, Response: &actions.CommandResponse{Status: actions.ActionStatus_ACTION_STATUS_DISPATCHED}, Metadata: metadata, HasMetadata: true, AcceptedAt: time.Now(), APIReconciliationUnavailable: true, ErrorKind: storage.IntentNoError}
	operation, err := physicalOperation(name, status)
	fault, ok := err.(apierrors.Fault)
	if operation != nil || !ok || fault.Reason() != api.ApiErrorReason_API_ERROR_REASON_EFFECT_OUTCOME_UNKNOWN {
		t.Fatal("pending occurrence hid owner failure", operation, err)
	}
	linked, present := fault.Operation()
	if !present || linked != name {
		t.Fatal("lost exact reconciliation handle")
	}
	// Current SQL proof wins even if the owner could not receive COMMIT's reply.
	status.State = storage.IntentResult
	status.Response = &actions.CommandResponse{Status: actions.ActionStatus_ACTION_STATUS_CONFIRMED}
	operation, err = physicalOperation(name, status)
	if err != nil || operation == nil || !operation.Done || operation.GetResponse() == nil {
		t.Fatal("known durable proof erased", operation, err)
	}
	// Owner evidence is separate from an admitted native terminal tracking status.
	status.State = storage.IntentDispatching
	status.APIReconciliationUnavailable = false
	operation, err = physicalOperation(name, status)
	if err != nil || operation.Done || operation.GetResponse() != nil {
		t.Fatal("non-durable confirmation promoted", operation, err)
	}
}
