package contracts

import (
	"fmt"

	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
)

var (
	ErrUnsupportedClimateObservation        = fmt.Errorf("%w: climate observation is not advertised", ErrInvalidCapability)
	ErrClimateObservationOutsideConstraints = fmt.Errorf("%w: climate observation is outside advertised constraints", ErrInvalidCapability)
)

func AdmitWireClimateMode(mode registry.ClimateMode) (domain.ClimateMode, error) {
	switch mode {
	case registry.ClimateMode_CLIMATE_MODE_OFF:
		return domain.ClimateModeOff, nil
	case registry.ClimateMode_CLIMATE_MODE_HEAT:
		return domain.ClimateModeHeat, nil
	case registry.ClimateMode_CLIMATE_MODE_COOL:
		return domain.ClimateModeCool, nil
	case registry.ClimateMode_CLIMATE_MODE_AUTO:
		return domain.ClimateModeAuto, nil
	case registry.ClimateMode_CLIMATE_MODE_DRY:
		return domain.ClimateModeDry, nil
	case registry.ClimateMode_CLIMATE_MODE_FAN_ONLY:
		return domain.ClimateModeFanOnly, nil
	case registry.ClimateMode_CLIMATE_MODE_UNSPECIFIED:
	default:
	}
	return domain.ClimateModeUnspecified, ErrInvalidWireState
}

func WireClimateMode(mode domain.ClimateMode) (registry.ClimateMode, error) {
	switch mode {
	case domain.ClimateModeOff:
		return registry.ClimateMode_CLIMATE_MODE_OFF, nil
	case domain.ClimateModeHeat:
		return registry.ClimateMode_CLIMATE_MODE_HEAT, nil
	case domain.ClimateModeCool:
		return registry.ClimateMode_CLIMATE_MODE_COOL, nil
	case domain.ClimateModeAuto:
		return registry.ClimateMode_CLIMATE_MODE_AUTO, nil
	case domain.ClimateModeDry:
		return registry.ClimateMode_CLIMATE_MODE_DRY, nil
	case domain.ClimateModeFanOnly:
		return registry.ClimateMode_CLIMATE_MODE_FAN_ONLY, nil
	case domain.ClimateModeUnspecified:
	default:
	}
	return registry.ClimateMode_CLIMATE_MODE_UNSPECIFIED, ErrInvalidWireState
}

func AdmitWireClimateActivity(activity registry.ClimateActivity) (domain.ClimateActivity, error) {
	switch activity {
	case registry.ClimateActivity_CLIMATE_ACTIVITY_IDLE:
		return domain.ClimateActivityIdle, nil
	case registry.ClimateActivity_CLIMATE_ACTIVITY_HEATING:
		return domain.ClimateActivityHeating, nil
	case registry.ClimateActivity_CLIMATE_ACTIVITY_COOLING:
		return domain.ClimateActivityCooling, nil
	case registry.ClimateActivity_CLIMATE_ACTIVITY_DRYING:
		return domain.ClimateActivityDrying, nil
	case registry.ClimateActivity_CLIMATE_ACTIVITY_FAN_ONLY:
		return domain.ClimateActivityFanOnly, nil
	case registry.ClimateActivity_CLIMATE_ACTIVITY_UNSPECIFIED:
	default:
	}
	return domain.ClimateActivityUnspecified, ErrInvalidWireState
}

func WireClimateActivity(activity domain.ClimateActivity) (registry.ClimateActivity, error) {
	switch activity {
	case domain.ClimateActivityIdle:
		return registry.ClimateActivity_CLIMATE_ACTIVITY_IDLE, nil
	case domain.ClimateActivityHeating:
		return registry.ClimateActivity_CLIMATE_ACTIVITY_HEATING, nil
	case domain.ClimateActivityCooling:
		return registry.ClimateActivity_CLIMATE_ACTIVITY_COOLING, nil
	case domain.ClimateActivityDrying:
		return registry.ClimateActivity_CLIMATE_ACTIVITY_DRYING, nil
	case domain.ClimateActivityFanOnly:
		return registry.ClimateActivity_CLIMATE_ACTIVITY_FAN_ONLY, nil
	case domain.ClimateActivityUnspecified:
	default:
	}
	return registry.ClimateActivity_CLIMATE_ACTIVITY_UNSPECIFIED, ErrInvalidWireState
}

