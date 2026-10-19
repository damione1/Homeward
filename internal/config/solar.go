package config

import (
	"fmt"
	"github.com/damione1/Homeward/internal/solar"
	"go.yaml.in/yaml/v3"
)

func parseSolarInput(node yaml.Node) (*solar.Settings, error) {
	if node.Kind == 0 {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode || len(node.Content) == 0 || len(node.Content)%2 != 0 {
		return nil, ErrInvalidYAML
	}
	var input solar.Input
	seen := make(map[string]bool, 4)
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] || value.Kind != yaml.ScalarNode {
			return nil, ErrInvalidYAML
		}
		seen[key.Value] = true
		switch key.Value {
		case "latitude", "longitude", "elevation_meters":
			if value.Tag != "!!float" && value.Tag != "!!int" {
				return nil, ErrInvalidYAML
			}
			var number float64
			if value.Decode(&number) != nil {
				return nil, ErrInvalidYAML
			}
			switch key.Value {
			case "latitude":
				input.Latitude = &number
			case "longitude":
				input.Longitude = &number
			case "elevation_meters":
				input.ElevationMeters = &number
			}
		case "timezone":
			if value.Tag != "!!str" {
				return nil, ErrInvalidYAML
			}
			input.Timezone = value.Value
		default:
			return nil, ErrInvalidYAML
		}
	}
	settings, err := solar.ParseSettings(input)
	if err != nil {
		return nil, ErrInvalidYAML
	}
	return &settings, nil
}

func (c Config) SolarSettings() (solar.Settings, bool) {
	if c.solar == nil {
		return solar.Settings{}, false
	}
	return *c.solar, true
}

// The private pointer slot also prevents fmt's fallback traversal of an
// unexported containing field from descending into the observer's numbers.
func (Config) Format(out fmt.State, _ rune) { _, _ = out.Write([]byte("config.Config(<private>)")) }
func (Config) String() string               { return "config.Config(<private>)" }
func (Config) GoString() string             { return "config.Config(<private>)" }
