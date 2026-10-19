package config

import (
	"errors"

	"github.com/damione1/Homeward/internal/workday"
	"go.yaml.in/yaml/v3"
)

var ErrUnsupportedWorkdayProfile = errors.New("configuration: unsupported Workday profile")

// Only the retained profile is configurable. An explicit timezone is needed
// for the current binary state; no address or host timezone is inferred.
func parseWorkdayInput(node yaml.Node) (*workday.Settings, error) {
	if node.Kind == 0 {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode || len(node.Content) == 0 || len(node.Content)%2 != 0 {
		return nil, ErrInvalidYAML
	}
	if len(node.Content) != 2 {
		return nil, ErrUnsupportedWorkdayProfile
	}
	key, value := node.Content[0], node.Content[1]
	if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
		return nil, ErrInvalidYAML
	}
	if key.Value != "timezone" {
		return nil, ErrUnsupportedWorkdayProfile
	}
	if value.Kind != yaml.ScalarNode || value.Tag != "!!str" || value.Value == "" {
		return nil, ErrInvalidYAML
	}
	settings, err := workday.ParseSettings(workday.Input{Timezone: value.Value})
	if err != nil {
		return nil, err
	}
	return &settings, nil
}

func (c Config) WorkdaySettings() (workday.Settings, bool) {
	if c.workday == nil {
		return workday.Settings{}, false
	}
	return *c.workday, true
}
