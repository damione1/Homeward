package apiresources

import (
	api "github.com/damione1/Homeward/gen/home/api/v2"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"google.golang.org/protobuf/proto"
)

func temperatureRangeBounds(raw *registry.ClimateSetpointRangeConstraints) (*api.TemperatureRangeBounds, error) {
	if raw == nil {
		return nil, nil
	}
	low, lowErr := bounds(raw.LowCelsius)
	high, highErr := bounds(raw.HighCelsius)
	if lowErr != nil || highErr != nil || low != nil && low.Minimum < -273.15 || high != nil && high.Minimum < -273.15 {
		return nil, ErrProjection
	}
	result := &api.TemperatureRangeBounds{LowCelsius: low, HighCelsius: high}
	if raw.MinimumDeltaCelsius != nil {
		v := *raw.MinimumDeltaCelsius
		if !finite(v) || v < 0 {
			return nil, ErrProjection
		}
		result.MinimumDeltaCelsius = &v
	}
	if raw.MaximumDeltaCelsius != nil {
		v := *raw.MaximumDeltaCelsius
		if !finite(v) || v < 0 || result.MinimumDeltaCelsius != nil && v < *result.MinimumDeltaCelsius {
			return nil, ErrProjection
		}
		result.MaximumDeltaCelsius = &v
	}
	return result, nil
}

func projectThermal(c *registry.CapabilityDescription_Climate, raw *state.ObservedState) ([]Capability, error) {
	if c == nil || c.Climate == nil {
		return nil, ErrProjection
	}
	definition := c.Climate
	single, err := bounds(definition.SetpointCelsius)
	if err != nil || single != nil && single.Minimum < -273.15 {
		return nil, ErrProjection
	}
	rangeBounds, err := temperatureRangeBounds(definition.SetpointRangeCelsius)
	if err != nil || rangeBounds != nil && (rangeBounds.LowCelsius == nil || rangeBounds.HighCelsius == nil) {
		return nil, ErrProjection
	}
	modes, err := mappedOptions(definition.ModeOptions, temperatureModes)
	if err != nil {
		return nil, err
	}
	fans, err := mappedOptions(definition.FanTargetOptions, temperatureFanTargets)
	if err != nil {
		return nil, err
	}
	control := &api.TemperatureControlCapability{SetpointCelsius: single, SetpointRangeCelsius: rangeBounds, ModeOptions: modes, FanTargetOptions: fans}
	control.ObservedModeOptions, err = mappedOptions(definition.ObservedModeOptions, temperatureModes)
	if err != nil {
		return nil, err
	}
	control.ObservedActivityOptions, err = mappedOptions(definition.ActivityOptions, temperatureActivities)
	if err != nil {
		return nil, err
	}
	control.ObservedFanTargetOptions, err = mappedOptions(definition.ObservedFanTargetOptions, temperatureFanTargets)
	if err != nil {
		return nil, err
	}
	control.ObservedSetpointRangeCelsius, err = temperatureRangeBounds(definition.ObservedSetpointRangeCelsius)
	if err != nil {
		return nil, err
	}
	control.ObservesFanActivity = definition.ObservesFanActivity
	if definition.ObservedSetpointCelsius != nil {
		limits, err := bounds(definition.ObservedSetpointCelsius.Bounds)
		if err != nil || limits != nil && limits.Minimum < -273.15 {
			return nil, ErrProjection
		}
		control.ObservedSetpointCelsius = &api.TemperatureObservationDefinition{Bounds: limits}
	}
	value := raw.GetClimate()
	if raw != nil {
		q, err := quality(raw.Quality)
		if err != nil {
			return nil, err
		}
		observation := &api.TemperatureControlObservation{ObservedTime: proto.CloneOf(raw.ObservedAt), Quality: q}
		if value != nil {
			switch setpoint := value.Setpoint.(type) {
			case *state.ClimateValue_SetpointCelsius:
				observation.Setpoint = &api.TemperatureControlObservation_SetpointCelsius{SetpointCelsius: setpoint.SetpointCelsius}
			case *state.ClimateValue_SetpointRangeCelsius:
				if setpoint.SetpointRangeCelsius == nil || setpoint.SetpointRangeCelsius.LowCelsius == nil || setpoint.SetpointRangeCelsius.HighCelsius == nil {
					return nil, ErrProjection
				}
				low, high := *setpoint.SetpointRangeCelsius.LowCelsius, *setpoint.SetpointRangeCelsius.HighCelsius
				observation.Setpoint = &api.TemperatureControlObservation_SetpointRangeCelsius{SetpointRangeCelsius: &api.TemperatureRange{LowCelsius: &low, HighCelsius: &high}}
			case nil:
			default:
				return nil, ErrProjection
			}
			if value.Mode != nil {
				mode, err := mapped(*value.Mode, temperatureModes)
				if err != nil {
					return nil, err
				}
				observation.Mode = &mode
			}
			if value.Activity != nil {
				activity, err := mapped(*value.Activity, temperatureActivities)
				if err != nil {
					return nil, err
				}
				observation.Activity = &activity
			}
			if value.FanTarget != nil {
				target, err := mapped(*value.FanTarget, temperatureFanTargets)
				if err != nil {
					return nil, err
				}
				observation.FanTarget = &target
			}
			observation.FanActive = value.FanActive
		}
		control.Observation = observation
	}
	result := []Capability{{trait: Thermal, value: &api.Capability{Trait: &api.Capability_TemperatureControl{TemperatureControl: control}}}}
	if definition.ObservedTemperatureCelsius != nil {
		var number *float64
		if value != nil {
			number = value.CurrentTemperatureCelsius
		}
		reading, err := numericCapability("Cel", api.MeasurementRole_MEASUREMENT_ROLE_TEMPERATURE, definition.ObservedTemperatureCelsius.Bounds, raw, number, nil)
		if err != nil {
			return nil, err
		}
		if reading.Bounds != nil && reading.Bounds.Minimum < -273.15 {
			return nil, ErrProjection
		}
		result = append(result, Capability{trait: Temperature, value: &api.Capability{Trait: &api.Capability_NumericMeasurement{NumericMeasurement: reading}}})
	}
	if definition.ObservedHumidityPercent != nil {
		var number *float64
		if value != nil {
			number = value.HumidityPercent
		}
		reading, err := numericCapability("%", api.MeasurementRole_MEASUREMENT_ROLE_HUMIDITY, definition.ObservedHumidityPercent, raw, number, nil)
		if err != nil || reading.Bounds != nil && (reading.Bounds.Minimum < 0 || reading.Bounds.Maximum > 100) {
			return nil, ErrProjection
		}
		result = append(result, Capability{trait: Humidity, value: &api.Capability{Trait: &api.Capability_NumericMeasurement{NumericMeasurement: reading}}})
	}
	return result, nil
}
