package integrations

import (
	"context"
	"errors"
	"testing"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/domain"
)

func TestNHA166CommonPolicyAndTransportBackstopsRefuseBeforeEffects(t *testing.T) {
	old, _ := domain.ParseCommandID("nha166-existing-policy")
	fresh, _ := domain.ParseCommandID("nha166-new-policy")
	host := &Manager{mqttReadOnlyObservation: true, admission: admissionOpen, commandPolicies: map[domain.CommandID]commandPolicy{old: commandPolicyVolatile}}
	for _, policy := range []commandPolicy{commandPolicyVolatile, commandPolicyDurable} {
		for _, id := range []domain.CommandID{old, fresh} {
			if err := host.claimCommandPolicy(id, policy); !errors.Is(err, ErrCommandAuthorizationDenied) {
				t.Fatal("common prepared/volatile/durable policy admitted action or replay", err)
			}
		}
	}
	if len(host.commandPolicies) != 1 || host.commandPolicies[old] != commandPolicyVolatile {
		t.Fatal("read-only claim mutated existing ledger")
	}
	// A router with no authority, projection, clock or driver is a tripwire:
	// dispatchOnce must refuse at its immutable fence before touching any seam.
	router := &CommandRouter{host: host}
	if response, err := router.dispatchOnce(context.Background(), domain.ActorID{}, &actions.CommandRequest{CommandId: "nha166-backstop"}, nil); response != nil || !errors.Is(err, ErrCommandAuthorizationDenied) {
		t.Fatal("final dispatch backstop entered routing", err)
	}
	if (AdmittedCommand{host: host}).ReadyForTransport() {
		t.Fatal("transport fence admitted an observation command")
	}
	if host.CommandDriverEntries() != 0 {
		t.Fatal("refused transport incremented dispatch count")
	}
}
