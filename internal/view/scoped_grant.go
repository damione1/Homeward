package view

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"

	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	statewire "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
)

// EntityAccess is admitted from the trusted catalogue's complete slot set and
// the readable subset. Composite state is readable only for a complete set.
type EntityAccess struct {
	entity   domain.EntityID
	slots    map[authorization.CapabilitySlot]struct{}
	complete bool
	actions  map[authorization.CapabilitySlot]statewire.ActionAccess
}

func NewEntityAccess(entity domain.EntityID, catalogue, readable []authorization.CapabilitySlot) (EntityAccess, error) {
	if _, err := entity.Value(); err != nil || len(catalogue) == 0 || len(readable) == 0 {
		return EntityAccess{}, ErrInvalidInput
	}
	all := make(map[authorization.CapabilitySlot]struct{}, len(catalogue))
	for _, slot := range catalogue {
		if _, _, err := slot.Values(); err != nil {
			return EntityAccess{}, ErrInvalidInput
		}
		if _, duplicate := all[slot]; duplicate {
			return EntityAccess{}, ErrInvalidInput
		}
		all[slot] = struct{}{}
	}
	allowed := make(map[authorization.CapabilitySlot]struct{}, len(readable))
	for _, slot := range readable {
		if _, exists := all[slot]; !exists {
			return EntityAccess{}, ErrInvalidInput
		}
		if _, duplicate := allowed[slot]; duplicate {
			return EntityAccess{}, ErrInvalidInput
		}
		allowed[slot] = struct{}{}
	}
	return EntityAccess{entity: entity, slots: allowed, complete: len(all) == len(allowed)}, nil
}

// NewEntityAccessWithActions adds actual current write permissions, restricted
// to slots whose declarations are readable. The legacy constructor leaves this
// knowledge absent rather than interpreting readable as writable.
func NewEntityAccessWithActions(entity domain.EntityID, catalogue, readable, writable []authorization.CapabilitySlot) (EntityAccess, error) {
	entry, err := NewEntityAccess(entity, catalogue, readable)
	if err != nil {
		return EntityAccess{}, err
	}
	entry.actions = make(map[authorization.CapabilitySlot]statewire.ActionAccess, len(readable))
	for slot := range entry.slots {
		entry.actions[slot] = statewire.ActionAccess_ACTION_ACCESS_DENIED
	}
	seen := make(map[authorization.CapabilitySlot]struct{}, len(writable))
	for _, slot := range writable {
		if _, present := entry.slots[slot]; !present {
			return EntityAccess{}, ErrInvalidInput
		}
		if _, duplicate := seen[slot]; duplicate {
			return EntityAccess{}, ErrInvalidInput
		}
		seen[slot] = struct{}{}
		entry.actions[slot] = statewire.ActionAccess_ACTION_ACCESS_ALLOWED
	}
	return entry, nil
}

