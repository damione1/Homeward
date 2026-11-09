package config

import (
	"github.com/damione1/Homeward/internal/calendars"
	"go.yaml.in/yaml/v3"
)

// The house presentation zone is explicit operational configuration. It does
// not infer a zone from a device, a calendar source or the server environment.
func parseDisplayProfile(node yaml.Node) (string, error) {
	if node.Kind == 0 {
		return "", nil
	}
	if node.Kind != yaml.MappingNode || len(node.Content) != 2 {
		return "", ErrInvalidYAML
	}
	key, value := node.Content[0], node.Content[1]
	if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value != "time_zone" || value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
		return "", ErrInvalidYAML
	}
	zone, err := calendars.ParseZone(value.Value)
	if err != nil {
		return "", ErrInvalidYAML
	}
	return zone.Name(), nil
}

// DisplayTimeZone returns the admitted house presentation preference. Empty
// means unconfigured; callers must preserve that absence without a fallback.
func (c Config) DisplayTimeZone() string { return c.displayTimeZone }
