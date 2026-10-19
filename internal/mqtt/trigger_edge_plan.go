package mqtt

import (
	"sort"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

// TriggerEdgePilot is proof of the real factory constructed for one admitted
// settings instance. Its private admission cannot be forged by declaring topic
// coverage alongside an unrelated discovery-only factory.
//
// Settings already contain the complete static union (including all script
// waits) before broker bootstrap. Runtime wait removal only disarms consumers;
// it never changes the broker connection or sends another SUBSCRIBE.
type TriggerEdgePilot struct {
	instance domain.IntegrationInstanceID
	filters  []TopicFilter
	factory  integrations.PilotFactory
}

func NewTriggerEdgePilot(settings Settings, repository discoveryRegistry, filters []TopicFilter) (TriggerEdgePilot, error) {
	instance, err := settings.Instance()
	if err != nil {
		return TriggerEdgePilot{}, err
	}
	// The existing constructor validates exact subscription coverage, limits,
	// uniqueness and the actual mapper before constructing the same factory.
	factory, err := NewDiscoveryPilotFactoryWithTriggerEdges(settings, repository, filters)
	if err != nil || missingInterface(factory) {
		return TriggerEdgePilot{}, ErrInvalidSettings
	}
	admitted := append([]TopicFilter(nil), filters...)
	sort.Slice(admitted, func(i, j int) bool { a, _ := admitted[i].Value(); b, _ := admitted[j].Value(); return a < b })
	return TriggerEdgePilot{instance: instance, filters: admitted, factory: factory}, nil
}

func (p TriggerEdgePilot) Instance() (domain.IntegrationInstanceID, error) {
	if missingInterface(p.factory) {
		return domain.IntegrationInstanceID{}, ErrInvalidSettings
	}
	if _, err := p.instance.Value(); err != nil {
		return domain.IntegrationInstanceID{}, ErrInvalidSettings
	}
	return p.instance, nil
}
func (p TriggerEdgePilot) Covers(instance domain.IntegrationInstanceID, filters []TopicFilter) bool {
	admitted, err := p.Instance()
	if err != nil || admitted != instance {
		return false
	}
	for _, required := range filters {
		if _, err := required.Value(); err != nil {
			return false
		}
		found := false
		for _, configured := range p.filters {
			if configured == required {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func (p TriggerEdgePilot) Factory() (integrations.PilotFactory, error) {
	if _, err := p.Instance(); err != nil {
		return nil, err
	}
	return p.factory, nil
}
func (p TriggerEdgePilot) Filters() []TopicFilter { return append([]TopicFilter(nil), p.filters...) }

// PilotSpec binds the instance and the actual factory from the same private
// admission. Server composition installs this spec itself and rejects another
// spec for that instance, so coverage cannot be detached from its transport.
func (p TriggerEdgePilot) PilotSpec() (integrations.PilotSpec, error) {
	instance, err := p.Instance()
	if err != nil {
		return integrations.PilotSpec{}, err
	}
	return integrations.NewPilotSpec(instance, p.factory)
}
