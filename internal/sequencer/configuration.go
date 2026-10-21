package sequencer

import (
	"context"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"reflect"
)

func (s *Sequencer) ResolveConfigurationIdentities(ctx context.Context, instance domain.IntegrationInstanceID, keys []integrations.SourceKey) ([]integrations.ResolvedIdentity, error) {
	if s == nil || ctx == nil || len(keys) == 0 || len(keys) > 256 {
		return nil, ErrInvalidInput
	}
	v := reflect.ValueOf(ctx)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if v.IsNil() {
			return nil, ErrInvalidInput
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := instance.Value(); err != nil {
		return nil, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.configurationPublication == nil || s.registryState != registryReady {
		return nil, ErrRegistryUnavailable
	}
	resolved := make([]integrations.ResolvedIdentity, len(keys))
	seen := make(map[integrations.SourceKey]bool, len(keys))
	for n, key := range keys {
		if _, _, err := key.Components(); err != nil || seen[key] {
			return nil, ErrInvalidInput
		}
		seen[key] = true
		found := false
		for _, record := range s.registry {
			owner, source, err := record.Source()
			if err != nil || record.Retired() || owner != instance || source != key {
				continue
			}
			if found {
				return nil, ErrIdentityConflict
			}
			device, entity, err := record.IDs()
			if err != nil {
				return nil, ErrIdentityConflict
			}
			resolved[n], found = integrations.ResolvedIdentity{Device: device, Entity: entity}, true
		}
		if !found {
			return nil, ErrIdentityConflict
		}
	}
	return resolved, nil
}

// ConfigurationPublication closes external captures while existing internal
// trigger/template subscriptions continue to consume their immutable events.
// Registry publication itself remains available to the activation owner.
type ConfigurationPublication struct {
	source   *Sequencer
	released bool
}

func (s *Sequencer) BeginConfigurationPublication() (*ConfigurationPublication, error) {
	if s == nil || s.journal == nil {
		return nil, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.configurationPublication != nil || s.registryState != registryReady {
		return nil, ErrRegistryUnavailable
	}
	p := &ConfigurationPublication{source: s}
	s.configurationPublication = p
	return p, nil
}

func (p *ConfigurationPublication) Release() error {
	if p == nil || p.source == nil {
		return ErrInvalidInput
	}
	s := p.source
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.released || s.configurationPublication != p {
		return ErrInvalidInput
	}
	p.released = true
	s.configurationPublication = nil
	return nil
}
