package integrations

import (
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"testing"
)

func TestAPICommandFlightBindsNativeOccurrenceAndNeverRearms(t *testing.T) {
	host := &Manager{commandPolicies: make(map[domain.CommandID]commandPolicyClaim), admission: admissionOpen}
	id, _ := domain.ParseCommandID("native-synthetic")
	otherID, _ := domain.ParseCommandID("native-other")
	entity, _ := domain.ParseEntityID("synthetic-relay")
	slot, _ := authorization.NewCapabilitySlot("core.switch.v1", "relay")
	otherSlot, _ := authorization.NewCapabilitySlot("core.switch.v1", "other")
	target, err := authorization.NewCommandTarget(entity, slot)
	if err != nil {
		t.Fatal(err)
	}
	otherTarget, _ := authorization.NewCommandTarget(entity, otherSlot)
	fingerprint := []byte("exact actor and native request")
	if _, err := host.beginAPICommandFlight(id, "epoch", target, fingerprint); err == nil {
		t.Fatal("flight without native forclusion")
	}
	if err := host.claimCommandPolicy(id, commandPolicyDurable); err != nil {
		t.Fatal(err)
	}
	flight, err := host.beginAPICommandFlight(id, "epoch", target, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	before := host.sampleAPICommandFlight(id)
	if before.unavailable(id, "epoch", target, fingerprint) {
		t.Fatal("in-flight treated as abandoned")
	}
	if _, err := host.beginAPICommandFlight(id, "epoch", target, fingerprint); err == nil {
		t.Fatal("second occurrence admitted")
	}
	foreign := &Manager{commandPolicies: make(map[domain.CommandID]commandPolicyClaim)}
	foreign.finishAPICommandFlight(flight, true)
	if host.sampleAPICommandFlight(id).finished {
		t.Fatal("foreign owner ended flight")
	}
	host.finishAPICommandFlight(flight, true)
	evidence := host.sampleAPICommandFlight(id)
	if !evidence.unavailable(id, "epoch", target, fingerprint) {
		t.Fatal("owner completion absent")
	}
	if before.unavailable(id, "epoch", target, fingerprint) {
		t.Fatal("previous sample mutated across SQL read")
	}
	if evidence.unavailable(otherID, "epoch", target, fingerprint) || evidence.unavailable(id, "other-epoch", target, fingerprint) || evidence.unavailable(id, "epoch", otherTarget, fingerprint) || evidence.unavailable(id, "epoch", target, []byte("changed")) {
		t.Fatal("completion transplanted")
	}
	host.finishAPICommandFlight(flight, false)
	if !host.sampleAPICommandFlight(id).unavailable(id, "epoch", target, fingerprint) {
		t.Fatal("completion rewritten")
	}
	if err := host.claimCommandPolicy(id, commandPolicyDurable); err != nil {
		t.Fatal(err)
	}
	if len(host.commandPolicies) != 1 || host.commandPolicies[id].apiFlight != flight {
		t.Fatal("claim retry replaced or duplicated flight")
	}
	if err := host.claimCommandPolicy(id, commandPolicyVolatile); err == nil {
		t.Fatal("flight downgraded to volatile")
	}
}
