package authorization

import auth "github.com/damione1/Homeward/gen/home/auth/v1"

// MediaEvidence carries the current native caller's exact MEDIA.READ entity
// permit into the MediaSession SQL owner. It grants no role, source, kind or
// credential authority beyond that already admitted target slot.
func (p *Permit) MediaEvidence() (RightsEvidence, error) {
	if p == nil || p.authority == nil || p.entry == nil || p.principal.kind != principalNative || p.object.kind != objectEntity || p.permission.family != auth.ScopeFamily_SCOPE_FAMILY_MEDIA || p.permission.operation != auth.ScopeOperation_SCOPE_OPERATION_READ {
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
