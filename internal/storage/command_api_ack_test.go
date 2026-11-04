package storage

import (
	"crypto/sha256"
	"testing"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

func TestNHA201AcknowledgementRecoveryIsBoundToNativeOccurrence(t *testing.T) {
	store, ctx := nha151Store(t)
	actor, _ := domain.ParseActorID("ack-test-actor")
	entity, _ := domain.ParseEntityID("ack-test-entity")
	slot, _ := authorization.NewCapabilitySlot("core.switch.v1", "main")
	target, _ := authorization.NewCommandTarget(entity, slot)
	otherEntity, _ := domain.ParseEntityID("other-entity")
	otherTarget, _ := authorization.NewCommandTarget(otherEntity, slot)
	capability, _ := apitypes.ParseCapabilityName("homes/synthetic/entities/relay/capabilities/power")
	metadata, err := NewCommandAPIMetadata(capability, nil, sha256.Sum256([]byte("test intention")))
	if err != nil {
		t.Fatal(err)
	}
	for _, hasACK := range []bool{false, true} {
		name := "without-ack"
		if hasACK {
			name = "with-ack"
		}
		t.Run(name, func(t *testing.T) {
			id, _ := domain.ParseCommandID(name)
			if _, created, outcome, err := store.AcceptAPICommandIntent(ctx, id, actor, "ack-epoch", []byte(name), target, metadata); err != nil || !created || outcome != TransactionCommitted {
				t.Fatal("admission", outcome, err)
			}
			if moved, outcome, err := store.MoveCommandIntent(ctx, id, IntentAccepted, IntentDispatching, nil, IntentNoError); err != nil || !moved || outcome != TransactionCommitted {
				t.Fatal("dispatch", outcome, err)
			}
			dispatch, _ := proto.Marshal(&actions.CommandResponse{CommandId: name, Status: actions.ActionStatus_ACTION_STATUS_DISPATCHED})
			if moved, outcome, err := store.MoveCommandIntent(ctx, id, IntentDispatching, IntentResult, dispatch, IntentNoError); err != nil || !moved || outcome != TransactionCommitted {
				t.Fatal("result", outcome, err)
			}
			ack, _ := proto.Marshal(&actions.CommandResponse{CommandId: name, Status: actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED})
			for _, invalid := range []*actions.CommandResponse{
				{CommandId: name, Status: actions.ActionStatus_ACTION_STATUS_ACCEPTED},
				{CommandId: "another-command", Status: actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED},
				{CommandId: name, Status: actions.ActionStatus_ACTION_STATUS_CONFIRMED},
			} {
				raw, _ := proto.Marshal(invalid)
				if outcome, err := store.AppendCommandAPIAcknowledgement(ctx, id, target, raw); err == nil || outcome != TransactionNotCommitted {
					t.Fatal("invalid native ACK accepted", outcome, err)
				}
			}
			if outcome, err := store.AppendCommandAPIAcknowledgement(ctx, id, otherTarget, ack); err == nil || outcome != TransactionNotCommitted {
				t.Fatal("wrong native target accepted", outcome, err)
			}
			if hasACK {
				for i := 0; i < 2; i++ {
					if outcome, err := store.AppendCommandAPIAcknowledgement(ctx, id, target, ack); err != nil || outcome != TransactionCommitted {
						t.Fatal("ACK append/idempotence", outcome, err)
					}
				}
				if _, err := store.pool.Exec(ctx, `UPDATE public.command_intents SET api_acknowledged_response=$2 WHERE command_id=$1`, name, dispatch); err == nil {
					t.Fatal("ACK proof was mutable")
				}
			}
			record, found, err := store.CommandIntent(ctx, id)
			if err != nil || !found || len(record.APITerminalResponse()) != 0 {
				t.Fatal("ACK prematurely terminal", err)
			}
			// Simulate startup after a process lost its native tracker. Recovery only
			// consumes immutable SQL facts; it never dispatches or claims confirmation.
			if outcome, err := store.RecoverCommandAPITerminals(ctx); err != nil || outcome != TransactionCommitted {
				t.Fatal("recovery", outcome, err)
			}
			record, found, err = store.CommandIntent(ctx, id)
			if err != nil || !found {
				t.Fatal(err)
			}
			terminal := &actions.CommandResponse{}
			expected := actions.ActionStatus_ACTION_STATUS_DISPATCHED
			if hasACK {
				expected = actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED
			}
			if proto.Unmarshal(record.APITerminalResponse(), terminal) != nil || terminal.CommandId != name || terminal.Status != expected {
				t.Fatal("recovery invented/lost evidence", terminal)
			}
		})
	}
}