// AdmitWireClimateValue checks representation, not endpoint authority. Both
// external workers and snapshot admission still cross the shared catalogue.
func AdmitWireClimateValue(wire *state.ClimateValue) (domain.ClimateValue, error) {
	if wire == nil || !knownFieldsOnly(wire.ProtoReflect()) {
		return domain.ClimateValue{}, ErrInvalidWireState
	}
	var input domain.ClimateValueInput
	if wire.CurrentTemperatureCelsius != nil {
		temperature, err := domain.NewCelsiusTemperature(*wire.CurrentTemperatureCelsius)
		if err != nil {
			return domain.ClimateValue{}, ErrInvalidWireState
		}
		input.CurrentTemperature = &temperature
	}
	switch setpoint := wire.Setpoint.(type) {
	case *state.ClimateValue_SetpointCelsius:
		if setpoint == nil {
			return domain.ClimateValue{}, ErrInvalidWireState
		}
		temperature, err := domain.NewCelsiusTemperature(setpoint.SetpointCelsius)
		if err != nil {
			return domain.ClimateValue{}, ErrInvalidWireState
		}
		value, err := domain.NewSingleClimateSetpoint(temperature)
		if err != nil {
			return domain.ClimateValue{}, ErrInvalidWireState
		}
		input.Setpoint = &value
	case *state.ClimateValue_SetpointRangeCelsius:
		if setpoint == nil || setpoint.SetpointRangeCelsius == nil || setpoint.SetpointRangeCelsius.LowCelsius == nil || setpoint.SetpointRangeCelsius.HighCelsius == nil {
			return domain.ClimateValue{}, ErrInvalidWireState
		}
		low, err := domain.NewCelsiusTemperature(*setpoint.SetpointRangeCelsius.LowCelsius)
		if err != nil {
			return domain.ClimateValue{}, ErrInvalidWireState
		}
		high, err := domain.NewCelsiusTemperature(*setpoint.SetpointRangeCelsius.HighCelsius)
		if err != nil {
			return domain.ClimateValue{}, ErrInvalidWireState
		}
		value, err := domain.NewRangeClimateSetpoint(low, high)
		if err != nil {
			return domain.ClimateValue{}, ErrInvalidWireState
		}
		input.Setpoint = &value
	case nil:
	default:
		return domain.ClimateValue{}, ErrInvalidWireState
	}
	if wire.Mode != nil {
		mode, err := AdmitWireClimateMode(*wire.Mode)
		if err != nil {
			return domain.ClimateValue{}, err
		}
		input.Mode = &mode
	}
	if wire.Activity != nil {
		activity, err := AdmitWireClimateActivity(*wire.Activity)
		if err != nil {
			return domain.ClimateValue{}, err
		}
		input.Activity = &activity
	}
	if wire.HumidityPercent != nil {
		humidity, err := domain.NewRelativeHumidity(*wire.HumidityPercent)
		if err != nil {
			return domain.ClimateValue{}, ErrInvalidWireState
		}
		input.Humidity = &humidity
	}
	if wire.FanTarget != nil {
		target, err := AdmitWireClimateFanTarget(*wire.FanTarget)
		if err != nil {
			return domain.ClimateValue{}, err
		}
		input.FanTarget = &target
	}
	input.FanActive = wire.FanActive
	value, err := domain.NewClimateValue(input)
	if err != nil {
		return domain.ClimateValue{}, ErrInvalidWireState
	}
	return value, nil
}

