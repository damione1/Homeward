package authorization

import authv1 "github.com/damione1/Homeward/gen/home/auth/v1"

// AdmitCredentialScopes admits only scopes for which this account currently
// has a grant (or an intrinsic administrative right). Scoped principals still
// evaluate each actual object through the same authority at use time.
func (s AccountState) AdmitCredentialScopes(permissions []Permission) error {
	if !s.valid() || s.status != authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE {
		return ErrDenied
	}
	if _, err := NewScopeSet(permissions); err != nil {
		return err
	}
	for _, permission := range permissions {
		if _, _, err := permission.Values(); err != nil {
			return err
		}
		if object, err := AdministrativeObject(permission.family); err == nil && intrinsic(s.role, permission, object) {
			continue
		}
		found := false
		for _, grant := range s.policy.grants {
			if grant.permission == permission && mcpGrantHasObjects(grant) {
				if s.role == authv1.AccountRole_ACCOUNT_ROLE_USER && permission.family == authv1.ScopeFamily_SCOPE_FAMILY_DASHBOARDS && permission.operation == authv1.ScopeOperation_SCOPE_OPERATION_ADMINISTER {
					continue
				}
				found = true
				break
			}
		}
		if !found {
			return ErrDenied
		}
	}
	return nil
}

func mcpGrantHasObjects(g grant) bool {
	switch g.object {
	case objectEntity:
		hasSlots := func(s slots) bool {
			return s.kind == selectionAll || s.kind == selectionExplicit && len(s.explicit) > 0
		}
		if g.kind == selectionAll {
			return hasSlots(g.allSlots)
		}
		if g.kind == selectionExplicit {
			for _, s := range g.entities {
				if hasSlots(s) {
					return true
				}
			}
		}
		return false
	case objectDashboard:
		return g.kind == selectionAll || g.kind == selectionOwn || g.kind == selectionExplicit && len(g.dashboards) > 0
	case objectList:
		return g.kind == selectionAll || g.kind == selectionExplicit && len(g.lists) > 0
	default:
		return false
	}
}

// CredentialBinding reuses the authority's canonical immutable binding without
// reacquiring its mutation barrier. Publication compares admitted MCP leases
// against this next committed state before making that state visible.
func (s AccountState) CredentialBinding(principal Principal) ([32]byte, error) {
	if !s.valid() || !principal.valid() || principal.account != s.account || s.status != authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE {
		return [32]byte{}, ErrDenied
	}
	return currentBinding(principal, s), nil
}
