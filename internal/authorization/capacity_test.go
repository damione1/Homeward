package authorization

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
)

// These are real admitted sources at the production 32 MiB aggregate bound,
// not a smaller test-only authority limit or a mutated private cost estimate.
func capacitySource(t *testing.T, account string, cost int) PolicySource {
	t.Helper()
	const entryCost = 192 + len("budget-0000")
	n := (cost - 384) / entryCost
	padding := cost - 384 - n*entryCost
	if n < 1 || n > MaxEntities || padding < 0 || padding > 245 {
		t.Fatal("invalid capacity fixture cost", cost)
	}
	var data strings.Builder
	fmt.Fprintf(&data, "schema: homeward.authorization/1\naccount_id: %q\ngrants:\n  - scope: {family: actions, operation: write}\n    objects:\n      entities:\n        explicit:\n", account)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("budget-%04d", i)
		if i == n-1 {
			id += strings.Repeat("x", padding)
		}
		fmt.Fprintf(&data, "          - entity_id: %q\n            capabilities: {all: {}}\n", id)
	}
	source, err := AdmitPolicySource(testAccount(t, account), []byte(data.String()))
	if err != nil || source.policy.cost != cost {
		t.Fatal("capacity fixture source/cost", cost, source.policy.cost, err)
	}
	return source
}

func capacityStates(t *testing.T, free int, initial ...PolicySource) []AccountState {
	t.Helper()
	remaining := MaxAuthorityBytes - free
	states := make([]AccountState, 0, 48)
	add := func(source PolicySource) {
		states = append(states, testState(t, source, authv1.AccountRole_ACCOUNT_ROLE_USER, 1, 1))
		remaining -= source.policy.cost
	}
	for _, source := range initial {
		add(source)
	}
	const maximum = 384 + MaxEntities*(192+len("budget-0000"))
	for index := 0; remaining > 0; index++ {
		cost := min(maximum, remaining)
		if after := remaining - cost; after > 0 && after < 600 {
			cost -= 600
		}
		add(capacitySource(t, fmt.Sprintf("capacity-filler-%d", index), cost))
	}
	return states
}

func TestNHA050PolicyCapacityReservationBoundariesAndConcurrentAccounts(t *testing.T) {
	aSource := capacitySource(t, "capacity-a", 600)
	bSource := capacitySource(t, "capacity-b", 600)
	states := capacityStates(t, 1, aSource, bSource)
	newAuthority := func() *Authority {
		a := NewAuthority()
		if err := a.Hydrate(states); err != nil || a.bytes != MaxAuthorityBytes-1 {
			t.Fatal("initial aggregate capacity", a.bytes, err)
		}
		return a
	}
	t.Run("exact-boundary-and-rollback", func(t *testing.T) {
		a := newAuthority()
		m, _ := a.BeginMutation(aSource.policy.account)
		if err := m.ReservePolicy(capacitySource(t, "capacity-a", 602)); !errors.Is(err, ErrCapacity) {
			t.Fatal("N+1 growth admitted", err)
		}
		if err := m.ReservePolicy(capacitySource(t, "capacity-a", 601)); err != nil || a.bytes+a.reservedBytes != MaxAuthorityBytes {
			t.Fatal("exact capacity rejected", err)
		}
		// An unrelated direct reconciliation cannot spend another mutation's
		// reserved byte even though the current committed projection fits.
		if err := a.Publish(testState(t, capacitySource(t, "capacity-b", 601), authv1.AccountRole_ACCOUNT_ROLE_USER, 2, 2)); !errors.Is(err, ErrCapacity) {
			t.Fatal("unreserved publication spent another account's budget", err)
		}
		m.Release() // confirmed rollback before any Publish
		if a.reservedBytes != 0 || len(a.reservations) != 0 || a.bytes != MaxAuthorityBytes-1 {
			t.Fatal("rollback leaked capacity reservation")
		}
		if err := m.ReservePolicy(aSource); !errors.Is(err, ErrInvalid) {
			t.Fatal("released mutation reused", err)
		}
	})
	t.Run("two-concurrent-accounts", func(t *testing.T) {
		a := newAuthority()
		mutations := make([]*Mutation, 2)
		sources := []PolicySource{capacitySource(t, "capacity-a", 601), capacitySource(t, "capacity-b", 601)}
		for i, source := range sources {
			mutations[i], _ = a.BeginMutation(source.policy.account)
			defer mutations[i].Release()
		}
		start := make(chan struct{})
		results := make([]error, 2)
		var wait sync.WaitGroup
		for i := range mutations {
			wait.Add(1)
			go func(i int) { defer wait.Done(); <-start; results[i] = mutations[i].ReservePolicy(sources[i]) }(i)
		}
		close(start)
		wait.Wait()
		accepted, refused := 0, 0
		for _, err := range results {
			if err == nil {
				accepted++
			} else if errors.Is(err, ErrCapacity) {
				refused++
			} else {
				t.Fatal(err)
			}
		}
		if accepted != 1 || refused != 1 || a.bytes+a.reservedBytes != MaxAuthorityBytes {
			t.Fatal("aggregate reservation race", accepted, refused)
		}
	})
	t.Run("shrinking-and-source-no-op", func(t *testing.T) {
		a := newAuthority()
		m, _ := a.BeginMutation(aSource.policy.account)
		data, _ := aSource.Bytes()
		same, _ := AdmitPolicySource(aSource.policy.account, append(data, []byte("# unchanged rights\n")...))
		if err := m.ReservePolicy(same); err != nil || a.reservedBytes != 0 {
			t.Fatal("no-op required new capacity", err)
		}
		if err := a.Publish(testState(t, same, authv1.AccountRole_ACCOUNT_ROLE_USER, 2, 1)); err != nil {
			t.Fatal(err)
		}
		m.Release()
		m, _ = a.BeginMutation(aSource.policy.account)
		empty, _ := EmptySource(aSource.policy.account)
		if err := m.ReservePolicy(empty); err != nil || a.bytes != MaxAuthorityBytes-1 {
			t.Fatal("uncommitted shrink released budget", err)
		}
		other, _ := a.BeginMutation(bSource.policy.account)
		if err := other.ReservePolicy(capacitySource(t, "capacity-b", 602)); !errors.Is(err, ErrCapacity) {
			t.Fatal("uncommitted shrink funded another transaction", err)
		}
		if err := a.Publish(testState(t, empty, authv1.AccountRole_ACCOUNT_ROLE_USER, 3, 3)); err != nil {
			t.Fatal(err)
		}
		m.Release()
		if err := other.ReservePolicy(capacitySource(t, "capacity-b", 602)); err != nil {
			t.Fatal("committed shrink did not release capacity", err)
		}
		other.Release()
		if a.reservedBytes != 0 || len(a.reservations) != 0 {
			t.Fatal("shrink/no-op leaked reservations")
		}
	})
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("unknown-reconcile-committed-%t", committed), func(t *testing.T) {
			a := newAuthority()
			m, _ := a.BeginMutation(aSource.policy.account)
			grown := capacitySource(t, "capacity-a", 601)
			if err := m.ReservePolicy(grown); err != nil {
				t.Fatal(err)
			}
			if err := a.QuarantineAccount(aSource.policy.account); err != nil {
				t.Fatal(err)
			}
			m.Release() // unknown commit or known commit with projection failure
			if a.reservedBytes != 1 || len(a.reservations) != 1 {
				t.Fatal("unknown outcome dropped its possible committed growth")
			}
			other, _ := a.BeginMutation(bSource.policy.account)
			if err := other.ReservePolicy(capacitySource(t, "capacity-b", 601)); !errors.Is(err, ErrCapacity) {
				t.Fatal("unknown outcome allowed aggregate oversubscription", err)
			}
			other.Release()
			settled := states[0]
			if committed {
				settled = testState(t, grown, authv1.AccountRole_ACCOUNT_ROLE_USER, 2, 2)
			}
			if err := a.Publish(settled); err != nil || a.reservedBytes != 0 || len(a.reservations) != 0 {
				t.Fatal("primary reconciliation did not consume reservation", err)
			}
			restartedStates := append([]AccountState(nil), states...)
			restartedStates[0] = settled
			restarted := NewAuthority()
			if err := restarted.Hydrate(restartedStates); err != nil || restarted.bytes != a.bytes {
				t.Fatal("confirmed policy could not hydrate after restart", err)
			}
		})
	}
}