func EncodeClimateValue(value domain.ClimateValue) (*state.ClimateValue, error) {
	input, err := value.Components()
	if err != nil {
		return nil, err
	}
	wire := &state.ClimateValue{FanActive: input.FanActive}
	if input.Humidity != nil {
		number, err := input.Humidity.Value()
		if err != nil {
			return nil, err
		}
		wire.HumidityPercent = &number
	}
	if input.FanTarget != nil {
		target, err := WireClimateFanTarget(*input.FanTarget)
		if err != nil {
			return nil, err
		}
		wire.FanTarget = &target
	}
	if input.CurrentTemperature != nil {
		temperature, err := input.CurrentTemperature.Value()
		if err != nil {
			return nil, err
		}
		wire.CurrentTemperatureCelsius = &temperature
	}
	if input.Setpoint != nil {
		kind, err := input.Setpoint.Kind()
		if err != nil {
			return nil, err
		}
		switch kind {
		case domain.ClimateSetpointSingle:
			temperature, _ := input.Setpoint.Single()
			number, err := temperature.Value()
			if err != nil {
				return nil, err
			}
			wire.Setpoint = &state.ClimateValue_SetpointCelsius{SetpointCelsius: number}
		case domain.ClimateSetpointRange:
			low, high, _ := input.Setpoint.Range()
			lower, err := low.Value()
			if err != nil {
				return nil, err
			}
			upper, err := high.Value()
			if err != nil {
				return nil, err
			}
			wire.Setpoint = &state.ClimateValue_SetpointRangeCelsius{SetpointRangeCelsius: &state.ClimateSetpointRange{LowCelsius: &lower, HighCelsius: &upper}}
		case domain.ClimateSetpointUnspecified:
			return nil, ErrInvalidWireState
		default:
			return nil, ErrInvalidWireState
		}
	}
	if input.Mode != nil {
		mode, err := WireClimateMode(*input.Mode)
		if err != nil {
			return nil, err
		}
		wire.Mode = &mode
	}
	if input.Activity != nil {
		activity, err := WireClimateActivity(*input.Activity)
		if err != nil {
			return nil, err
		}
		wire.Activity = &activity
	}
	return wire, nil
}

func validClimateTemperatureBounds(bounds *registry.NumberBounds) bool {
	if bounds == nil {
		return true
	}
	if !validBounds(bounds) {
		return false
	}
	_, err := domain.NewCelsiusTemperature(bounds.Minimum)
	return err == nil
}

func validClimateObservations(constraints *registry.ClimateConstraints) bool {
	if !knownFieldsOnly(constraints.ProtoReflect()) || !validModes(constraints.ObservedModeOptions) {
		return false
	}
	if !validClimateFanTargets(constraints.ObservedFanTargetOptions) || !validClimateFanTargets(constraints.FanTargetOptions) {
		return false
	}
	if b := constraints.ObservedHumidityPercent; b != nil && (!validBounds(b) || b.Minimum < 0 || b.Maximum > 100) {
		return false
	}
	if !validClimateCommandRange(constraints.SetpointRangeCelsius) {
		return false
	}
	for _, observation := range []*registry.ClimateTemperatureObservation{constraints.ObservedTemperatureCelsius, constraints.ObservedSetpointCelsius} {
		if observation != nil && !validClimateTemperatureBounds(observation.Bounds) {
			return false
		}
	}
	rangeBounds := constraints.ObservedSetpointRangeCelsius
	if rangeBounds == nil {
		return true
	}
	if !validClimateTemperatureBounds(rangeBounds.LowCelsius) || !validClimateTemperatureBounds(rangeBounds.HighCelsius) {
		return false
	}
	minimum, maximum := rangeBounds.MinimumDeltaCelsius, rangeBounds.MaximumDeltaCelsius
	if (minimum != nil && (!finite(*minimum) || *minimum < 0)) || (maximum != nil && (!finite(*maximum) || *maximum < 0)) || (minimum != nil && maximum != nil && *minimum > *maximum) {
		return false
	}
	if low, high := rangeBounds.LowCelsius, rangeBounds.HighCelsius; low != nil && high != nil {
		// Reject provably disjoint intervals. Steps still apply to each value
		// during admission, without inventing missing device limits.
		if high.Maximum < low.Minimum || (minimum != nil && high.Maximum-low.Minimum < *minimum) || (maximum != nil && high.Minimum-low.Maximum > *maximum) {
			return false
		}
	}
	return true
}

