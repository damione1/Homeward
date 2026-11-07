package apitypes

import (
	"github.com/damione1/Homeward/internal/domain"
	"strings"
	"time"
	"unicode/utf8"
)

func copied[T any](v *T) *T {
	if v == nil {
		return nil
	}
	x := *v
	return &x
}

// LightCommand retains one source-atomic intention. All fields are privately
// copied; an empty target or a transition without a target is never executable.
type LightInput struct {
	On                     *bool
	Brightness             *Percentage
	ColorTemperatureMireds *int32
	Transition             *time.Duration
}
type LightCommand struct {
	input LightInput
	valid bool
}

func NewLightCommand(v LightInput) (LightCommand, error) {
	if v.On == nil && v.Brightness == nil && v.ColorTemperatureMireds == nil {
		return LightCommand{}, ErrIntent
	}
	if v.ColorTemperatureMireds != nil && (*v.ColorTemperatureMireds <= 0 || *v.ColorTemperatureMireds > 1000) {
		return LightCommand{}, ErrIntent
	}
	var brightness *float64
	if v.Brightness != nil {
		x := v.Brightness.Value()
		brightness = &x
	}
	var color *uint32
	if v.ColorTemperatureMireds != nil {
		x := uint32(*v.ColorTemperatureMireds)
		color = &x
	}
	if _, err := domain.NewLightValue(false, brightness, color); err != nil {
		return LightCommand{}, ErrIntent
	}
	if v.Transition != nil && (*v.Transition < 0 || *v.Transition > 6553*time.Second) {
		return LightCommand{}, ErrIntent
	}
	if v.Transition != nil && v.On != nil && !*v.On && (v.Brightness != nil || v.ColorTemperatureMireds != nil) {
		return LightCommand{}, ErrIntent
	}
	return LightCommand{LightInput{copied(v.On), copied(v.Brightness), copied(v.ColorTemperatureMireds), copied(v.Transition)}, true}, nil
}
func (LightCommand) apiIntent() {}
func (v LightCommand) Input() LightInput {
	return LightInput{copied(v.input.On), copied(v.input.Brightness), copied(v.input.ColorTemperatureMireds), copied(v.input.Transition)}
}
func (v LightCommand) Valid() bool { _, err := NewLightCommand(v.input); return v.valid && err == nil }

// SelectorKey is open catalogue data. Domain support is checked against the
// current source definition at dispatch; spelling is never normalized.
type SelectorKey struct{ value string }

func NewSelectorKey(v string) (SelectorKey, error) {
	if len(v) == 0 || len(v) > 16384 || !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
		return SelectorKey{}, ErrIntent
	}
	return SelectorKey{v}, nil
}
func (v SelectorKey) Value() string { return v.value }

type FanCommand struct {
	on    *bool
	speed *SelectorKey
	valid bool
}

func NewFanCommand(on *bool, speed *SelectorKey) (FanCommand, error) {
	if on == nil && speed == nil || speed != nil && speed.value == "" {
		return FanCommand{}, ErrIntent
	}
	return FanCommand{copied(on), copied(speed), true}, nil
}
func (FanCommand) apiIntent()                     {}
func (v FanCommand) Input() (*bool, *SelectorKey) { return copied(v.on), copied(v.speed) }
func (v FanCommand) Valid() bool {
	_, err := NewFanCommand(v.on, v.speed)
	return v.valid && err == nil
}

// ThermalCommand reuses admitted domain temperatures and the atomic setpoint
// sum; mode and fan policy accompany that same physical intention.
type ThermalCommand struct {
	value domain.ClimateValue
	valid bool
}

func NewThermalCommand(setpoint *domain.ClimateSetpoint, mode *domain.ClimateMode, fan *domain.ClimateFanTarget) (ThermalCommand, error) {
	value, err := domain.NewClimateValue(domain.ClimateValueInput{Setpoint: setpoint, Mode: mode, FanTarget: fan})
	if err != nil {
		return ThermalCommand{}, ErrIntent
	}
	return ThermalCommand{value, true}, nil
}
func (ThermalCommand) apiIntent()                        {}
func (v ThermalCommand) Input() domain.ClimateValueInput { x, _ := v.value.Components(); return x }
func (v ThermalCommand) Valid() bool                     { _, err := v.value.Components(); return v.valid && err == nil }

