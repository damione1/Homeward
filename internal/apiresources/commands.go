package apiresources

import (
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	controls "github.com/damione1/Homeward/gen/home/controls/v1"
	helpers "github.com/damione1/Homeward/gen/home/helpers/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/types/known/timestamppb"
	"time"
)

// NativeArguments converts one admitted public intention into one private Host
// argument union. It performs no dispatch and confers no permission. The caller
// must resolve Capability again from the current authorized registry capture and
// enforce its definition etag, source catalogue, deadlines and durable admission.
func NativeArguments(c Capability, intent apitypes.Intent) (*actions.ActionArgs, error) {
	schema, _, err := c.source.Values()
	if err != nil {
		return nil, ErrProjection
	}
	args := &actions.ActionArgs{}
	switch v := intent.(type) {
	case apitypes.MediaCommand:
		return mediaNativeArguments(c, v)
	case apitypes.PowerCommand:
		if c.trait != Power || !v.Valid() || !c.value.GetPower().GetSupportsSet() {
			return nil, ErrUnsupportedTrait
		}
		on := v.IsOn()
		switch schema {
		case contracts.SwitchSchema:
			args.Arguments = &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: on}}
		case contracts.LightSchema:
			args.Arguments = &actions.ActionArgs_SetLight{SetLight: &actions.SetLight{On: &on}}
		default:
			return nil, ErrUnsupportedTrait
		}
	case apitypes.LevelCommand:
		if c.trait != Level || schema != contracts.LightSchema || !c.value.GetLevel().GetSupportsSet() {
			return nil, ErrUnsupportedTrait
		}
		p := v.Percentage().Value()
		args.Arguments = &actions.ActionArgs_SetLight{SetLight: &actions.SetLight{BrightnessPercent: &p}}
	case apitypes.LightCommand:
		if c.trait != Light || schema != contracts.LightSchema || !v.Valid() {
			return nil, ErrUnsupportedTrait
		}
		x := v.Input()
		d := c.value.GetLightControl()
		if d == nil || x.On != nil && !d.SupportsPower || x.Brightness != nil && d.BrightnessPercent == nil || x.ColorTemperatureMireds != nil && d.ColorTemperatureMireds == nil || x.Transition != nil && d.TransitionDurationBounds == nil || x.RGB != nil && !d.Rgb || x.Preset != nil && !containsControlChoice(d.Presets, x.Preset.Value()) {
			return nil, ErrUnsupportedTrait
		}
		a := &actions.SetLight{On: x.On, Rgb: contracts.WireRGB(x.RGB)}
		if x.Preset != nil {
			key := x.Preset.Value()
			a.Preset = &key
		}
		if x.Brightness != nil {
			p := x.Brightness.Value()
			a.BrightnessPercent = &p
		}
		if x.ColorTemperatureMireds != nil {
			p := uint32(*x.ColorTemperatureMireds)
			a.ColorTempMireds = &p
		}
		if x.Transition != nil {
			p := float64(*x.Transition) / float64(time.Second)
			a.TransitionSeconds = &p
		}
		args.Arguments = &actions.ActionArgs_SetLight{SetLight: a}
	case apitypes.FanCommand:
		if c.trait != Fan || schema != contracts.FanSchema || !v.Valid() {
			return nil, ErrUnsupportedTrait
		}
		on, speed, level := v.Input()
		d := c.value.GetFanControl()
		if d == nil || on != nil && !d.SupportsPower || speed != nil && !containsControlChoice(d.GetLevel().GetSteps(), speed.Value()) || level != nil && d.GetLevel().GetRange() == nil {
			return nil, ErrUnsupportedTrait
		}
		a := &actions.SetFan{On: on, Level: level}
		if speed != nil {
			key := speed.Value()
			a.Speed = &key
		}
		args.Arguments = &actions.ActionArgs_SetFan{SetFan: a}
	case apitypes.ThermalCommand:
		if c.trait != Thermal || schema != contracts.ClimateSchema || !v.Valid() {
			return nil, ErrUnsupportedTrait
		}
		x := v.Input()
		a := &actions.SetClimate{}
		if x.Setpoint != nil {
			if s, ok := x.Setpoint.Single(); ok {
				p, _ := s.Value()
				a.SetpointCelsius = &p
			} else if l, h, ok := x.Setpoint.Range(); ok {
				lo, _ := l.Value()
				hi, _ := h.Value()
				a.HeatThresholdCelsius = &lo
				a.CoolThresholdCelsius = &hi
			} else {
				return nil, ErrProjection
			}
		}
		if x.Mode != nil {
			p, err := contracts.WireClimateMode(*x.Mode)
			if err != nil {
				return nil, ErrProjection
			}
			a.Mode = &p
		}
		if x.FanTarget != nil {
			p, err := contracts.WireClimateFanTarget(*x.FanTarget)
			if err != nil {
				return nil, ErrProjection
			}
			a.FanTarget = &p
		}
		args.Arguments = &actions.ActionArgs_SetClimate{SetClimate: a}
	case apitypes.CoverCommand:
		if c.trait != Cover || schema != contracts.CoverSchema || !v.Valid() {
			return nil, ErrUnsupportedTrait
		}
		a := &controls.CoverAction{}
		if p, ok := v.Position(); ok {
			a.Command = &controls.CoverAction_PositionPercent{PositionPercent: p.Value()}
		} else {
			op, _ := v.Operation()
			switch op {
			case apitypes.CoverOpen:
				a.Command = &controls.CoverAction_Operation{Operation: controls.CoverOperation_COVER_OPERATION_OPEN}
			case apitypes.CoverClose:
				a.Command = &controls.CoverAction_Operation{Operation: controls.CoverOperation_COVER_OPERATION_CLOSE}
			case apitypes.CoverStop:
				a.Command = &controls.CoverAction_Operation{Operation: controls.CoverOperation_COVER_OPERATION_STOP}
			default:
				return nil, ErrProjection
			}
		}
		args.Arguments = &actions.ActionArgs_Cover{Cover: a}
	case apitypes.LockCommand:
		if c.trait != Lock || schema != contracts.LockSchema || !v.Valid() {
			return nil, ErrUnsupportedTrait
		}
		a := &controls.LockAction{}
		switch v.Operation() {
		case apitypes.Lock:
			a.Operation = controls.LockOperation_LOCK_OPERATION_LOCK
		case apitypes.Unlock:
			a.Operation = controls.LockOperation_LOCK_OPERATION_UNLOCK
		default:
			return nil, ErrProjection
		}
		args.Arguments = &actions.ActionArgs_Lock{Lock: a}
	case apitypes.AlarmCommand:
		if c.trait != Alarm || schema != contracts.AlarmSchema || !v.Valid() {
			return nil, ErrUnsupportedTrait
		}
		a := &controls.AlarmAction{}
		switch v.Operation() {
		case apitypes.AlarmArmHome:
			a.Operation = controls.AlarmOperation_ALARM_OPERATION_ARM_HOME
		case apitypes.AlarmArmAway:
			a.Operation = controls.AlarmOperation_ALARM_OPERATION_ARM_AWAY
		case apitypes.AlarmArmNight:
			a.Operation = controls.AlarmOperation_ALARM_OPERATION_ARM_NIGHT
		case apitypes.AlarmDisarm:
			a.Operation = controls.AlarmOperation_ALARM_OPERATION_DISARM
		default:
			return nil, ErrProjection
		}
		args.Arguments = &actions.ActionArgs_Alarm{Alarm: a}
	case apitypes.HelperCommand:
		if c.trait != Helper || schema != contracts.HelperSchema || !v.Valid() {
			return nil, ErrUnsupportedTrait
		}
		op, value, revision := v.Input()
		r := uint64(revision)
		a := &helpers.HelperAction{ExpectedHelperRevision: &r}
		switch op {
		case apitypes.HelperSet:
			a.Operation = helpers.HelperOperation_HELPER_OPERATION_SET
		case apitypes.HelperToggle:
			a.Operation = helpers.HelperOperation_HELPER_OPERATION_TOGGLE
		case apitypes.HelperIncrement:
			a.Operation = helpers.HelperOperation_HELPER_OPERATION_INCREMENT
		case apitypes.HelperDecrement:
			a.Operation = helpers.HelperOperation_HELPER_OPERATION_DECREMENT
		case apitypes.HelperReset:
			a.Operation = helpers.HelperOperation_HELPER_OPERATION_RESET
		case apitypes.HelperPress:
			a.Operation = helpers.HelperOperation_HELPER_OPERATION_PRESS
		case apitypes.HelperNext:
			a.Operation = helpers.HelperOperation_HELPER_OPERATION_NEXT
		case apitypes.HelperPrevious:
			a.Operation = helpers.HelperOperation_HELPER_OPERATION_PREVIOUS
		default:
			return nil, ErrProjection
		}
		if value != nil {
			a.Value, err = nativeHelperValue(*value)
			if err != nil {
				return nil, err
			}
		}
		args.Arguments = &actions.ActionArgs_Helper{Helper: a}
	default:
		return nil, ErrUnsupportedTrait
	}
	return args, nil
}
func nativeHelperValue(value domain.HelperValue) (*helpers.HelperValue, error) {
	kind, err := value.Kind()
	if err != nil {
		return nil, ErrProjection
	}
	r := &helpers.HelperValue{}
	switch kind {
	case domain.HelperBoolean:
		x, _ := value.Boolean()
		r.Value = &helpers.HelperValue_Boolean{Boolean: x}
	case domain.HelperNumber:
		x, _ := value.Number()
		r.Value = &helpers.HelperValue_Number{Number: x}
	case domain.HelperCounter:
		x, _ := value.Counter()
		r.Value = &helpers.HelperValue_Counter{Counter: x}
	case domain.HelperButtonPress:
		x, _ := value.ButtonPress()
		r.Value = &helpers.HelperValue_ButtonPressedAt{ButtonPressedAt: timestamppb.New(x)}
	case domain.HelperDate:
		x, _ := value.Date()
		r.Value = &helpers.HelperValue_Date{Date: nativeDate(x)}
	case domain.HelperTime:
		x, _ := value.Time()
		r.Value = &helpers.HelperValue_Time{Time: nativeTime(x)}
	case domain.HelperDateTime:
		x, _ := value.DateTime()
		d, t, err := x.Components()
		if err != nil {
			return nil, ErrProjection
		}
		r.Value = &helpers.HelperValue_Datetime{Datetime: &helpers.CivilDateTime{Date: nativeDate(d), Time: nativeTime(t)}}
	case domain.HelperSelect:
		x, _ := value.Select()
		r.Value = &helpers.HelperValue_SelectedOption{SelectedOption: x}
	case domain.HelperText:
		x, _ := value.Text()
		r.Value = &helpers.HelperValue_Text{Text: x}
	case domain.HelperInvalid:
		return nil, ErrProjection
	default:
		return nil, ErrProjection
	}
	return r, nil
}
func nativeDate(v domain.CivilDate) *helpers.CivilDate {
	y, m, d, _ := v.Components()
	return &helpers.CivilDate{Year: uint32(y), Month: uint32(m), Day: uint32(d)}
}
func nativeTime(v domain.CivilTime) *helpers.CivilTime {
	h, m, s, _ := v.Components()
	return &helpers.CivilTime{Hour: uint32(h), Minute: uint32(m), Second: uint32(s)}
}

func containsSelector(options []string, key string) bool {
	for _, option := range options {
		if option == key {
			return true
		}
	}
	return false
}
