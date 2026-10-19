package mqtt

import (
	"context"
	"errors"
	"math"

	"github.com/damione1/Homeward/internal/integrations"
)

var ErrTriggerEdgeAdmission = errors.New("mqtt: native trigger edge admission failed")

// triggerEdgeHandler is allocated anew with each supervised pilot session.
// Mapper publications precede the generic edge from the same packet.
type triggerEdgeHandler struct {
	mapper  *DiscoveryMapper
	filters []TopicFilter
	nextSeq uint64
}

func (h *triggerEdgeHandler) Observe(ctx context.Context, host integrations.SessionHost, observation Observation) error {
	if h == nil || h.mapper == nil || ctx == nil || missingInterface(host) {
		return ErrTriggerEdgeAdmission
	}
	if err := h.mapper.Observe(ctx, host, observation); err != nil {
		return err
	}
	topic, payload, err := observation.Event()
	if errors.Is(err, ErrNotAnEvent) {
		return nil // Retained restoration is never a trigger edge.
	}
	if err != nil {
		return err
	}
	if h.nextSeq == math.MaxUint64 {
		return ErrTriggerEdgeAdmission
	}
	h.nextSeq++
	matched := false
	for _, filter := range h.filters {
		if filter.Matches(topic) {
			matched = true
			break
		}
	}
	if !matched {
		return nil
	}
	publisher, ok := host.(integrations.NativeMQTTEdgePublisher)
	if !ok || missingInterface(publisher) {
		return ErrTriggerEdgeAdmission
	}
	name, err := topic.Value()
	if err != nil {
		return err
	}
	edge, err := integrations.NewNativeMQTTEdge(name, payload, false, h.nextSeq)
	if err != nil {
		return err
	}
	return publisher.PublishNativeMQTTEdge(ctx, edge)
}

// NewDiscoveryPilotFactoryWithTriggerEdges enables only filters explicitly
// present in the already admitted broker subscriptions. It neither adds a
// subscription nor changes the existing discovery-only factory.
func NewDiscoveryPilotFactoryWithTriggerEdges(settings Settings, repository discoveryRegistry, filters []TopicFilter) (integrations.PilotFactory, error) {
	if len(filters) == 0 {
		return NewDiscoveryPilotFactory(settings, repository)
	}
	if _, err := settings.Instance(); err != nil || len(filters) > len(settings.subscriptions) || len(filters) > 16 {
		return nil, ErrInvalidSettings
	}
	allowed := make(map[TopicFilter]bool, len(settings.subscriptions))
	for _, filter := range settings.subscriptions {
		allowed[filter] = true
	}
	admitted := make([]TopicFilter, 0, len(filters))
	seen := make(map[TopicFilter]bool, len(filters))
	for _, filter := range filters {
		if _, err := filter.Value(); err != nil || !allowed[filter] || seen[filter] {
			return nil, ErrInvalidSettings
		}
		seen[filter] = true
		admitted = append(admitted, filter)
	}
	if _, err := newDiscoveryMapper(settings, repository, nil); err != nil {
		return nil, err
	}
	return func() integrations.Pilot {
		mapper, err := newDiscoveryMapper(settings, repository, nil)
		if err != nil {
			return nil
		}
		handler := &triggerEdgeHandler{mapper: mapper, filters: admitted}
		pilot, err := NewPilot(settings, handler)
		if err != nil {
			return nil
		}
		return &discoveryPilot{Pilot: pilot, mapper: mapper}
	}, nil
}
