package apiv2

import (
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/genproto/googleapis/type/date"
	"google.golang.org/genproto/googleapis/type/timeofday"
	"time"
)

func ParseTemperatureMode(v wire.TemperatureMode) (domain.ClimateMode, error) {
	switch v {
	case wire.TemperatureMode_TEMPERATURE_MODE_OFF:
		return domain.ClimateModeOff, nil
	case wire.TemperatureMode_TEMPERATURE_MODE_HEAT:
		return domain.ClimateModeHeat, nil
	case wire.TemperatureMode_TEMPERATURE_MODE_COOL:
		return domain.ClimateModeCool, nil
	case wire.TemperatureMode_TEMPERATURE_MODE_AUTO:
		return domain.ClimateModeAuto, nil
	case wire.TemperatureMode_TEMPERATURE_MODE_DRY:
		return domain.ClimateModeDry, nil
	case wire.TemperatureMode_TEMPERATURE_MODE_FAN_ONLY:
		return domain.ClimateModeFanOnly, nil
	case wire.TemperatureMode_TEMPERATURE_MODE_UNSPECIFIED:
		return 0, reject("temperature_control.mode", MissingIntent)
	default:
		return 0, reject("temperature_control.mode", UnknownValue)
	}
}
func ParseTemperatureFanTarget(v wire.TemperatureFanTarget) (domain.ClimateFanTarget, error) {
	switch v {
	case wire.TemperatureFanTarget_TEMPERATURE_FAN_TARGET_ON:
		return domain.NewClimateFanTarget(false), nil
	case wire.TemperatureFanTarget_TEMPERATURE_FAN_TARGET_AUTO:
		return domain.NewClimateFanTarget(true), nil
	case wire.TemperatureFanTarget_TEMPERATURE_FAN_TARGET_UNSPECIFIED:
		return domain.ClimateFanTarget{}, reject("temperature_control.fan_target", MissingIntent)
	default:
		return domain.ClimateFanTarget{}, reject("temperature_control.fan_target", UnknownValue)
	}
}
func parseLight(v *wire.SetLightCommand) (apitypes.Intent, error) {
	if v == nil {
		return nil, reject("light", MissingIntent)
	}
	input := apitypes.LightInput{ColorTemperatureMireds: v.ColorTemperatureMireds}
	if v.Power != nil {
		p, err := ParsePower(v.Power.Value)
		if err != nil {
			violation := err.(*Violation)
			return nil, reject("light.power.value", violation.Kind())
		}
		on := p.IsOn()
		input.On = &on
	}
	if v.Level != nil {
		target, ok := v.Level.Target.(*wire.SetLevelCommand_Percentage)
		if !ok || target == nil {
			return nil, reject("light.level", MissingIntent)
		}
		x, err := apitypes.NewPercentage(target.Percentage)
		if err != nil {
			return nil, reject("light.level.percentage", OutOfRange)
		}
		input.Brightness = &x
	}
	if v.ColorTemperatureMireds != nil && (*v.ColorTemperatureMireds <= 0 || *v.ColorTemperatureMireds > 1000) {
		return nil, reject("light.color_temperature_mireds", OutOfRange)
	}
	if v.TransitionDuration != nil {
		if v.TransitionDuration.CheckValid() != nil || v.TransitionDuration.Seconds < 0 || v.TransitionDuration.Nanos < 0 || v.TransitionDuration.Seconds > 6553 || v.TransitionDuration.Seconds == 6553 && v.TransitionDuration.Nanos > 0 {
			return nil, reject("light.transition_duration", OutOfRange)
		}
		x := time.Duration(v.TransitionDuration.Seconds)*time.Second + time.Duration(v.TransitionDuration.Nanos)
		input.Transition = &x
	}
	if input.Transition != nil && input.On != nil && !*input.On && (input.Brightness != nil || input.ColorTemperatureMireds != nil) {
		return nil, reject("light", wire.ValidationReason_VALIDATION_REASON_INCONSISTENT_FIELDS)
	}
	result, err := apitypes.NewLightCommand(input)
	if err != nil {
		return nil, reject("light", MissingIntent)
	}
	return result, nil
}
func parseFan(v *wire.SetFanCommand) (apitypes.Intent, error) {
	if v == nil {
		return nil, reject("fan", MissingIntent)
	}
	var key *apitypes.SelectorKey
	if v.Speed != nil {
		x, err := apitypes.NewSelectorKey(*v.Speed)
		if err != nil {
			return nil, reject("fan.speed", OutOfRange)
		}
		key = &x
	}
	var on *bool
	if v.Power != nil {
		p, err := ParsePower(v.Power.Value)
		if err != nil {
			violation := err.(*Violation)
			return nil, reject("fan.power.value", violation.Kind())
		}
		x := p.IsOn()
		on = &x
	}
	result, err := apitypes.NewFanCommand(on, key)
	if err != nil {
		return nil, reject("fan", MissingIntent)
	}
	return result, nil
}
func parseThermal(v *wire.SetTemperatureControlCommand) (apitypes.Intent, error) {
	if v == nil {
		return nil, reject("temperature_control", MissingIntent)
	}
	var setpoint *domain.ClimateSetpoint
	switch x := v.Setpoint.(type) {
	case *wire.SetTemperatureControlCommand_SetpointCelsius:
		if x == nil {
			return nil, reject("temperature_control.setpoint_celsius", MissingIntent)
		}
		t, err := domain.NewCelsiusTemperature(x.SetpointCelsius)
		if err != nil {
			return nil, reject("temperature_control.setpoint_celsius", OutOfRange)
		}
		s, _ := domain.NewSingleClimateSetpoint(t)
		setpoint = &s
	case *wire.SetTemperatureControlCommand_SetpointRangeCelsius:
		if x == nil || x.SetpointRangeCelsius == nil || x.SetpointRangeCelsius.LowCelsius == nil || x.SetpointRangeCelsius.HighCelsius == nil {
			return nil, reject("temperature_control.setpoint_range_celsius", MissingIntent)
		}
		l, le := domain.NewCelsiusTemperature(*x.SetpointRangeCelsius.LowCelsius)
		h, he := domain.NewCelsiusTemperature(*x.SetpointRangeCelsius.HighCelsius)
		if le != nil || he != nil {
			return nil, reject("temperature_control.setpoint_range_celsius", OutOfRange)
		}
		s, err := domain.NewRangeClimateSetpoint(l, h)
		if err != nil {
			return nil, reject("temperature_control.setpoint_range_celsius", OutOfRange)
		}
		setpoint = &s
	case nil:
	default:
		return nil, reject("temperature_control", UnknownValue)
	}
	var mode *domain.ClimateMode
	if v.Mode != nil {
		x, err := ParseTemperatureMode(*v.Mode)
		if err != nil {
			return nil, err
		}
		mode = &x
	}
	var fan *domain.ClimateFanTarget
	if v.FanTarget != nil {
		x, err := ParseTemperatureFanTarget(*v.FanTarget)
		if err != nil {
			return nil, err
		}
		fan = &x
	}
	result, err := apitypes.NewThermalCommand(setpoint, mode, fan)
	if err != nil {
		return nil, reject("temperature_control", MissingIntent)
	}
	return result, nil
}
func ParseCoverOperation(v wire.CoverOperation) (apitypes.CoverOperation, error) {
	switch v {
	case wire.CoverOperation_COVER_OPERATION_OPEN:
		return apitypes.CoverOpen, nil
	case wire.CoverOperation_COVER_OPERATION_CLOSE:
		return apitypes.CoverClose, nil
	case wire.CoverOperation_COVER_OPERATION_STOP:
		return apitypes.CoverStop, nil
	case wire.CoverOperation_COVER_OPERATION_UNSPECIFIED:
		return 0, reject("cover.operation", MissingIntent)
	default:
		return 0, reject("cover.operation", UnknownValue)
	}
}
func parseCover(v *wire.SetCoverCommand) (apitypes.Intent, error) {
	if v == nil {
		return nil, reject("cover", MissingIntent)
	}
	switch x := v.Target.(type) {
	case *wire.SetCoverCommand_Operation:
		if x == nil {
			return nil, reject("cover.operation", MissingIntent)
		}
		o, err := ParseCoverOperation(x.Operation)
		if err != nil {
			return nil, err
		}
		return apitypes.NewCoverOperation(o)
	case *wire.SetCoverCommand_PositionPercent:
		if x == nil {
			return nil, reject("cover.position_percent", MissingIntent)
		}
		p, err := apitypes.NewPercentage(x.PositionPercent)
		if err != nil {
			return nil, reject("cover.position_percent", OutOfRange)
		}
		return apitypes.NewCoverPosition(p), nil
	default:
		return nil, reject("cover", MissingIntent)
	}
}
func ParseLockOperation(v wire.LockOperation) (apitypes.LockOperation, error) {
	switch v {
	case wire.LockOperation_LOCK_OPERATION_LOCK:
		return apitypes.Lock, nil
	case wire.LockOperation_LOCK_OPERATION_UNLOCK:
		return apitypes.Unlock, nil
	case wire.LockOperation_LOCK_OPERATION_UNSPECIFIED:
		return 0, reject("lock.operation", MissingIntent)
	default:
		return 0, reject("lock.operation", UnknownValue)
	}
}
func parseLock(v *wire.SetLockCommand) (apitypes.Intent, error) {
	if v == nil {
		return nil, reject("lock", MissingIntent)
	}
	o, err := ParseLockOperation(v.Operation)
	if err != nil {
		return nil, err
	}
	return apitypes.NewLockCommand(o)
}
func ParseAlarmOperation(v wire.AlarmOperation) (apitypes.AlarmOperation, error) {
	switch v {
	case wire.AlarmOperation_ALARM_OPERATION_ARM_HOME:
		return apitypes.AlarmArmHome, nil
	case wire.AlarmOperation_ALARM_OPERATION_ARM_AWAY:
		return apitypes.AlarmArmAway, nil
	case wire.AlarmOperation_ALARM_OPERATION_DISARM:
		return apitypes.AlarmDisarm, nil
	case wire.AlarmOperation_ALARM_OPERATION_UNSPECIFIED:
		return 0, reject("alarm.operation", MissingIntent)
	default:
		return 0, reject("alarm.operation", UnknownValue)
	}
}
func parseAlarm(v *wire.SetAlarmCommand) (apitypes.Intent, error) {
	if v == nil {
		return nil, reject("alarm", MissingIntent)
	}
	o, err := ParseAlarmOperation(v.Operation)
	if err != nil {
		return nil, err
	}
	return apitypes.NewAlarmCommand(o)
}
func ParseHelperOperation(v wire.HelperOperation) (apitypes.HelperOperation, error) {
	switch v {
	case wire.HelperOperation_HELPER_OPERATION_SET:
		return apitypes.HelperSet, nil
	case wire.HelperOperation_HELPER_OPERATION_TOGGLE:
		return apitypes.HelperToggle, nil
	case wire.HelperOperation_HELPER_OPERATION_INCREMENT:
		return apitypes.HelperIncrement, nil
	case wire.HelperOperation_HELPER_OPERATION_DECREMENT:
		return apitypes.HelperDecrement, nil
	case wire.HelperOperation_HELPER_OPERATION_RESET:
		return apitypes.HelperReset, nil
	case wire.HelperOperation_HELPER_OPERATION_PRESS:
		return apitypes.HelperPress, nil
	case wire.HelperOperation_HELPER_OPERATION_NEXT:
		return apitypes.HelperNext, nil
	case wire.HelperOperation_HELPER_OPERATION_PREVIOUS:
		return apitypes.HelperPrevious, nil
	case wire.HelperOperation_HELPER_OPERATION_UNSPECIFIED:
		return 0, reject("helper.operation", MissingIntent)
	default:
		return 0, reject("helper.operation", UnknownValue)
	}
}
func civilDate(v *date.Date) (domain.CivilDate, error) {
	if v == nil {
		return domain.CivilDate{}, apitypes.ErrIntent
	}
	return domain.NewCivilDate(int(v.Year), int(v.Month), int(v.Day))
}
func civilTime(v *timeofday.TimeOfDay) (domain.CivilTime, error) {
	if v == nil || v.Nanos != 0 {
		return domain.CivilTime{}, apitypes.ErrIntent
	}
	return domain.NewCivilTime(int(v.Hours), int(v.Minutes), int(v.Seconds))
}
func parseHelperValue(v *wire.HelperInputValue) (domain.HelperValue, error) {
	if v == nil {
		return domain.HelperValue{}, apitypes.ErrIntent
	}
	switch x := v.Value.(type) {
	case *wire.HelperInputValue_BooleanValue:
		if x != nil {
			return domain.NewHelperBoolean(x.BooleanValue), nil
		}
	case *wire.HelperInputValue_Number:
		if x != nil {
			return domain.NewHelperNumber(x.Number)
		}
	case *wire.HelperInputValue_Counter:
		if x != nil {
			return domain.NewHelperCounter(x.Counter), nil
		}
	case *wire.HelperInputValue_Date:
		if x != nil {
			d, err := civilDate(x.Date)
			if err == nil {
				return domain.NewHelperDate(d)
			}
		}
	case *wire.HelperInputValue_Time:
		if x != nil {
			t, err := civilTime(x.Time)
			if err == nil {
				return domain.NewHelperTime(t)
			}
		}
	case *wire.HelperInputValue_LocalValue:
		if x != nil && x.LocalValue != nil {
			d, de := civilDate(x.LocalValue.Date)
			t, te := civilTime(x.LocalValue.Time)
			if de == nil && te == nil {
				dt, _ := domain.NewCivilDateTime(d, t)
				return domain.NewHelperDateTime(dt)
			}
		}
	case *wire.HelperInputValue_SelectedOption:
		if x != nil {
			return domain.NewHelperSelect(x.SelectedOption)
		}
	case *wire.HelperInputValue_Text:
		if x != nil {
			return domain.NewHelperText(x.Text)
		}
	case nil:
	}
	return domain.HelperValue{}, apitypes.ErrIntent
}
func parseHelper(v *wire.SetHelperCommand) (apitypes.Intent, error) {
	if v == nil {
		return nil, reject("helper", MissingIntent)
	}
	operation, err := ParseHelperOperation(v.Operation)
	if err != nil {
		return nil, err
	}
	if v.ExpectedHelperRevision == nil {
		return nil, reject("helper.expected_helper_revision", MissingIntent)
	}
	if *v.ExpectedHelperRevision < 0 {
		return nil, reject("helper.expected_helper_revision", OutOfRange)
	}
	var value *domain.HelperValue
	if v.Value != nil {
		x, err := parseHelperValue(v.Value)
		if err != nil {
			return nil, reject("helper.value", OutOfRange)
		}
		value = &x
	}
	if operation != apitypes.HelperSet && value != nil {
		return nil, reject("helper.value", wire.ValidationReason_VALIDATION_REASON_INCONSISTENT_FIELDS)
	}
	result, err := apitypes.NewHelperCommand(operation, value, *v.ExpectedHelperRevision)
	if err != nil {
		return nil, reject("helper.value", MissingIntent)
	}
	return result, nil
}
