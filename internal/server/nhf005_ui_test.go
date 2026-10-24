package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	registryv1 "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/scope"
)

// Test-only presentation profiles. Production serving modes never read this
// environment boundary. The default retains NHF004's original 850 switches.
type nhf005UIProfile uint8

const (
	nhf005CacheUI nhf005UIProfile = iota + 1
	nhf005HomeUI
)

func parseNHF005UIProfile(input string) (nhf005UIProfile, error) {
	switch input {
	case "", "cache":
		return nhf005CacheUI, nil
	case "home":
		return nhf005HomeUI, nil
	default:
		return 0, fmt.Errorf("unknown synthetic UI profile %q", input)
	}
}
func (p nhf005UIProfile) label() string {
	switch p {
	case nhf005CacheUI:
		return "cache"
	case nhf005HomeUI:
		return "home"
	default:
		panic("invalid UI profile")
	}
}
func (p nhf005UIProfile) revision() scope.Revision {
	switch p {
	case nhf005CacheUI:
		return scope.BaselineV1()
	case nhf005HomeUI:
		return scope.EcobeeMVPV1()
	default:
		panic("invalid UI profile")
	}
}

type nhf005SampleKind uint8

const (
	nhf005SwitchSample nhf005SampleKind = iota + 1
	nhf005ClimateSample
	nhf005FanSample
	nhf005MissingSample
)

func (p nhf005UIProfile) sampleKind(alias string) nhf005SampleKind {
	switch p {
	case nhf005CacheUI:
		return nhf005SwitchSample
	case nhf005HomeUI:
		switch alias {
		case "climate.home":
			return nhf005ClimateSample
		case "fan.living_room_ceiling_fan":
			return nhf005FanSample
		case "weather.maison":
			return nhf005MissingSample
		default:
			return nhf005SwitchSample
		}
	default:
		panic("invalid UI profile")
	}
}
func (p nhf005UIProfile) capabilities(alias string, index int) []*registryv1.CapabilityDescription {
	description := &registryv1.CapabilityDescription{Major: 1, Endpoint: "relay", DisplayName: "Synthetic observation"}
	if p == nhf005CacheUI {
		description.DisplayName = fmt.Sprintf("Synthetic relay %03d", index)
	}
	switch p.sampleKind(alias) {
	case nhf005ClimateSample:
		description.SchemaId = contracts.ClimateSchema
		description.Constraints = &registryv1.CapabilityDescription_Climate{Climate: &registryv1.ClimateConstraints{ObservedTemperatureCelsius: &registryv1.ClimateTemperatureObservation{}}}
	case nhf005FanSample:
		description.SchemaId = contracts.FanSchema
		description.Constraints = &registryv1.CapabilityDescription_Fan{Fan: &registryv1.FanConstraints{SpeedOptions: []string{"Off", "Low", "Medium", "MediumHigh", "High"}}}
	case nhf005SwitchSample, nhf005MissingSample:
		description.SchemaId = contracts.SwitchSchema
		description.Constraints = &registryv1.CapabilityDescription_Switch{Switch: &registryv1.SwitchConstraints{SetOn: true}}
	default:
		panic("invalid synthetic sample kind")
	}
	return []*registryv1.CapabilityDescription{description}
}
func (p nhf005UIProfile) observation(alias string, sequence *uint64, at time.Time) (integrations.StateObservation, error) {
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		return integrations.StateObservation{}, err
	}
	var value domain.StateValue
	switch p.sampleKind(alias) {
	case nhf005SwitchSample:
		value = domain.NewSwitchValue(sequence != nil && *sequence%2 != 0)
	case nhf005FanSample:
		value, err = domain.NewFanStateValue(domain.FanSpeedLow)
	case nhf005ClimateSample:
		temperature, temperatureErr := domain.NewCelsiusTemperature(21.5)
		if temperatureErr != nil {
			return integrations.StateObservation{}, temperatureErr
		}
		climate, climateErr := domain.NewClimateValue(domain.ClimateValueInput{CurrentTemperature: &temperature})
		if climateErr != nil {
			return integrations.StateObservation{}, climateErr
		}
		value, err = domain.NewClimateStateValue(climate)
	case nhf005MissingSample:
		quality, err = domain.NewQuality(domain.AvailabilityUnavailable, domain.FreshnessStale, domain.ProvenanceObserved)
		if err != nil {
			return integrations.StateObservation{}, err
		}
		return integrations.NewStateObservation("relay", quality, nil, nil, at, sequence)
	default:
		panic("invalid synthetic sample kind")
	}
	if err != nil {
		return integrations.StateObservation{}, err
	}
	return integrations.NewStateObservation("relay", quality, &value, &at, at, sequence)
}

func TestNHF005PresentationProfileIsExplicitAndKeepsCacheDefaults(t *testing.T) {
	for _, input := range []string{"unknown", "HOME", " home", "home "} {
		if _, err := parseNHF005UIProfile(input); err == nil {
			t.Fatal("accepted unknown UI profile", input)
		}
	}
	profile, err := parseNHF005UIProfile("")
	if err != nil || profile != nhf005CacheUI || profile.revision() != scope.BaselineV1() {
		t.Fatal("default cache fixture changed")
	}
	for _, alias := range []string{"climate.home", "fan.living_room_ceiling_fan", "weather.maison"} {
		if profile.sampleKind(alias) != nhf005SwitchSample {
			t.Fatal("NHF004 default changed", alias)
		}
	}
	for _, alias := range []string{"climate.home", "fan.living_room_ceiling_fan", "weather.maison", "light.kitchen"} {
		key, _ := integrations.NewSourceKey("synthetic", "relay")
		if _, err := integrations.NewCandidate(key, nhf005HomeUI.capabilities(alias, 0)); err != nil {
			t.Fatal("invalid presentation capability", alias, err)
		}
		if _, err := nhf005HomeUI.observation(alias, nil, time.Now()); err != nil {
			t.Fatal("invalid presentation observation", alias, err)
		}
	}
}
