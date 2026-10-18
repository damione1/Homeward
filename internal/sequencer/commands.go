package sequencer

import (
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"google.golang.org/protobuf/proto"
)

var _ integrations.CommandProjection = (*Sequencer)(nil)
var _ integrations.CommandEpochAuthority = (*Sequencer)(nil)

func (s *Sequencer) CurrentAdmissionEpoch() (string, error) {
	start, err := s.Start()
	if err != nil {
		return "", err
	}
	epoch, _, err := start.Components()
	return epoch, err
}

type commandRouteSlot struct {
	device   domain.DeviceID
	schema   string
	endpoint string
}

// Called only while the sequencer lock is held, after a registry or capability
// mutation. Command admission subsequently consults this bounded index.
func (s *Sequencer) rebuildCommandRoutes() {
	routes := make(map[commandRouteSlot]map[domain.EntityID]struct{})
	aliases := make(map[domain.HAAlias]domain.EntityID)
	for entity, record := range s.registry {
		if record.Retired() {
			continue
		}
		_, _, names, err := record.Aliases()
		if err != nil {
			continue
		}
		for _, alias := range names {
			if previous, exists := aliases[alias]; exists && previous != entity {
				// A malformed projection cannot pick an owner by map order.
				aliases[alias] = domain.EntityID{}
			} else if !exists {
				aliases[alias] = entity
			}
		}
	}
	for entity, descriptions := range s.capabilities {
		record, ok := s.registry[entity]
		if !ok || record.Retired() {
			continue
		}
		device, _, err := record.IDs()
		if err != nil {
			continue
		}
		for _, description := range descriptions {
			slot := commandRouteSlot{device: device, schema: description.SchemaId, endpoint: description.Endpoint}
			if routes[slot] == nil {
				routes[slot] = make(map[domain.EntityID]struct{})
			}
			routes[slot][entity] = struct{}{}
		}
	}
	s.commandRoutes = routes
	s.commandAliases = aliases
}

func (s *Sequencer) ResolveCommandAlias(alias domain.HAAlias) (domain.EntityID, error) {
	if s == nil || s.journal == nil {
		return domain.EntityID{}, integrations.ErrCommandRouteUnavailable
	}
	if _, err := alias.Value(); err != nil {
		return domain.EntityID{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.registryState != registryReady {
		return domain.EntityID{}, integrations.ErrCommandRouteUnavailable
	}
	entity, ok := s.commandAliases[alias]
	if !ok {
		return domain.EntityID{}, integrations.ErrCommandRouteUnavailable
	}
	if _, err := entity.Value(); err != nil {
		return domain.EntityID{}, integrations.ErrCommandRouteAmbiguous
	}
	return entity, nil
}

func (s *Sequencer) AdmitCommandRoute(route integrations.CommandRoute) error {
	if s == nil || s.journal == nil || route.Description == nil {
		return integrations.ErrCommandRouteUnavailable
	}
	if _, err := route.Entity.Value(); err != nil {
		return integrations.ErrCommandRouteUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.admitCommandRouteLocked(route)
}

// CaptureLightRoute applies the same route checks as command admission and
// copies the observed state under their sequencer lock. The caller holds the
// existing generation, Host and catalogue fences around this call.
func (s *Sequencer) CaptureLightRoute(route integrations.CommandRoute) (domain.ObservedState, bool, error) {
	if s == nil || s.journal == nil || route.Description == nil {
		return domain.ObservedState{}, false, integrations.ErrCommandRouteUnavailable
	}
	if _, err := route.Entity.Value(); err != nil {
		return domain.ObservedState{}, false, integrations.ErrCommandRouteUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.admitCommandRouteLocked(route); err != nil {
		return domain.ObservedState{}, false, err
	}
	state, exists := s.states[route.Entity]
	return state, exists, nil
}

func (s *Sequencer) admitCommandRouteLocked(route integrations.CommandRoute) error {
	if s.registryState != registryReady || route.Epoch == "" {
		return integrations.ErrCommandRouteUnavailable
	}
	epoch, _, _ := (Cursor{epoch: s.epoch}).Components()
	if route.Epoch != epoch {
		return integrations.ErrCommandEpochChanged
	}
	record, ok := s.registry[route.Entity]
	if !ok || record.Retired() {
		return integrations.ErrCommandRouteUnavailable
	}
	device, _, err := record.IDs()
	if err != nil {
		return integrations.ErrCommandRouteUnavailable
	}
	instance, key, err := record.Source()
	if err != nil {
		return integrations.ErrCommandRouteUnavailable
	}
	boundDevice, boundEntity, err := route.Binding.IDs()
	if err != nil || boundDevice != device || boundEntity != route.Entity {
		return integrations.ErrCommandRouteUnavailable
	}
	boundInstance, boundKey, err := route.Binding.Source()
	if err != nil || boundInstance != instance || boundKey != key {
		return integrations.ErrCommandRouteUnavailable
	}
	if route.Alias != nil {
		if resolved, ok := s.commandAliases[*route.Alias]; !ok || resolved != route.Entity {
			return integrations.ErrCommandRouteAmbiguous
		}
		_, _, aliases, err := record.Aliases()
		if err != nil {
			return integrations.ErrCommandRouteUnavailable
		}
		found := false
		for _, alias := range aliases {
			if alias == *route.Alias {
				found = true
				break
			}
		}
		if !found {
			return integrations.ErrCommandRouteUnavailable
		}
	}
	matched := false
	for _, description := range s.capabilities[route.Entity] {
		if proto.Equal(description, route.Description) {
			matched = true
			break
		}
	}
	if !matched {
		return integrations.ErrCommandRouteUnavailable
	}
	slot := commandRouteSlot{device: device, schema: route.Description.SchemaId, endpoint: route.Description.Endpoint}
	candidates := s.commandRoutes[slot]
	if _, exists := candidates[route.Entity]; !exists {
		return integrations.ErrCommandRouteUnavailable
	}
	if len(candidates) > 1 {
		if route.Designated != route.Entity {
			return integrations.ErrCommandRouteAmbiguous
		}
	} else if route.Designated != (domain.EntityID{}) && route.Designated != route.Entity {
		return integrations.ErrCommandRouteUnavailable
	}
	if route.ExpectedRevision != nil {
		state, exists := s.states[route.Entity]
		if !exists {
			return integrations.ErrCommandRevisionChanged
		}
		_, _, _, _, revision, err := state.Components()
		if err != nil || revision != *route.ExpectedRevision {
			return integrations.ErrCommandRevisionChanged
		}
	}
	return nil
}
