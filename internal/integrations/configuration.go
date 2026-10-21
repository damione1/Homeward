package integrations

import (
	"context"
	"errors"
	"github.com/damione1/Homeward/internal/domain"
)

// PublishConfigurationMQTTEdges changes the private native ingress only after
// Supervisor has joined the previous pilot. It grants no worker/API port.
func (h *Manager) PublishConfigurationMQTTEdges(instance domain.IntegrationInstanceID, enabled bool) error {
	if h == nil {
		return ErrInvalidInput
	}
	if enabled {
		if _, err := instance.Value(); err != nil {
			return ErrInvalidInput
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.admission != admissionOpen || h.composition != compositionFrozen {
		return ErrSupervisorState
	}
	if old := h.active[h.nativeMQTTInstance]; old != nil && old.active {
		return ErrSupervisorState
	}
	if enabled {
		if next := h.active[instance]; next != nil && next.active {
			return ErrSupervisorState
		}
	}
	if !enabled {
		h.nativeMQTTInstance, h.nativeMQTTSink = domain.IntegrationInstanceID{}, nil
		return nil
	}
	sink, ok := h.sink.(NativeMQTTEdgeSink)
	if !ok || nilInterface(sink) {
		return ErrNativeMQTTEdgeUnavailable
	}
	h.nativeMQTTInstance, h.nativeMQTTSink = instance, sink
	return nil
}

// DiscoverConfiguration is deliberately absent from the physical SessionHost
// port. It borrows the committed projection while the SQL registry gate is
// retained, avoiding a recursive wait on that gate during calculated output
// publication. All normal candidate/Host/capability checks remain shared.
func (h *Manager) DiscoverConfiguration(ctx context.Context, session *Session, candidates []Candidate) ([]Binding, error) {
	if h == nil || session == nil || session.host != h || nilInterface(ctx) {
		return nil, ErrInvalidInput
	}
	return session.discover(ctx, candidates, true)
}

// PublishConfigurationRegistry fences only bindings no longer authorized by
// committed SQL. Alias-only edits retain the same live binding and driver.
// The activation owner has drained DCS before entering this memory cut.
func (h *Manager) PublishConfigurationRegistry(ctx context.Context, records []RegistryRecord) error {
	if h == nil || nilInterface(ctx) || len(records) > 4096 {
		return ErrInvalidInput
	}
	admitted := make(map[domain.EntityID]RegistryRecord, len(records))
	for _, record := range records {
		_, id, err := record.IDs()
		if err != nil {
			return ErrInvalidInput
		}
		if _, exists := admitted[id]; exists {
			return ErrInvalidInput
		}
		admitted[id] = record
	}
	type retirement struct {
		session *Session
		binding Binding
	}
	h.mu.Lock()
	retire := make([]retirement, 0)
	for entity, binding := range h.owners {
		record, present := admitted[entity]
		instance, key, err := record.Source()
		if present && !record.Retired() && err == nil && instance == binding.instance && key == binding.key {
			continue
		}
		if session := h.active[binding.instance]; session != nil {
			retire = append(retire, retirement{session, binding})
		}
	}
	h.mu.Unlock()
	for _, item := range retire {
		err := item.session.withdrawFenced(ctx, item.binding)
		if err != nil && !errors.Is(err, ErrInactiveSession) && !errors.Is(err, ErrForeignBinding) {
			return err
		}
	}
	return nil
}
