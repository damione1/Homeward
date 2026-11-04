package authorization

import (
	"sort"

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/domain"
)

// HistoryEntityWitness supplies an actual currently granted object for the
// entity-level admission/lease before query diagnostics. It is not evidence
// that any recorded observation is readable: every frozen slot is checked
// separately. A removed slot can remain readable without granting its current
// replacement. The catalogue candidate is used only by an all-slots grant.
func (c Current) HistoryEntityWitness(entity domain.EntityID, catalogue CapabilitySlot) (Object, error) {
	permission, err := NewPermission(auth.ScopeFamily_SCOPE_FAMILY_HISTORY, auth.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil || c.AdmitRoute(permission) != nil {
		return Object{}, ErrDenied
	}
	if _, err := entity.Value(); err != nil {
		return Object{}, ErrDenied
	}
	for _, g := range c.state.policy.grants {
		if g.permission != permission || g.object != objectEntity {
			continue
		}
		var allowed slots
		switch g.kind {
		case selectionAll:
			allowed = g.allSlots
		case selectionExplicit:
			allowed = g.entities[entity]
		default:
			continue
		}
		var candidates []CapabilitySlot
		switch allowed.kind {
		case selectionAll:
			candidates = []CapabilitySlot{catalogue}
		case selectionExplicit:
			for slot := range allowed.explicit {
				candidates = append(candidates, slot)
			}
			sort.Slice(candidates, func(i, j int) bool {
				return candidates[i].schema < candidates[j].schema || candidates[i].schema == candidates[j].schema && candidates[i].endpoint < candidates[j].endpoint
			})
		default:
			continue
		}
		for _, slot := range candidates {
			object, err := EntityObject(entity, slot)
			if err == nil && c.Authorize(permission, object) == nil {
				return object, nil
			}
		}
	}
	return Object{}, ErrDenied
}
