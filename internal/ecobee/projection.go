package ecobee

import (
	"fmt"
	"time"

	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
)

func (b binding) bounds() *wire.NumberBounds {
	if !b.hasBounds {
		return nil
	}
	step := b.step
	return &wire.NumberBounds{Minimum: b.min, Maximum: b.max, Step: &step}
}
func (p climateProfile) description() *wire.CapabilityDescription {
	c := &wire.ClimateConstraints{ObservedTemperatureCelsius: &wire.ClimateTemperatureObservation{Bounds: p.bindings[currentTemperature].bounds()}, ObservedSetpointCelsius: &wire.ClimateTemperatureObservation{Bounds: p.bindings[targetTemperature].bounds()}}
	if b := p.bindings[targetTemperature]; b.writable {
		c.SetpointCelsius = b.bounds()
	}
	for _, n := range []float64{0, 1, 2, 3} {
		mode := wire.ClimateMode(int32(n) + 1)
		c.ObservedModeOptions = append(c.ObservedModeOptions, mode)
		if b := p.bindings[requestedMode]; b.writable && b.accepts(n) {
			c.ModeOptions = append(c.ModeOptions, mode)
		}
	}
	c.ActivityOptions = []wire.ClimateActivity{wire.ClimateActivity_CLIMATE_ACTIVITY_HEATING, wire.ClimateActivity_CLIMATE_ACTIVITY_COOLING}
	if p.bindings[observedFan].present {
		c.ObservesFanActivity = true
		c.ActivityOptions = append(c.ActivityOptions, wire.ClimateActivity_CLIMATE_ACTIVITY_IDLE, wire.ClimateActivity_CLIMATE_ACTIVITY_FAN_ONLY)
	}
	if p.bindings[relativeHumidity].present {
		c.ObservedHumidityPercent = &wire.NumberBounds{Minimum: 0, Maximum: 100}
	}
	lo, hi := p.bindings[heatThreshold], p.bindings[coolThreshold]
	if lo.present && hi.present {
		c.ObservedSetpointRangeCelsius = &wire.ClimateSetpointRangeConstraints{LowCelsius: lo.bounds(), HighCelsius: hi.bounds()}
		if lo.writable && hi.writable {
			c.SetpointRangeCelsius = &wire.ClimateSetpointRangeConstraints{LowCelsius: lo.bounds(), HighCelsius: hi.bounds()}
		}
	}
	if b := p.bindings[targetFan]; b.present {
		c.ObservedFanTargetOptions = []wire.ClimateFanTarget{wire.ClimateFanTarget_CLIMATE_FAN_TARGET_ON, wire.ClimateFanTarget_CLIMATE_FAN_TARGET_AUTO}
		for _, n := range []float64{0, 1} {
			if b.writable && b.accepts(n) {
				c.FanTargetOptions = append(c.FanTargetOptions, wire.ClimateFanTarget(int32(n)+1))
			}
		}
	}
	return &wire.CapabilityDescription{SchemaId: contracts.ClimateSchema, Major: 1, Endpoint: Endpoint, DisplayName: "Ecobee ECB601", Constraints: &wire.CapabilityDescription_Climate{Climate: c}}
}

func (o climateObservation) domainValue(now time.Time) (*domain.StateValue, error) {
	var input domain.ClimateValueInput
	number := func(field climateField) (float64, bool) {
		v := o.value(field, now)
		return v.number, v.kind == valuePresent
	}
	if n, ok := number(currentTemperature); ok {
		v, err := domain.NewCelsiusTemperature(n)
		if err != nil {
			return nil, err
		}
		input.CurrentTemperature = &v
	}
	if lo, ok := number(heatThreshold); ok {
		if hi, present := number(coolThreshold); present {
			low, e := domain.NewCelsiusTemperature(lo)
			if e != nil {
				return nil, e
			}
			high, e := domain.NewCelsiusTemperature(hi)
			if e != nil {
				return nil, e
			}
			v, e := domain.NewRangeClimateSetpoint(low, high)
			if e != nil {
				return nil, e
			}
			input.Setpoint = &v
		}
	}
	// Auto uses the observed pair; other modes retain the single target when present.
	mode, hasMode := number(requestedMode)
	if input.Setpoint == nil || hasMode && mode != 3 {
		if n, ok := number(targetTemperature); ok {
			temperature, e := domain.NewCelsiusTemperature(n)
			if e != nil {
				return nil, e
			}
			v, e := domain.NewSingleClimateSetpoint(temperature)
			if e != nil {
				return nil, e
			}
			input.Setpoint = &v
		}
	}
	if hasMode {
		v, err := contracts.AdmitWireClimateMode(wire.ClimateMode(int32(mode) + 1))
		if err != nil {
			return nil, err
		}
		input.Mode = &v
	}
	if n, ok := number(relativeHumidity); ok {
		v, err := domain.NewRelativeHumidity(n)
		if err != nil {
			return nil, err
		}
		input.Humidity = &v
	}
	if n, ok := number(targetFan); ok {
		v := domain.NewClimateFanTarget(n == 1)
		input.FanTarget = &v
	}
	if n, ok := number(observedFan); ok {
		active := n == 2
		input.FanActive = &active
	}
	activity, kind := o.activity(now)
	if kind == valuePresent {
		var v domain.ClimateActivity
		switch activity {
		case activityHeating:
			v = domain.ClimateActivityHeating
		case activityCooling:
			v = domain.ClimateActivityCooling
		case activityFanOnly:
			v = domain.ClimateActivityFanOnly
		case activityIdle:
			v = domain.ClimateActivityIdle
		case activityUnknown:
			return nil, fmt.Errorf("unknown activity cannot be present")
		default:
			return nil, fmt.Errorf("invalid activity")
		}
		input.Activity = &v
	}
	climate, err := domain.NewClimateValue(input)
	if err != nil {
		return nil, err
	}
	v, err := domain.NewClimateStateValue(climate)
	if err != nil {
		return nil, err
	}
	return &v, nil
}
