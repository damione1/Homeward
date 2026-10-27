package authorization

import "github.com/damione1/Homeward/internal/domain"
import authv1 "github.com/damione1/Homeward/gen/home/auth/v1"

// CalendarObject denotes the calendar data belonging to a real registry entity.
// This fixed data slot is supplied by trusted service composition, never RPC.
func CalendarObject(entity domain.EntityID) (Object, error) {
	slot, err := NewCapabilitySlot("core.calendar.v1", "calendar")
	if err != nil {
		return Object{}, err
	}
	return EntityObject(entity, slot)
}

// CalendarEvidence reuses050's opaque committed-rights evidence. It grants no
// new identity or role and cannot be constructed from a client claim.
func (p *Permit) CalendarEvidence() (RightsEvidence, error) {
	if p == nil || p.authority == nil || p.entry == nil || p.object.kind != objectEntity || p.permission.family != authv1.ScopeFamily_SCOPE_FAMILY_CALENDARS {
		return RightsEvidence{}, ErrDenied
	}
	a := p.authority
	p.entry.barrier.RLock()
	defer p.entry.barrier.RUnlock()
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !p.readyLocked() {
		return RightsEvidence{}, ErrDenied
	}
	state := a.accounts[p.account].state
	return RightsEvidence{account: state.account, role: state.role, status: state.status, revocation: state.revocation, rightsRevision: state.rightsRevision, semantic: state.policy.semantic, valid: true}, nil
}
