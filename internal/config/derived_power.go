package config

import (
	"errors"

	"go.yaml.in/yaml/v3"
)

var ErrInvalidDerivedPower = errors.New("configuration: invalid derived power profile")

// DerivedPowerProfile admits one closed, versioned formula set. Omission
// leaves the feature unconfigured; there is no arbitrary formula vocabulary.
type DerivedPowerProfile uint8

const (
	DerivedPowerUnspecified DerivedPowerProfile = iota
	DerivedPowerMVPV1
)

func (p DerivedPowerProfile) Enabled() bool               { return p == DerivedPowerMVPV1 }
func (c Config) DerivedPowerProfile() DerivedPowerProfile { return c.derivedPower }

func parseDerivedPowerInput(node yaml.Node) (DerivedPowerProfile, error) {
	if node.Kind == 0 {
		return DerivedPowerUnspecified, nil
	}
	if node.Kind == yaml.ScalarNode && node.Tag == "!!str" && node.Value == "mvp-v1" {
		return DerivedPowerMVPV1, nil
	}
	return DerivedPowerUnspecified, ErrInvalidDerivedPower
}
