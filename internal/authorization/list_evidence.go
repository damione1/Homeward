package authorization

import (
	"bytes"
	"crypto/sha256"
	"slices"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/domain"
)

// RightsEvidence is a private-authority-created snapshot for the SQL fence.
// Its fields cannot be supplied by a caller or derived from a client claim.
type RightsEvidence struct {
	account        contracts.AccountID
	role           authv1.AccountRole
	status         authv1.AccountStatus
	revocation     uint64
	rightsRevision uint64
	semantic       [sha256.Size]byte
	valid          bool
}

func (e RightsEvidence) Account() (contracts.AccountID, error) {
	if !e.valid {
		return contracts.AccountID{}, ErrInvalid
	}
	return e.account, nil
}
func (e RightsEvidence) Binding() (uint64, [sha256.Size]byte, error) {
	if !e.valid {
		return 0, [sha256.Size]byte{}, ErrInvalid
	}
	return e.rightsRevision, e.semantic, nil
}

// MatchesSQL must be called after locking auth_accounts, then the active
// policy, FOR SHARE in the same transaction before any list data is queried.
func (e RightsEvidence) MatchesSQL(role, status int16, revocation, rightsRevision int64, semantic []byte) bool {
	return e.valid && revocation > 0 && rightsRevision > 0 &&
		int16(e.role) == role && int16(e.status) == status &&
		e.status == authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE &&
		e.revocation == uint64(revocation) && e.rightsRevision == uint64(rightsRevision) &&
		len(semantic) == sha256.Size && bytes.Equal(e.semantic[:], semantic)
}

// ListSelection is an immutable copy of the exact READ collection grant.
// An explicit selector may be queried only with its IDs in the SQL predicate.
type ListSelection struct {
	all   bool
	ids   []domain.ListID
	valid bool
}

func (s ListSelection) All() bool { return s.valid && s.all }
func (s ListSelection) IDs() ([]domain.ListID, error) {
	if !s.valid {
		return nil, ErrInvalid
	}
	return slices.Clone(s.ids), nil
}

func (p *Permit) ListEvidence() (RightsEvidence, ListSelection, error) {
	if p == nil || p.authority == nil || p.entry == nil || p.object.kind != objectList ||
		p.permission.family != authv1.ScopeFamily_SCOPE_FAMILY_LISTS {
		return RightsEvidence{}, ListSelection{}, ErrDenied
	}
	a := p.authority
	p.entry.barrier.RLock()
	defer p.entry.barrier.RUnlock()
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !p.readyLocked() {
		return RightsEvidence{}, ListSelection{}, ErrDenied
	}
	state := a.accounts[p.account].state
	evidence := RightsEvidence{
		account: state.account, role: state.role, status: state.status,
		revocation: state.revocation, rightsRevision: state.rightsRevision,
		semantic: state.policy.semantic, valid: true,
	}
	for _, g := range state.policy.grants {
		if g.permission != p.permission || g.object != objectList || !g.permits(state.account, p.object) {
			continue
		}
		selection := ListSelection{all: g.kind == selectionAll, valid: true}
		if g.kind == selectionExplicit {
			selection.ids = make([]domain.ListID, 0, len(g.lists))
			for id := range g.lists {
				selection.ids = append(selection.ids, id)
			}
			slices.SortFunc(selection.ids, func(a, b domain.ListID) int {
				left, _ := a.Value()
				right, _ := b.Value()
				if left < right {
					return -1
				}
				if left > right {
					return 1
				}
				return 0
			})
		}
		return evidence, selection, nil
	}
	return RightsEvidence{}, ListSelection{}, ErrDenied
}