// NewScopedGrant binds both the credential ceiling and its exact readable
// slots. NewGrant remains the explicit legacy all-slots composition API.
func NewScopedGrant(actor ActorID, revision uint64, binding [sha256.Size]byte, access []EntityAccess) (Grant, error) {
	if _, err := actor.Value(); err != nil || revision == 0 || binding == ([sha256.Size]byte{}) {
		return Grant{}, ErrInvalidInput
	}
	allowed := make(map[domain.EntityID]struct{}, len(access))
	slots := make(map[domain.EntityID]map[authorization.CapabilitySlot]struct{}, len(access))
	complete := make(map[domain.EntityID]bool, len(access))
	actions := make(map[domain.EntityID]map[authorization.CapabilitySlot]statewire.ActionAccess)
	for _, entry := range access {
		if _, err := entry.entity.Value(); err != nil || len(entry.slots) == 0 {
			return Grant{}, ErrInvalidInput
		}
		if _, duplicate := allowed[entry.entity]; duplicate {
			return Grant{}, ErrInvalidInput
		}
		owned := make(map[authorization.CapabilitySlot]struct{}, len(entry.slots))
		for slot := range entry.slots {
			owned[slot] = struct{}{}
		}
		allowed[entry.entity] = struct{}{}
		slots[entry.entity] = owned
		complete[entry.entity] = entry.complete
		if entry.actions != nil {
			own := make(map[authorization.CapabilitySlot]statewire.ActionAccess, len(entry.actions))
			for slot, permission := range entry.actions {
				own[slot] = permission
			}
			actions[entry.entity] = own
		}
	}
	g := Grant{actor: actor, revision: revision, allowed: allowed, slots: slots, complete: complete, actions: actions}
	h := sha256.New()
	h.Write([]byte("homeward-scoped-view-grant-v1"))
	h.Write(binding[:])
	write := func(value string) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		h.Write(size[:])
		h.Write([]byte(value))
	}
	ids := make([]domain.EntityID, 0, len(allowed))
	for id := range allowed {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { a, _ := ids[i].Value(); b, _ := ids[j].Value(); return a < b })
	for _, id := range ids {
		text, _ := id.Value()
		write(text)
		keys := make([]authorization.CapabilitySlot, 0, len(slots[id]))
		for slot := range slots[id] {
			keys = append(keys, slot)
		}
		sort.Slice(keys, func(i, j int) bool {
			a, b, _ := keys[i].Values()
			c, d, _ := keys[j].Values()
			if a != c {
				return a < c
			}
			return b < d
		})
		var count [8]byte
		binary.BigEndian.PutUint64(count[:], uint64(len(keys)))
		h.Write(count[:])
		for _, slot := range keys {
			schema, endpoint, _ := slot.Values()
			write(schema)
			write(endpoint)
		}
		if complete[id] {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}
	}
	// Preserve legacy state-only digests. Explicit action knowledge is part of
	// the grant binding, so it cannot change under an existing snapshot cursor.
	if len(actions) != 0 {
		write("homeward-action-access-v1")
		for _, id := range ids {
			text, _ := id.Value()
			write(text)
			known := actions[id]
			if known == nil {
				h.Write([]byte{0})
				continue
			}
			h.Write([]byte{1})
			keys := make([]authorization.CapabilitySlot, 0, len(known))
			for slot := range known {
				keys = append(keys, slot)
			}
			sort.Slice(keys, func(i, j int) bool {
				a, b, _ := keys[i].Values()
				c, d, _ := keys[j].Values()
				if a != c {
					return a < c
				}
				return b < d
			})
			var count [8]byte
			binary.BigEndian.PutUint64(count[:], uint64(len(keys)))
			h.Write(count[:])
			for _, slot := range keys {
				schema, endpoint, _ := slot.Values()
				write(schema)
				write(endpoint)
				h.Write([]byte{byte(known[slot])})
			}
		}
	}
	copy(g.digest[:], h.Sum(nil))
	return g, nil
}

func (g Grant) actionAccess(id domain.EntityID, descriptions []*wireregistry.CapabilityDescription) []*statewire.CapabilityActionAccess {
	known := g.actions[id]
	if known == nil {
		return nil
	}
	result := make([]*statewire.CapabilityActionAccess, 0, len(descriptions))
	for _, description := range descriptions {
		slot, err := authorization.NewCapabilitySlot(description.SchemaId, description.Endpoint)
		if err != nil {
			continue
		}
		permission, present := known[slot]
		if !present {
			continue
		}
		result = append(result, &statewire.CapabilityActionAccess{SchemaId: description.SchemaId, Endpoint: description.Endpoint, Access: permission})
	}
	return result
}

func (g Grant) statePermitted(id domain.EntityID) bool {
	if _, allowed := g.allowed[id]; !allowed {
		return false
	}
	return g.slots == nil || g.complete[id]
}
func (g Grant) filterCapabilities(id domain.EntityID, descriptions []*wireregistry.CapabilityDescription) ([]*wireregistry.CapabilityDescription, bool) {
	if g.slots == nil {
		return descriptions, true
	}
	allowed := g.slots[id]
	filtered := make([]*wireregistry.CapabilityDescription, 0, len(descriptions))
	complete := g.complete[id] && len(descriptions) > 0
	for _, description := range descriptions {
		if description == nil {
			complete = false
			continue
		}
		slot, err := authorization.NewCapabilitySlot(description.SchemaId, description.Endpoint)
		if err != nil {
			complete = false
			continue
		}
		if _, ok := allowed[slot]; ok {
			filtered = append(filtered, description)
		} else {
			complete = false
		}
	}
	return filtered, complete
}
