package integrations

import (
	"errors"
	"fmt"
	"testing"

	"github.com/damione1/Homeward/internal/domain"
)

func TestNHA024CommandPolicyBudgetAndZeroIDs(t *testing.T) {
	host := &Manager{commandPolicies: make(map[domain.CommandID]commandPolicy), admission: admissionOpen}
	if err := host.claimCommandPolicy(domain.CommandID{}, commandPolicyDurable); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero ID = %v", err)
	}
	if err := host.claimCommandPolicy(domain.CommandID{}, commandPolicyUnknown); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero policy = %v", err)
	}
	valid, _ := domain.ParseCommandID("valid")
	if err := host.claimCommandPolicy(valid, commandPolicyUnknown); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown zero policy with valid ID = %v", err)
	}
	if err := host.claimCommandPolicy(valid, commandPolicy(255)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown policy = %v", err)
	}
	for i := 0; i < maxCommandPolicyEntries; i++ {
		id, err := domain.ParseCommandID(fmt.Sprintf("budget-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if err := host.claimCommandPolicy(id, commandPolicyDurable); err != nil {
			t.Fatalf("claim %d = %v", i, err)
		}
	}
	first, _ := domain.ParseCommandID("budget-0")
	if err := host.claimCommandPolicy(first, commandPolicyDurable); err != nil {
		t.Fatalf("same policy retry = %v", err)
	}
	if err := host.claimCommandPolicy(first, commandPolicyVolatile); !errors.Is(err, ErrCommandPolicyConflict) {
		t.Fatalf("downgrade = %v", err)
	}
	next, _ := domain.ParseCommandID("budget-next")
	if err := host.claimCommandPolicy(next, commandPolicyDurable); !errors.Is(err, ErrCommandPolicyLedgerFull) {
		t.Fatalf("overflow = %v", err)
	}
}
