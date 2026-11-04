package authorization

import (
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
)

// IntegrationAdministrationEvidence retains the exact principal and delegated
// ceiling of the native integration permit. An administrator MCP key is allowed
// only with its explicit INTEGRATIONS ADMINISTER ceiling; it never inherits the
// native session's unbounded ceiling. Storage must fence the key as well as rights.
type IntegrationAdministrationEvidence struct {
	rights    RightsEvidence
	principal Principal
}

type IntegrationCredentialKind uint8

const (
	IntegrationCredentialInvalid IntegrationCredentialKind = iota
	IntegrationNativeSession
	IntegrationMCPKey
)

func (e IntegrationAdministrationEvidence) CredentialKind() IntegrationCredentialKind {
	if !e.rights.valid || !e.principal.valid() {
		return IntegrationCredentialInvalid
	}
	switch e.principal.kind {
	case principalNative:
		return IntegrationNativeSession
	case principalScoped:
		return IntegrationMCPKey
	default:
		return IntegrationCredentialInvalid
	}
}
func (e IntegrationAdministrationEvidence) MCPKey() (contracts.McpKeyID, error) {
	if e.CredentialKind() != IntegrationMCPKey {
		return contracts.McpKeyID{}, ErrDenied
	}
	return contracts.ParseMcpKeyID(e.principal.credential)
}
func (e IntegrationAdministrationEvidence) Session() (contracts.SessionID, error) {
	if e.CredentialKind() != IntegrationNativeSession {
		return contracts.SessionID{}, ErrDenied
	}
	return contracts.ParseSessionID(e.principal.credential)
}

// MatchesMCPKey is called only on a locked, current credential row after the
// account/policy fence. It compares the complete ceiling, not merely one scope.
func (e IntegrationAdministrationEvidence) MatchesMCPKey(account string, revocation, rights int64, scopes []Permission) bool {
	if e.CredentialKind() != IntegrationMCPKey || revocation < 1 || rights < 1 || e.rights.revocation != uint64(revocation) || e.rights.rightsRevision != uint64(rights) {
		return false
	}
	actor, err := e.principal.account.Value()
	if err != nil || actor != account || len(scopes) != len(e.principal.scopes.values) {
		return false
	}
	seen := make(map[Permission]struct{}, len(scopes))
	for _, scope := range scopes {
		if _, duplicate := seen[scope]; duplicate {
			return false
		}
		seen[scope] = struct{}{}
		if _, present := e.principal.scopes.values[scope]; !present {
			return false
		}
	}
	return true
}

func (e IntegrationAdministrationEvidence) Rights() (RightsEvidence, error) {
	if !e.rights.valid || !e.principal.valid() {
		return RightsEvidence{}, ErrDenied
	}
	return e.rights, nil
}

func (p *Permit) IntegrationAdministrationEvidence() (IntegrationAdministrationEvidence, error) {
	if p == nil || p.authority == nil || p.entry == nil || !p.principal.valid() ||
		p.object.kind != objectIntegrations || p.permission.family != auth.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS ||
		p.permission.operation != auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER {
		return IntegrationAdministrationEvidence{}, ErrDenied
	}
	a := p.authority
	p.entry.barrier.RLock()
	defer p.entry.barrier.RUnlock()
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !p.readyLocked() {
		return IntegrationAdministrationEvidence{}, ErrDenied
	}
	state := a.accounts[p.account].state
	if state.role != auth.AccountRole_ACCOUNT_ROLE_ADMIN {
		return IntegrationAdministrationEvidence{}, ErrDenied
	}
	return IntegrationAdministrationEvidence{rights: RightsEvidence{account: state.account, role: state.role, status: state.status,
		revocation: state.revocation, rightsRevision: state.rightsRevision, semantic: state.policy.semantic, valid: true}, principal: p.principal}, nil
}

func (e IntegrationAdministrationEvidence) MatchesNativeSession(account string, revocation int64) bool {
	if e.CredentialKind() != IntegrationNativeSession || revocation < 1 || e.rights.revocation != uint64(revocation) {
		return false
	}
	actor, err := e.principal.account.Value()
	return err == nil && actor == account
}