func withinClimateTemperature(temperature domain.CelsiusTemperature, bounds *registry.NumberBounds) bool {
	number, err := temperature.Value()
	return err == nil && (bounds == nil || withinBounds(number, bounds))
}

// AdmitDomainClimate applies only observation declarations. Commandable mode
// options and setpoint bounds never authorize a read-only field implicitly.
func (c *CapabilityCatalog) AdmitDomainClimate(entity domain.EntityID, endpoint string, value domain.ClimateValue) error {
	if !c.ready() {
		return ErrUninitializedCatalog
	}
	id, err := entity.Value()
	if err != nil {
		return err
	}
	input, err := value.Components()
	if err != nil {
		return err
	}
	c.mu.RLock()
	description := c.instances[id][capabilityKey(ClimateSchema, endpoint)]
	c.mu.RUnlock()
	if endpoint == "" || description == nil || description.GetClimate() == nil {
		return ErrUnsupportedClimateObservation
	}
	constraints := description.GetClimate()
	if input.CurrentTemperature != nil {
		if constraints.ObservedTemperatureCelsius == nil {
			return ErrUnsupportedClimateObservation
		}
		if !withinClimateTemperature(*input.CurrentTemperature, constraints.ObservedTemperatureCelsius.Bounds) {
			return ErrClimateObservationOutsideConstraints
		}
	}
	if input.Setpoint != nil {
		kind, _ := input.Setpoint.Kind()
		switch kind {
		case domain.ClimateSetpointSingle:
			if constraints.ObservedSetpointCelsius == nil {
				return ErrUnsupportedClimateObservation
			}
			temperature, _ := input.Setpoint.Single()
			if !withinClimateTemperature(temperature, constraints.ObservedSetpointCelsius.Bounds) {
				return ErrClimateObservationOutsideConstraints
			}
		case domain.ClimateSetpointRange:
			limits := constraints.ObservedSetpointRangeCelsius
			if limits == nil {
				return ErrUnsupportedClimateObservation
			}
			low, high, _ := input.Setpoint.Range()
			lower, _ := low.Value()
			upper, _ := high.Value()
			delta := upper - lower
			if !withinClimateTemperature(low, limits.LowCelsius) || !withinClimateTemperature(high, limits.HighCelsius) || (limits.MinimumDeltaCelsius != nil && delta < *limits.MinimumDeltaCelsius) || (limits.MaximumDeltaCelsius != nil && delta > *limits.MaximumDeltaCelsius) {
				return ErrClimateObservationOutsideConstraints
			}
		case domain.ClimateSetpointUnspecified:
			return ErrClimateObservationOutsideConstraints
		default:
			return ErrClimateObservationOutsideConstraints
		}
	}
	if input.Mode != nil {
		mode, err := WireClimateMode(*input.Mode)
		if err != nil {
			return err
		}
		if len(constraints.ObservedModeOptions) == 0 {
			return ErrUnsupportedClimateObservation
		}
		if !contains(constraints.ObservedModeOptions, mode) {
			return ErrClimateObservationOutsideConstraints
		}
	}
	if input.Activity != nil {
		activity, err := WireClimateActivity(*input.Activity)
		if err != nil {
			return err
		}
		if len(constraints.ActivityOptions) == 0 {
			return ErrUnsupportedClimateObservation
		}
		if !contains(constraints.ActivityOptions, activity) {
			return ErrClimateObservationOutsideConstraints
		}
	}
	if input.Humidity != nil {
		number, _ := input.Humidity.Value()
		if constraints.ObservedHumidityPercent == nil {
			return ErrUnsupportedClimateObservation
		}
		if !withinBounds(number, constraints.ObservedHumidityPercent) {
			return ErrClimateObservationOutsideConstraints
		}
	}
	if input.FanTarget != nil {
		target, err := WireClimateFanTarget(*input.FanTarget)
		if err != nil {
			return err
		}
		if len(constraints.ObservedFanTargetOptions) == 0 {
			return ErrUnsupportedClimateObservation
		}
		if !contains(constraints.ObservedFanTargetOptions, target) {
			return ErrClimateObservationOutsideConstraints
		}
	}
	if input.FanActive != nil && !constraints.ObservesFanActivity {
		return ErrUnsupportedClimateObservation
	}
	return nil
}

