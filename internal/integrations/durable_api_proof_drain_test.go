package integrations

import (
	"context"
	"errors"
	"testing"
	"time"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/storage"
)

func TestAPIProofWaitEndsOnDurableCloseWithoutCancellingEffectContext(t *testing.T) {
	service := &DurableCommandService{
		router: &CommandRouter{}, store: &storage.Store{}, closed: make(chan struct{}),
		gate: durableGate{state: gateOpen, changed: make(chan struct{})}, active: 1,
	}
	retained, release := context.WithCancel(context.Background())
	defer release()
	proof, cleanup := service.apiProofContext(retained)
	defer cleanup()
	// An expired caller still initiates Close. No runtime is installed in this
	// unit fixture: SQL release belongs to the real-service integration case.
	closeRequest, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.Close(closeRequest); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-proof.Done():
	case <-time.After(time.Second):
		t.Fatal("proof wait ignored drain")
	}
	if retained.Err() != nil {
		t.Fatal("drain cancelled the retained effect credential")
	}
	late, lateCleanup := service.apiProofContext(retained)
	defer lateCleanup()
	if late.Err() != context.Canceled {
		t.Fatal("wait admitted after drain")
	}
	if service.active != 1 {
		t.Fatal("proof cancellation released occurrence before persistence")
	}
}

func TestAPIProofWaitPreservesCredentialAndPauseBoundaries(t *testing.T) {
	service := &DurableCommandService{gate: durableGate{state: gatePaused}}
	retained, release := context.WithCancel(context.Background())
	proof, cleanup := service.apiProofContext(retained)
	defer cleanup()
	if proof.Err() != nil {
		t.Fatal("activation pause ended proof tracking")
	}
	release()
	if proof.Err() != context.Canceled {
		t.Fatal("credential cancellation lost")
	}
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	deadline, done := service.apiProofContext(expired)
	defer done()
	if deadline.Err() != context.DeadlineExceeded {
		t.Fatal("credential deadline lost")
	}
}

func TestAPIProofDrainNeverPromotesDispatchIntoAcknowledgement(t *testing.T) {
	for _, status := range []actions.ActionStatus{
		actions.ActionStatus_ACTION_STATUS_ACCEPTED,
		actions.ActionStatus_ACTION_STATUS_DISPATCHED,
		actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED,
		actions.ActionStatus_ACTION_STATUS_CONFIRMED,
		actions.ActionStatus_ACTION_STATUS_INDETERMINATE,
	} {
		original := &actions.CommandResponse{CommandId: "synthetic-command", Status: status}
		result := stoppedAPICommandProof(original)
		expected := status
		if status == actions.ActionStatus_ACTION_STATUS_ACCEPTED || status == actions.ActionStatus_ACTION_STATUS_DISPATCHED {
			expected = actions.ActionStatus_ACTION_STATUS_INDETERMINATE
		}
		if result.Status != expected || result.CommandId != original.CommandId || original.Status != status {
			t.Fatal("evidence changed", status, result)
		}
	}
}
