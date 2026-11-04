package server

import (
	"crypto/sha256"
	"testing"
	"time"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
)

func TestNHA201OmittedRequestIDCreatesOccurrenceWithoutClientIdentity(t *testing.T) {
	actor, _ := domain.ParseActorID("synthetic-actor")
	capability, _ := apitypes.ParseCapabilityName("homes/synthetic/entities/relay/capabilities/power")
	metadata, err := storage.NewCommandAPIMetadata(capability, nil, sha256.Sum256([]byte("same explicit intention")))
	if err != nil {
		t.Fatal(err)
	}
	first, err := publicCommandID(actor, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := publicCommandID(actor, nil)
	if err != nil || first == second {
		t.Fatal("absent client key deduplicated separate calls", err)
	}
	for _, id := range []domain.CommandID{first, second} {
		native, _ := id.Value()
		name, err := apitypes.ParseOperationName("homes/synthetic/operations/cmd-" + native)
		if err != nil {
			t.Fatal(err)
		}
		operation, err := physicalOperation(name, integrations.DurableCommandStatus{
			State: storage.IntentAccepted, Response: &actions.CommandResponse{CommandId: native, Status: actions.ActionStatus_ACTION_STATUS_ACCEPTED},
			Metadata: metadata, HasMetadata: true, AcceptedAt: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		got := &api.CommandMetadata{}
		if operation.Metadata.UnmarshalTo(got) != nil || got.RequestId != "" || operation.Done {
			t.Fatal("server occurrence became a fabricated client UUID", got)
		}
	}
}
