package domain

import "math"

// CelsiusTemperature has a fixed unit, matching the climate command contract.
// Device limits belong to the capability instance; only the physical lower
// limit and finitude are imposed here. Its Go zero value is not admitted.
type CelsiusTemperature struct {
	value float64
	valid bool
}

func NewCelsiusTemperature(value float64) (CelsiusTemperature, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < -273.15 {
		return CelsiusTemperature{}, ErrInvalidState
	}
	return CelsiusTemperature{value: value, valid: true}, nil
}

func (t CelsiusTemperature) Value() (float64, error) {
	if !t.valid {
		return 0, ErrInvalidState
	}
	if _, err := NewCelsiusTemperature(t.value); err != nil {
		return 0, err
	}
	return t.value, nil
}

// ClimateMode is the requested operating mode, never proof of active equipment.
// The registry enum is the wire authority; contracts converts it explicitly.
type ClimateMode uint8

const (
	ClimateModeUnspecified ClimateMode = iota
	ClimateModeOff
	ClimateModeHeat
	ClimateModeCool
	ClimateModeAuto
	ClimateModeDry
	ClimateModeFanOnly
)

func (m ClimateMode) valid() bool {
	switch m {
	case ClimateModeOff, ClimateModeHeat, ClimateModeCool, ClimateModeAuto, ClimateModeDry, ClimateModeFanOnly:
		return true
	case ClimateModeUnspecified:
		return false
	default:
		return false
	}
}

// ClimateActivity is an observed physical activity. An absent activity does not
// imply idle, and a requested mode does not establish this observation.
type ClimateActivity uint8

const (
	ClimateActivityUnspecified ClimateActivity = iota
	ClimateActivityIdle
	ClimateActivityHeating
	ClimateActivityCooling
	ClimateActivityDrying
	ClimateActivityFanOnly
)

func (a ClimateActivity) valid() bool {
	switch a {
	case ClimateActivityIdle, ClimateActivityHeating, ClimateActivityCooling, ClimateActivityDrying, ClimateActivityFanOnly:
		return true
	case ClimateActivityUnspecified:
		return false
	default:
		return false
	}
}

type ClimateSetpointKind uint8

const (
	ClimateSetpointUnspecified ClimateSetpointKind = iota
	ClimateSetpointSingle
	ClimateSetpointRange
)

// ClimateSetpoint is a closed, comparable sum. A range always has two admitted
// temperatures; the optional setpoint on ClimateValue represents its absence.
type ClimateSetpoint struct {
	kind ClimateSetpointKind
	low  CelsiusTemperature
	high CelsiusTemperature
}

func NewSingleClimateSetpoint(value CelsiusTemperature) (ClimateSetpoint, error) {
	if _, err := value.Value(); err != nil {
		return ClimateSetpoint{}, err
	}
	return ClimateSetpoint{kind: ClimateSetpointSingle, low: value}, nil
}

func NewRangeClimateSetpoint(low, high CelsiusTemperature) (ClimateSetpoint, error) {
	lower, err := low.Value()
	if err != nil {
		return ClimateSetpoint{}, err
	}
	upper, err := high.Value()
	if err != nil || lower > upper {
		return ClimateSetpoint{}, ErrInvalidState
	}
	return ClimateSetpoint{kind: ClimateSetpointRange, low: low, high: high}, nil
}

func (s ClimateSetpoint) Kind() (ClimateSetpointKind, error) {
	switch s.kind {
	case ClimateSetpointSingle:
		if _, err := s.low.Value(); err == nil && s.high == (CelsiusTemperature{}) {
			return ClimateSetpointSingle, nil
		}
	case ClimateSetpointRange:
		if _, err := NewRangeClimateSetpoint(s.low, s.high); err == nil {
			return ClimateSetpointRange, nil
		}
	case ClimateSetpointUnspecified:
	default:
	}
	return ClimateSetpointUnspecified, ErrInvalidState
}

func (s ClimateSetpoint) Single() (CelsiusTemperature, bool) {
	kind, err := s.Kind()
	return s.low, err == nil && kind == ClimateSetpointSingle
}

func (s ClimateSetpoint) Range() (CelsiusTemperature, CelsiusTemperature, bool) {
	kind, err := s.Kind()
	return s.low, s.high, err == nil && kind == ClimateSetpointRange
}

// ClimateValueInput is untrusted construction data. Every pointer is copied
// during admission and Components returns fresh copies of present fields.
type ClimateValueInput struct {
	CurrentTemperature *CelsiusTemperature
	Setpoint           *ClimateSetpoint
	Mode               *ClimateMode
	Activity           *ClimateActivity
	FanActive          *bool
}

// ClimateValue remains comparable, so the shared state transition clock compares
// all values and presences without a second climate-specific state authority.
type ClimateValue struct {
	current      CelsiusTemperature
	setpoint     ClimateSetpoint
	mode         ClimateMode
	activity     ClimateActivity
	fanActive    bool
	hasFanActive bool
	valid        bool
}

func NewClimateValue(input ClimateValueInput) (ClimateValue, error) {
	if input.CurrentTemperature == nil && input.Setpoint == nil && input.Mode == nil && input.Activity == nil && input.FanActive == nil {
		return ClimateValue{}, ErrInvalidState
	}
	v := ClimateValue{valid: true}
	if input.CurrentTemperature != nil {
		if _, err := input.CurrentTemperature.Value(); err != nil {
			return ClimateValue{}, err
		}
		v.current = *input.CurrentTemperature
	}
	if input.Setpoint != nil {
		if _, err := input.Setpoint.Kind(); err != nil {
			return ClimateValue{}, err
		}
		v.setpoint = *input.Setpoint
	}
	if input.Mode != nil {
		if !input.Mode.valid() {
			return ClimateValue{}, ErrInvalidState
		}
		v.mode = *input.Mode
	}
	if input.Activity != nil {
		if !input.Activity.valid() {
			return ClimateValue{}, ErrInvalidState
		}
		v.activity = *input.Activity
	}
	if input.FanActive != nil {
		v.fanActive, v.hasFanActive = *input.FanActive, true
	}
	return v, nil
}

func (v ClimateValue) Components() (ClimateValueInput, error) {
	if !v.valid {
		return ClimateValueInput{}, ErrInvalidState
	}
	var input ClimateValueInput
	if v.current != (CelsiusTemperature{}) {
		current := v.current
		input.CurrentTemperature = &current
	}
	if v.setpoint != (ClimateSetpoint{}) {
		setpoint := v.setpoint
		input.Setpoint = &setpoint
	}
	if v.mode != ClimateModeUnspecified {
		mode := v.mode
		input.Mode = &mode
	}
	if v.activity != ClimateActivityUnspecified {
		activity := v.activity
		input.Activity = &activity
	}
	if v.hasFanActive {
		active := v.fanActive
		input.FanActive = &active
	}
	if admitted, err := NewClimateValue(input); err != nil || admitted != v {
		return ClimateValueInput{}, ErrInvalidState
	}
	return input, nil
}
