package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

// All observations are supplied outside the frontend executable. Two sources
// deliberately expose the same Climate schema with different observed values.
type dashboardWidgetsDevice struct {
	Kind      string   `json:"kind"`
	Alias     string   `json:"alias"`
	Title     string   `json:"title"`
	Endpoint  string   `json:"endpoint"`
	Current   *float64 `json:"current,omitempty"`
	Target    *float64 `json:"target,omitempty"`
	On        *bool    `json:"on,omitempty"`
	Condition string   `json:"condition,omitempty"`
	alias     domain.HAAlias
	key       integrations.SourceKey
}

func dashboardWidgetsDevices(path string) ([]dashboardWidgetsDevice, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) > 131072 {
		return nil, errors.New("device fixture too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var devices []dashboardWidgetsDevice
	if err = decoder.Decode(&devices); err != nil {
		return nil, err
	}
	if decoder.Decode(new(any)) != io.EOF || len(devices) < 4 || len(devices) > 256 {
		return nil, errors.New("four to 256 explicit fixture sources required")
	}
	seen := map[string]bool{}
	for i := range devices {
		d := &devices[i]
		if d.Kind != "weather" && d.Condition != "" {
			return nil, errors.New("weather condition on another source kind")
		}
		if seen[d.Alias] || d.Title == "" || d.Endpoint == "" {
			return nil, errors.New("invalid source declaration")
		}
		seen[d.Alias] = true
		d.alias, err = domain.ParseHAAlias(d.Alias)
		if err != nil {
			return nil, err
		}
		d.key, err = integrations.NewSourceKey(d.Alias, d.Endpoint)
		if err != nil {
			return nil, err
		}
		switch d.Kind {
		case "thermostat":
			if d.Current == nil || d.Target == nil || d.On != nil {
				return nil, errors.New("climate observation required")
			}
		case "light", "binary", "switch", "fan":
			if d.On == nil || d.Current != nil || d.Target != nil {
				return nil, errors.New("binary observation required")
			}
		case "passive", "cover", "lock", "alarm":
			if d.On != nil || d.Current != nil || d.Target != nil {
				return nil, errors.New("passive source must not fabricate a value")
			}
		case "weather":
			if d.Current == nil || d.Target != nil || d.On != nil || (d.Condition != "clear" && d.Condition != "cloudy") {
				return nil, errors.New("weather observation required")
			}
		default:
			return nil, errors.New("unsupported source kind")
		}
		if _, err = d.observation(nil, false); err != nil {
			return nil, err
		}
	}
	return devices, nil
}
func (d dashboardWidgetsDevice) capability() *registrywire.CapabilityDescription {
	switch d.Kind {
	case "thermostat":
		return (nhf008Device{kind: nhf008Thermostat, endpoint: d.Endpoint, title: d.Title}).capability()
	case "light":
		return (nhf006Device{kind: nhf006Light, endpoint: nhf006Endpoint(d.Endpoint), title: d.Title}).capability()
	case "cover", "lock", "alarm":
		return (nhf009Device{kind: nhf009DeviceKind(d.Kind), endpoint: d.Endpoint, title: d.Title}).capability()
	case "fan":
		return (nhf008Device{kind: nhf008Fan, endpoint: d.Endpoint, title: d.Title}).capability()
	case "switch":
		return (nhf006Device{kind: nhf006Switch, endpoint: nhf006Endpoint(d.Endpoint), title: d.Title}).capability()
	case "binary", "passive":
		return (nhf006Device{kind: nhf006Binary, endpoint: nhf006Endpoint(d.Endpoint), title: d.Title}).capability()
	case "weather":
		return (nhf010Device{kind: nhf010Weather, endpoint: d.Endpoint, title: d.Title}).capability()
	default:
		panic("unvalidated source kind")
	}
}
func (d dashboardWidgetsDevice) observation(condition *bool, stale bool) (integrations.StateObservation, error) {
	var value domain.StateValue
	var err error
	switch d.Kind {
	case "thermostat":
		current, e := domain.NewCelsiusTemperature(*d.Current)
		if e != nil {
			return integrations.StateObservation{}, e
		}
		target, e := domain.NewCelsiusTemperature(*d.Target)
		if e != nil {
			return integrations.StateObservation{}, e
		}
		setpoint, e := domain.NewSingleClimateSetpoint(target)
		if e != nil {
			return integrations.StateObservation{}, e
		}
		climate, e := domain.NewClimateValue(domain.ClimateValueInput{CurrentTemperature: &current, Setpoint: &setpoint, Mode: nhf008Pointer(domain.ClimateModeHeat), Activity: nhf008Pointer(domain.ClimateActivityIdle), FanActive: nhf008Pointer(false)})
		if e != nil {
			return integrations.StateObservation{}, e
		}
		value, err = domain.NewClimateStateValue(climate)
	case "light":
		light, e := domain.NewLightValue(domain.LightValueInput{On: *d.On, Brightness: nhf008Pointer(30.), ColorTemperatureMireds: nil})
		if e != nil {
			return integrations.StateObservation{}, e
		}
		value, err = domain.NewLightStateValue(light)
	case "cover":
		state := domain.CoverStateOpen
		position := 35.
		obstruction := false
		cover, e := domain.NewCoverValue(&state, &position, &obstruction)
		if e != nil {
			return integrations.StateObservation{}, e
		}
		value, err = domain.NewCoverStateValue(cover)
	case "lock":
		value, err = domain.NewLockStateValue(domain.LockStateUnlocked)
	case "alarm":
		value, err = domain.NewAlarmStateValue(domain.AlarmStateDisarmed)
	case "fan":
		key := "Off"
		if *d.On {
			key = "High"
		}
		fan, e := domain.NewFanValue(domain.FanValueInput{On: d.On, Speed: &key})
		if e != nil {
			return integrations.StateObservation{}, e
		}
		value, err = domain.NewFanStateValue(fan)
	case "switch":
		value = domain.NewSwitchValue(*d.On)
	case "passive":
		quality, e := domain.NewQuality(domain.AvailabilityUnknown, domain.FreshnessStale, domain.ProvenanceObserved)
		if e != nil {
			return integrations.StateObservation{}, e
		}
		return integrations.NewStateObservation(d.Endpoint, quality, nil, nil, time.Now().UTC(), nil)
	case "binary":
		on := *d.On
		if condition != nil {
			on = *condition
		}
		value = domain.NewSwitchValue(on)
	case "weather":
		condition := domain.WeatherClearDay
		if d.Condition == "cloudy" {
			condition = domain.WeatherCloudy
		}
		weather, e := domain.NewWeatherValue(domain.WeatherParts{Condition: &condition, Temperature: &domain.Temperature{Value: *d.Current, Unit: domain.TemperatureCelsius}, SourceLabel: d.Title})
		if e != nil {
			return integrations.StateObservation{}, e
		}
		value, err = domain.NewWeatherStateValue(weather)
	default:
		return integrations.StateObservation{}, errors.New("unsupported source kind")
	}
	if err != nil {
		return integrations.StateObservation{}, err
	}
	freshness := domain.FreshnessFresh
	if stale && d.Kind == "binary" {
		freshness = domain.FreshnessStale
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, freshness, domain.ProvenanceObserved)
	if err != nil {
		return integrations.StateObservation{}, err
	}
	at := time.Now().UTC()
	return integrations.NewStateObservation(d.Endpoint, quality, &value, &at, at, nil)
}