func AdmitWireClimateFanTarget(target registry.ClimateFanTarget) (domain.ClimateFanTarget, error) {
	switch target {
	case registry.ClimateFanTarget_CLIMATE_FAN_TARGET_ON:
		return domain.NewClimateFanTarget(false), nil
	case registry.ClimateFanTarget_CLIMATE_FAN_TARGET_AUTO:
		return domain.NewClimateFanTarget(true), nil
	case registry.ClimateFanTarget_CLIMATE_FAN_TARGET_UNSPECIFIED:
	default:
	}
	return domain.ClimateFanTarget{}, ErrInvalidWireState
}
func WireClimateFanTarget(target domain.ClimateFanTarget) (registry.ClimateFanTarget, error) {
	automatic, err := target.Automatic()
	if err != nil {
		return registry.ClimateFanTarget_CLIMATE_FAN_TARGET_UNSPECIFIED, err
	}
	if automatic {
		return registry.ClimateFanTarget_CLIMATE_FAN_TARGET_AUTO, nil
	}
	return registry.ClimateFanTarget_CLIMATE_FAN_TARGET_ON, nil
}
func validClimateFanTargets(options []registry.ClimateFanTarget) bool {
	seen := make(map[registry.ClimateFanTarget]bool, len(options))
	for _, value := range options {
		if _, err := AdmitWireClimateFanTarget(value); err != nil || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}
func validClimateCommandRange(limits *registry.ClimateSetpointRangeConstraints) bool {
	if limits == nil {
		return true
	}
	if !knownFieldsOnly(limits.ProtoReflect()) || limits.LowCelsius == nil || limits.HighCelsius == nil || !validClimateTemperatureBounds(limits.LowCelsius) || !validClimateTemperatureBounds(limits.HighCelsius) {
		return false
	}
	minimum, maximum := limits.MinimumDeltaCelsius, limits.MaximumDeltaCelsius
	if minimum != nil && (!finite(*minimum) || *minimum < 0) || maximum != nil && (!finite(*maximum) || *maximum < 0) || minimum != nil && maximum != nil && *minimum > *maximum {
		return false
	}
	low, high := limits.LowCelsius, limits.HighCelsius
	return high.Maximum >= low.Minimum && (minimum == nil || high.Maximum-low.Minimum >= *minimum) && (maximum == nil || high.Minimum-low.Maximum <= *maximum)
}
func admitClimateThresholds(low, high *float64, limits *registry.ClimateSetpointRangeConstraints) bool {
	if low == nil && high == nil {
		return true
	}
	if low == nil || high == nil || limits == nil || *low > *high || !withinBounds(*low, limits.LowCelsius) || !withinBounds(*high, limits.HighCelsius) {
		return false
	}
	delta := *high - *low
	return (limits.MinimumDeltaCelsius == nil || delta >= *limits.MinimumDeltaCelsius) && (limits.MaximumDeltaCelsius == nil || delta <= *limits.MaximumDeltaCelsius)
}
