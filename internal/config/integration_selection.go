package config

import (
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/worker"
	"sort"
)

// selectEnabledIntegrations runs ONLY after parseOperational has validated every
// declared configuration. Disabled YAML remains in the one protected source;
// both initial Run and029 consume the same filtered operational Config.
func selectEnabledIntegrations(disabled []string, c Config) (Config, error) {
	if len(disabled) > 1024 {
		return Config{}, ErrInvalidYAML
	}
	declared, err := declaredIntegrationInstances(c)
	if err != nil {
		return Config{}, err
	}
	off := make(map[string]bool, len(disabled))
	for _, text := range disabled {
		if !hasDeclaredIntegration(declared, text) || off[text] {
			return Config{}, ErrInvalidYAML
		}
		off[text] = true
	}
	isOff := func(id domain.IntegrationInstanceID) bool { text, _ := id.Value(); return off[text] }
	if c.sonos != nil {
		id, _ := c.sonos.Instance()
		if isOff(id) {
			c.sonos = nil
		}
	}
	if c.androidTV != nil {
		id, _ := c.androidTV.Instance()
		if isOff(id) {
			c.androidTV = nil
		}
	}
	if c.mqtt != nil {
		id, _ := c.mqtt.Instance()
		if isOff(id) {
			c.mqtt = nil
		}
	}
	if c.ecobee != nil {
		id, _ := c.ecobee.Instance()
		if isOff(id) {
			c.ecobee = nil
		}
	}
	if c.neviweb != nil {
		id, _ := c.neviweb.Instance()
		if isOff(id) {
			c.neviweb = nil
		}
	}
	if c.hilo != nil {
		id, _ := c.hilo.Instance()
		if isOff(id) {
			c.hilo = nil
		}
	}
	var workers []worker.Config
	if len(c.workers) > 0 {
		workers = make([]worker.Config, 0, len(c.workers))
	}
	for _, configured := range c.workers {
		id, _ := configured.Instance()
		if !isOff(id) {
			workers = append(workers, configured)
		}
	}
	c.workers = workers
	if off["homeward-sun-v1"] {
		c.solar = nil
	}
	if off["homeward-workday-v1"] {
		c.workday = nil
	}
	if off["homeward-energy-v1"] {
		c.energy = nil
	}
	if off["homeward-derived-power-v1"] {
		c.derivedPower = DerivedPowerUnspecified
	}
	return c, nil
}

func hasDeclaredIntegration(values map[string]domain.IntegrationInstanceID, key string) bool {
	_, present := values[key]
	return present
}

func declaredIntegrationInstances(c Config) (map[string]domain.IntegrationInstanceID, error) {
	declared := make(map[string]domain.IntegrationInstanceID)
	add := func(id domain.IntegrationInstanceID) error {
		text, err := id.Value()
		if err != nil || hasDeclaredIntegration(declared, text) {
			return ErrInvalidYAML
		}
		declared[text] = id
		return nil
	}
	if c.sonos != nil {
		id, err := c.sonos.Instance()
		if err != nil || add(id) != nil {
			return nil, ErrInvalidYAML
		}
	}
	if c.androidTV != nil {
		id, err := c.androidTV.Instance()
		if err != nil || add(id) != nil {
			return nil, ErrInvalidYAML
		}
	}
	if c.mqtt != nil {
		id, err := c.mqtt.Instance()
		if err != nil || add(id) != nil {
			return nil, ErrInvalidYAML
		}
	}
	if c.ecobee != nil {
		id, err := c.ecobee.Instance()
		if err != nil || add(id) != nil {
			return nil, ErrInvalidYAML
		}
	}
	if c.neviweb != nil {
		id, err := c.neviweb.Instance()
		if err != nil || add(id) != nil {
			return nil, ErrInvalidYAML
		}
	}
	if c.hilo != nil {
		id, err := c.hilo.Instance()
		if err != nil || add(id) != nil {
			return nil, ErrInvalidYAML
		}
	}
	for _, configured := range c.workers {
		id, err := configured.Instance()
		if err != nil || add(id) != nil {
			return nil, ErrInvalidYAML
		}
	}
	fixed := func(text string, present bool) error {
		if !present {
			return nil
		}
		id, err := domain.ParseIntegrationInstanceID(text)
		if err != nil {
			return err
		}
		return add(id)
	}
	if fixed("homeward-sun-v1", c.solar != nil) != nil || fixed("homeward-workday-v1", c.workday != nil) != nil || fixed("homeward-energy-v1", len(c.energy) != 0) != nil || fixed("homeward-derived-power-v1", c.derivedPower.Enabled()) != nil {
		return nil, ErrInvalidYAML
	}
	return declared, nil
}

// DeclaredIntegrationInstances exposes only admitted native identities. Disabled
// declarations remain present; the API cannot infer identity from a label or
// accidentally enable a runtime while assigning a durable resource name.
func (c Config) DeclaredIntegrationInstances() ([]domain.IntegrationInstanceID, error) {
	declared, err := declaredIntegrationInstances(c.DeclaredOperationalSettings())
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(declared))
	for key := range declared {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]domain.IntegrationInstanceID, 0, len(keys))
	for _, key := range keys {
		result = append(result, declared[key])
	}
	return result, nil
}
