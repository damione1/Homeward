package authorization

import (
	"crypto/sha256"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
)

// AccountState is a trusted repository snapshot. Role and status are current
// account facts, not credential claims. Stored sources remain outside the
// memory authority; only their immutable compiled policy is retained.
type AccountState struct {
	account        contracts.AccountID
	role           authv1.AccountRole
	status         authv1.AccountStatus
	revocation     uint64
	version        Version
	rightsRevision uint64
	policy         Policy
}

func NewAccountState(source PolicySource, role authv1.AccountRole, status authv1.AccountStatus, revocation uint64, version Version, rightsRevision uint64) (AccountState, error) {
	if !source.valid() || contracts.AdmitAccountRole(role) != nil ||
		(status != authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE && status != authv1.AccountStatus_ACCOUNT_STATUS_DISABLED) ||
		revocation == 0 || revocation > 1<<63-1 || rightsRevision == 0 {
		return AccountState{}, ErrInvalid
	}
	revision, hash, err := version.Values()
	if err != nil || hash != source.hash || rightsRevision > revision {
		return AccountState{}, ErrInvalid
	}
	return AccountState{source.policy.account, role, status, revocation, version, rightsRevision, source.policy}, nil
}
func (s AccountState) Account() (contracts.AccountID, error) {
	if !s.valid() {
		return contracts.AccountID{}, ErrInvalid
	}
	return s.account, nil
}
func (s AccountState) Version() (Version, error) {
	if !s.valid() {
		return Version{}, ErrInvalid
	}
	return s.version, nil
}
func (s AccountState) RightsRevision() (uint64, error) {
	if !s.valid() {
		return 0, ErrInvalid
	}
	return s.rightsRevision, nil
}
func (s AccountState) Status() (authv1.AccountStatus, error) {
	if !s.valid() {
		return authv1.AccountStatus_ACCOUNT_STATUS_UNSPECIFIED, ErrInvalid
	}
	return s.status, nil
}
func (s AccountState) SemanticSHA256() ([sha256.Size]byte, error) {
	if !s.valid() {
		return [sha256.Size]byte{}, ErrInvalid
	}
	return s.policy.semantic, nil
}
func (s AccountState) valid() bool {
	if _, err := s.account.Value(); err != nil || s.account != s.policy.account || s.policy.semantic == ([sha256.Size]byte{}) || s.revocation == 0 || s.rightsRevision == 0 {
		return false
	}
	revision, _, err := s.version.Values()
	return err == nil && s.rightsRevision <= revision && contracts.AdmitAccountRole(s.role) == nil && (s.status == authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE || s.status == authv1.AccountStatus_ACCOUNT_STATUS_DISABLED)
}

func Evaluate(principal Principal, current AccountState, permission Permission, object Object) error {
	if !principal.valid() || !current.valid() || current.account != principal.account || !principal.permits(permission) || current.status != authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE {
		return ErrDenied
	}
	if _, _, err := permission.Values(); err != nil {
		return ErrDenied
	}
	if intrinsic(current.role, permission, object) {
		return nil
	}
	kind := configurable(permission)
	if kind == objectInvalid || object.kind != kind {
		return ErrDenied
	}
	switch current.role {
	case authv1.AccountRole_ACCOUNT_ROLE_ADMIN:
		// An administrator still needs a configurable object grant and its
		// credential ceiling. The role itself does not expose equipment.
	case authv1.AccountRole_ACCOUNT_ROLE_USER:
		if permission.family == authv1.ScopeFamily_SCOPE_FAMILY_DASHBOARDS &&
			(permission.operation == authv1.ScopeOperation_SCOPE_OPERATION_ADMINISTER || object.owner != current.account) {
			return ErrDenied
		}
	case authv1.AccountRole_ACCOUNT_ROLE_UNSPECIFIED:
		return ErrDenied
	default:
		return ErrDenied
	}
	for _, value := range current.policy.grants {
		if value.permission == permission && value.permits(current.account, object) {
			return nil
		}
	}
	return ErrDenied
}

func intrinsic(role authv1.AccountRole, permission Permission, object Object) bool {
	if role != authv1.AccountRole_ACCOUNT_ROLE_ADMIN {
		return false
	}
	switch permission.operation {
	case authv1.ScopeOperation_SCOPE_OPERATION_READ, authv1.ScopeOperation_SCOPE_OPERATION_WRITE, authv1.ScopeOperation_SCOPE_OPERATION_ADMINISTER:
	case authv1.ScopeOperation_SCOPE_OPERATION_UNSPECIFIED:
		return false
	default:
		return false
	}
	switch permission.family {
	case authv1.ScopeFamily_SCOPE_FAMILY_ACCOUNTS:
		return object.kind == objectAccounts
	case authv1.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS:
		return object.kind == objectIntegrations
	case authv1.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION:
		return object.kind == objectGlobalConfiguration
	case authv1.ScopeFamily_SCOPE_FAMILY_UNSPECIFIED, authv1.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY,
		authv1.ScopeFamily_SCOPE_FAMILY_HISTORY, authv1.ScopeFamily_SCOPE_FAMILY_MEDIA,
		authv1.ScopeFamily_SCOPE_FAMILY_LOCATION, authv1.ScopeFamily_SCOPE_FAMILY_ACTIONS,
		authv1.ScopeFamily_SCOPE_FAMILY_DASHBOARDS, authv1.ScopeFamily_SCOPE_FAMILY_CALENDARS,
		authv1.ScopeFamily_SCOPE_FAMILY_LISTS, authv1.ScopeFamily_SCOPE_FAMILY_LOGIC,
		authv1.ScopeFamily_SCOPE_FAMILY_BACKUPS:
		return false
	default:
		return false
	}
}
