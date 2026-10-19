package config

import (
	"errors"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestNHA107DerivedPowerProfileIsClosedAndExplicit(t *testing.T) {
	if profile, err := parseDerivedPowerInput(yaml.Node{}); err != nil || profile.Enabled() {
		t.Fatalf("omitted profile: %v, %v", profile, err)
	}
	for _, input := range []struct {
		text string
		want DerivedPowerProfile
	}{
		{"mvp-v1", DerivedPowerMVPV1},
		{"legacy-zero", DerivedPowerUnspecified},
		{"true", DerivedPowerUnspecified},
		{"null", DerivedPowerUnspecified},
	} {
		var node yaml.Node
		if err := yaml.Unmarshal([]byte(input.text), &node); err != nil {
			t.Fatal(err)
		}
		if len(node.Content) != 1 {
			t.Fatal("missing parsed scalar")
		}
		profile, err := parseDerivedPowerInput(*node.Content[0])
		if input.want == DerivedPowerUnspecified {
			if !errors.Is(err, ErrInvalidDerivedPower) {
				t.Fatalf("%q admitted: %v", input.text, profile)
			}
		} else if err != nil || profile != input.want || !profile.Enabled() {
			t.Fatalf("%q: %v, %v", input.text, profile, err)
		}
	}
}
