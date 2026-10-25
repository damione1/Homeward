package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/damione1/Homeward/contracts"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

type nhf008DeviceKind string

const (
	nhf008Thermostat nhf008DeviceKind = "thermostat"
	nhf008HVAC       nhf008DeviceKind = "hvac"
	nhf008Fan        nhf008DeviceKind = "fan"
	nhf008Light      nhf008DeviceKind = "light"
)

func (k *nhf008DeviceKind) UnmarshalJSON(raw []byte) error {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return err
	}
	value := nhf008DeviceKind(text)
	switch value {
	case nhf008Thermostat, nhf008HVAC, nhf008Fan, nhf008Light:
		*k = value
		return nil
	default:
		return errors.New("unknown NHF008 synthetic device kind")
	}
}

type nhf008Device struct {
	kind            nhf008DeviceKind
	alias           domain.HAAlias
	key             integrations.SourceKey
	endpoint, title string
}

func nhf008ReadDevices(file string) ([]nhf008Device, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	if len(raw) > 16384 {
		return nil, errors.New("synthetic devices exceed bound")
	}
	var rows []struct {
		Kind     nhf008DeviceKind `json:"kind"`
		Alias    string           `json:"alias"`
		Title    string           `json:"title"`
		Endpoint string           `json:"endpoint"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rows); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing device source")
	}
	if len(rows) != 4 {
		return nil, errors.New("four synthetic device kinds required")
	}
	seen := make(map[nhf008DeviceKind]bool)
	var result []nhf008Device
	for _, row := range rows {
		if seen[row.Kind] || row.Kind == "" || row.Title == "" || len(row.Title) > 128 {
			return nil, errors.New("invalid device fixture")
		}
		seen[row.Kind] = true
		alias, err := domain.ParseHAAlias(row.Alias)
		if err != nil {
			return nil, err
		}
		key, err := integrations.NewSourceKey(string(row.Kind), row.Endpoint)
		if err != nil {
			return nil, err
		}
		result = append(result, nhf008Device{row.Kind, alias, key, row.Endpoint, row.Title})
	}
	return result, nil
}

func nhf008Pointer[T any](value T) *T { return &value }
func nhf008Bounds() *registrywire.NumberBounds {
	return &registrywire.NumberBounds{Minimum: 5, Maximum: 35, Step: nhf008Pointer(.5)}
}

func (d nhf008Device) capability() *registrywire.CapabilityDescription {
	result := &registrywire.CapabilityDescription{Major: 1, Endpoint: d.endpoint, DisplayName: d.title}
	switch d.kind {
	case nhf008Thermostat, nhf008HVAC:
		result.SchemaId = contracts.ClimateSchema
		c := &registrywire.ClimateConstraints{
			ObservedTemperatureCelsius: &registrywire.ClimateTemperatureObservation{},
			ObservedModeOptions:        []registrywire.ClimateMode{registrywire.ClimateMode_CLIMATE_MODE_HEAT},
			ActivityOptions:            []registrywire.ClimateActivity{registrywire.ClimateActivity_CLIMATE_ACTIVITY_IDLE},
			ObservesFanActivity:        true,
			ObservedHumidityPercent:    &registrywire.NumberBounds{Minimum: 0, Maximum: 100},
			ModeOptions:                []registrywire.ClimateMode{registrywire.ClimateMode_CLIMATE_MODE_HEAT},
		}
		if d.kind == nhf008Thermostat {
			c.SetpointCelsius = nhf008Bounds()
			c.ObservedSetpointCelsius = &registrywire.ClimateTemperatureObservation{Bounds: nhf008Bounds()}
		} else {
			c.SetpointRangeCelsius = &registrywire.ClimateSetpointRangeConstraints{LowCelsius: nhf008Bounds(), HighCelsius: nhf008Bounds(), MinimumDeltaCelsius: nhf008Pointer(2.), MaximumDeltaCelsius: nhf008Pointer(10.)}
			c.ObservedSetpointRangeCelsius = c.SetpointRangeCelsius
			c.ModeOptions = append(c.ModeOptions, registrywire.ClimateMode_CLIMATE_MODE_COOL, registrywire.ClimateMode_CLIMATE_MODE_AUTO)
			c.FanTargetOptions = []registrywire.ClimateFanTarget{registrywire.ClimateFanTarget_CLIMATE_FAN_TARGET_ON, registrywire.ClimateFanTarget_CLIMATE_FAN_TARGET_AUTO}
			c.ObservedFanTargetOptions = c.FanTargetOptions
		}
		result.Constraints = &registrywire.CapabilityDescription_Climate{Climate: c}
	case nhf008Fan:
		result.SchemaId = contracts.FanSchema
		result.Constraints = &registrywire.CapabilityDescription_Fan{Fan: &registrywire.FanConstraints{SetOn: true, SpeedOptions: []string{"Off", "Low", "High"}}}
	case nhf008Light:
		result.SchemaId = contracts.LightSchema
		result.Constraints = &registrywire.CapabilityDescription_Light{Light: &registrywire.LightConstraints{SetOn: true, ColorTempMireds: &registrywire.NumberBounds{Minimum: 250, Maximum: 454, Step: nhf008Pointer(7.)}}}
	default:
		panic("unparsed synthetic device kind")
	}
	return result
}

func (d nhf008Device) observation() (integrations.StateObservation, error) {
	var value domain.StateValue
	var err error
	switch d.kind {
	case nhf008Thermostat, nhf008HVAC:
		ambient, _ := domain.NewCelsiusTemperature(0)
		low, _ := domain.NewCelsiusTemperature(18)
		high, _ := domain.NewCelsiusTemperature(24)
		setpoint, _ := domain.NewSingleClimateSetpoint(low)
		if d.kind == nhf008HVAC {
			setpoint, _ = domain.NewRangeClimateSetpoint(low, high)
		}
		humidity, _ := domain.NewRelativeHumidity(0)
		input := domain.ClimateValueInput{CurrentTemperature: &ambient, Setpoint: &setpoint, Mode: nhf008Pointer(domain.ClimateModeHeat), Activity: nhf008Pointer(domain.ClimateActivityIdle), FanActive: nhf008Pointer(false), Humidity: &humidity}
		if d.kind == nhf008HVAC {
			input.FanTarget = nhf008Pointer(domain.NewClimateFanTarget(true))
		}
		climate, e := domain.NewClimateValue(input)
		if e != nil {
			return integrations.StateObservation{}, e
		}
		value, err = domain.NewClimateStateValue(climate)
	case nhf008Fan:
		value, err = domain.NewFanStateValue(domain.FanSpeedOff)
	case nhf008Light:
		light, e := domain.NewLightValue(false, nil, nhf008Pointer(uint32(334)))
		if e != nil {
			return integrations.StateObservation{}, e
		}
		value, err = domain.NewLightStateValue(light)
	default:
		return integrations.StateObservation{}, errors.New("unparsed synthetic device kind")
	}
	if err != nil {
		return integrations.StateObservation{}, err
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		return integrations.StateObservation{}, err
	}
	at := time.Now().UTC()
	return integrations.NewStateObservation(d.endpoint, quality, &value, &at, at, nil)
}
