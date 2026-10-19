package authorization

// policyReservation protects the largest possible committed projection while
// a mutation is in SQL or has an unknown outcome. Only positive growth is
// reserved: shrinking cannot free budget until its commit is confirmed.
type policyReservation struct {
	owner       *Mutation
	growth      int
	quarantined bool
}

// ReservePolicy must run after the account gate/BeginMutation and before SQL
// can create an account or replace its policy. It reserves aggregate growth
// atomically across accounts. A known Publish consumes it; Release rolls it
// back unless QuarantineAccount preserved it until primary reconciliation.
func (m *Mutation) ReservePolicy(source PolicySource) error {
	if m == nil || m.authority == nil || !source.valid() {
		return ErrInvalid
	}
	key, _ := source.policy.account.Value()
	if key != m.key {
		return ErrInvalid
	}
	a := m.authority
	a.mu.Lock()
	defer a.mu.Unlock()
	if m.released || a.accounts == nil || a.reservations == nil {
		return ErrInvalid
	}
	if a.reservations[key] != nil {
		return ErrUnavailable
	}
	oldCost := 0
	if entry := a.accounts[key]; entry != nil {
		if entry.quarantined || entry.removed {
			return ErrUnavailable
		}
		oldCost = entry.state.policy.cost
	}
	growth := max(0, source.policy.cost-oldCost)
	if growth > MaxAuthorityBytes-a.bytes-a.reservedBytes {
		return ErrCapacity
	}
	a.reservations[key] = &policyReservation{owner: m, growth: growth}
	a.reservedBytes += growth
	return nil
}

// Caller holds authority.mu. This also handles primary reconciliation to an
// absent account after an unknown account-creation transaction.
func (a *Authority) releaseReservationLocked(key string) {
	if reservation := a.reservations[key]; reservation != nil {
		a.reservedBytes -= reservation.growth
		delete(a.reservations, key)
	}
}
