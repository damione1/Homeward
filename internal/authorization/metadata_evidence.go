package authorization

import auth "github.com/damione1/Homeward/gen/home/auth/v1"

// MetadataEvidence is distinct from list/calendar evidence. Only a current
// native administrator's global-configuration permit can construct it.
type MetadataEvidence struct{ rights RightsEvidence }

func (e MetadataEvidence) Rights() (RightsEvidence, error) {
	if !e.rights.valid {
		return RightsEvidence{}, ErrDenied
	}
	return e.rights, nil
}

func (p *Permit) EntityMetadataEvidence() (MetadataEvidence, error) {
	if p == nil || p.authority == nil || p.entry == nil || p.principal.kind != principalNative ||
		p.object.kind != objectGlobalConfiguration || p.permission.family != auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION ||
		p.permission.operation != auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER {
		return MetadataEvidence{}, ErrDenied
	}
	a := p.authority
	p.entry.barrier.RLock()
	defer p.entry.barrier.RUnlock()
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !p.readyLocked() {
		return MetadataEvidence{}, ErrDenied
	}
	state := a.accounts[p.account].state
	if state.role != auth.AccountRole_ACCOUNT_ROLE_ADMIN {
		return MetadataEvidence{}, ErrDenied
	}
	return MetadataEvidence{rights: RightsEvidence{account: state.account, role: state.role, status: state.status,
		revocation: state.revocation, rightsRevision: state.rightsRevision, semantic: state.policy.semantic, valid: true}}, nil
}