type CoverOperation uint8

const (
	CoverOpen CoverOperation = iota + 1
	CoverClose
	CoverStop
)

type CoverCommand struct {
	operation CoverOperation
	position  *Percentage
	valid     bool
}

func NewCoverOperation(v CoverOperation) (CoverCommand, error) {
	switch v {
	case CoverOpen, CoverClose, CoverStop:
		return CoverCommand{operation: v, valid: true}, nil
	default:
		return CoverCommand{}, ErrIntent
	}
}
func NewCoverPosition(v Percentage) CoverCommand { return CoverCommand{position: &v, valid: true} }
func (CoverCommand) apiIntent()                  {}
func (v CoverCommand) Operation() (CoverOperation, bool) {
	return v.operation, v.valid && v.position == nil
}
func (v CoverCommand) Position() (Percentage, bool) {
	if v.position == nil {
		return Percentage{}, false
	}
	return *v.position, v.valid
}
func (v CoverCommand) Valid() bool {
	if !v.valid {
		return false
	}
	if v.position != nil {
		return v.operation == 0
	}
	_, err := NewCoverOperation(v.operation)
	return err == nil
}

type LockOperation uint8

const (
	Lock LockOperation = iota + 1
	Unlock
)

type LockCommand struct{ operation LockOperation }

func NewLockCommand(v LockOperation) (LockCommand, error) {
	switch v {
	case Lock, Unlock:
		return LockCommand{v}, nil
	default:
		return LockCommand{}, ErrIntent
	}
}
func (LockCommand) apiIntent()                 {}
func (v LockCommand) Operation() LockOperation { return v.operation }
func (v LockCommand) Valid() bool              { _, err := NewLockCommand(v.operation); return err == nil }

type AlarmOperation uint8

const (
	AlarmArmHome AlarmOperation = iota + 1
	AlarmArmAway
	AlarmDisarm
	AlarmArmNight
)

type AlarmCommand struct{ operation AlarmOperation }

func NewAlarmCommand(v AlarmOperation) (AlarmCommand, error) {
	switch v {
	case AlarmArmHome, AlarmArmAway, AlarmDisarm, AlarmArmNight:
		return AlarmCommand{v}, nil
	default:
		return AlarmCommand{}, ErrIntent
	}
}
func (AlarmCommand) apiIntent()                  {}
func (v AlarmCommand) Operation() AlarmOperation { return v.operation }
func (v AlarmCommand) Valid() bool               { _, err := NewAlarmCommand(v.operation); return err == nil }

type HelperOperation uint8

const (
	HelperSet HelperOperation = iota + 1
	HelperToggle
	HelperIncrement
	HelperDecrement
	HelperReset
	HelperPress
	HelperNext
	HelperPrevious
)

type HelperCommand struct {
	operation HelperOperation
	value     *domain.HelperValue
	revision  int64
	valid     bool
}

func NewHelperCommand(operation HelperOperation, value *domain.HelperValue, revision int64) (HelperCommand, error) {
	if revision < 0 || operation < HelperSet || operation > HelperPrevious {
		return HelperCommand{}, ErrIntent
	}
	if operation == HelperSet {
		if value == nil || !value.Valid() {
			return HelperCommand{}, ErrIntent
		}
		kind, _ := value.Kind()
		if kind == domain.HelperButtonPress {
			return HelperCommand{}, ErrIntent
		}
	} else if value != nil {
		return HelperCommand{}, ErrIntent
	}
	return HelperCommand{operation, copied(value), revision, true}, nil
}
func (HelperCommand) apiIntent() {}
func (v HelperCommand) Input() (HelperOperation, *domain.HelperValue, int64) {
	return v.operation, copied(v.value), v.revision
}
func (v HelperCommand) Valid() bool {
	_, err := NewHelperCommand(v.operation, v.value, v.revision)
	return v.valid && err == nil
}