func TestNHA050CapacitySeedAbsentAndDeleteWithLiveConsumers(t *testing.T) {
	a := NewAuthority()
	account := testAccount(t, "capacity-created")
	empty, _ := EmptySource(account)
	m, _ := a.BeginMutation(account)
	if err := m.ReservePolicy(empty); err != nil {
		t.Fatal(err)
	}
	_ = a.QuarantineAccount(account) // creation's outcome is not yet known
	m.Release()
	if a.reservedBytes != empty.policy.cost {
		t.Fatal("unknown creation lost seed reservation")
	}
	if err := a.RemoveAccount(account); err != nil || a.reservedBytes != 0 || len(a.reservations) != 0 {
		t.Fatal("settled absent account leaked seed capacity", err)
	}
	source := capacitySource(t, "capacity-created", 600)
	state := testState(t, source, authv1.AccountRole_ACCOUNT_ROLE_USER, 1, 1)
	if err := a.Publish(state); err != nil {
		t.Fatal(err)
	}
	principal := testPrincipal(t, account)
	sub, err := a.Subscribe(context.Background(), principal)
	if err != nil {
		t.Fatal(err)
	}
	permission := testPermission(t, authv1.ScopeFamily_SCOPE_FAMILY_ACTIONS, authv1.ScopeOperation_SCOPE_OPERATION_WRITE)
	var object Object
	for entity := range source.policy.grants[0].entities {
		slot, _ := NewCapabilitySlot("core.switch.v1", "main")
		object, _ = EntityObject(entity, slot)
	}
	permit, err := a.Permit(context.Background(), principal, permission, object)
	if err != nil {
		t.Fatal(err)
	}
	m, _ = a.BeginMutation(account)
	if err := m.ReservePolicy(capacitySource(t, "capacity-created", 601)); err != nil {
		t.Fatal(err)
	}
	if err := a.RemoveAccount(account); err != nil {
		t.Fatal(err)
	}
	m.Release()
	key, _ := account.Value()
	entry := a.accounts[key]
	if entry == nil || !entry.removed || entry.state.policy.grants != nil || entry.state.policy.cost != 0 || a.bytes != 0 || a.reservedBytes != 0 {
		t.Fatal("closed tombstone retained unaccounted compiled policy")
	}
	if permit.Ready() || !errors.Is(context.Cause(permit.Context()), ErrDenied) {
		t.Fatal("deleted account's live permit stayed usable")
	}
	if err := a.RemoveAccount(account); err != nil || a.bytes != 0 {
		t.Fatal("repeated deletion double-counted budget", err)
	}
	permit.Release()
	sub.Close()
	if a.accounts[key] != nil || a.bytes != 0 || a.permits != 0 || a.subscriptions != 0 || len(a.reservations) != 0 {
		t.Fatal("released consumers left a tombstone or reservation")
	}
}
