package storage

import (
	"crypto/sha256"
	"testing"

	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
)

func TestNHA201OmittedRequestIDRemainsNULLInNativeLedger(t *testing.T) {
	store, ctx := nha151Store(t)
	actor, _ := domain.ParseActorID("optional-key-actor")
	entity, _ := domain.ParseEntityID("optional-key-entity")
	slot, _ := authorization.NewCapabilitySlot("core.switch.v1", "main")
	target, _ := authorization.NewCommandTarget(entity, slot)
	capability, _ := apitypes.ParseCapabilityName("homes/synthetic/entities/relay/capabilities/power")
	metadata, err := NewCommandAPIMetadata(capability, nil, sha256.Sum256([]byte("identical intention")))
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"optional-occurrence-one", "optional-occurrence-two"} {
		id, _ := domain.ParseCommandID(raw)
		record, created, outcome, err := store.AcceptAPICommandIntent(ctx, id, actor, "optional-epoch", []byte("identical intention"), target, metadata)
		if err != nil || !created || outcome != TransactionCommitted {
			t.Fatal("separate occurrence collapsed", outcome, err)
		}
		admitted, present := record.APIMetadata()
		if _, supplied := admitted.RequestID(); !present || supplied {
			t.Fatal("admission invented client key")
		}
		restored, found, err := store.CommandIntent(ctx, id)
		if err != nil || !found {
			t.Fatal(err)
		}
		admitted, present = restored.APIMetadata()
		if _, supplied := admitted.RequestID(); !present || supplied {
			t.Fatal("reading invented client key")
		}
		var absent bool
		if err := store.pool.QueryRow(ctx, `SELECT api_request_id IS NULL FROM public.command_intents WHERE command_id=$1`, raw).Scan(&absent); err != nil || !absent {
			t.Fatal("NULL became fabricated client key", err)
		}
	}
	var total, withKey int
	if err := store.pool.QueryRow(ctx, `SELECT count(*), count(api_request_id) FROM public.command_intents WHERE actor_id=$1`, "optional-key-actor").Scan(&total, &withKey); err != nil || total != 2 || withKey != 0 {
		t.Fatal("optional occurrences merged or became UUID lookup candidates", total, withKey, err)
	}
}
