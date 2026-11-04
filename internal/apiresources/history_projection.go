package apiresources

import (
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"google.golang.org/protobuf/proto"
)

// ProjectHistoricalState projects an already authorized recorder observation.
// It does not consult today's declarations: historical units, absence and
// quality retain their original interpretation, independently of live traits.
// Native source/session/entity identifiers are deliberately not projected.
func ProjectHistoricalState(recorded *state.ObservedState) (*api.HistoricalState, error) {
	if contracts.AdmitHistoricalState(recorded) != nil {
		return nil, ErrProjection
	}
	raw := proto.CloneOf(recorded)
	q, err := quality(raw.Quality)
	if err != nil {
		return nil, err
	}
	out := &api.HistoricalState{
		Quality: q, ObservedTime: raw.ObservedAt, ReceiveTime: raw.ReceivedAt,
		LastChangeTime: raw.LastChanged, LastUpdateTime: raw.LastUpdated, LastReportTime: raw.LastReported,
	}
	switch value := raw.Value.(type) {
	case nil:
	case *state.ObservedState_SwitchOn:
		out.Value = &api.HistoricalState_SwitchOn{SwitchOn: value.SwitchOn}
	case *state.ObservedState_Measurement:
		v := value.Measurement
		out.Value = &api.HistoricalState_Measurement{Measurement: &api.HistoricalMeasurement{Value: v.Value, Unit: v.Unit, DecimalValue: v.DecimalValue}}
	case *state.ObservedState_Light:
		v := value.Light
		out.Value = &api.HistoricalState_Light{Light: &api.HistoricalLight{On: v.On, BrightnessPercent: v.BrightnessPercent, ColorTempMireds: v.ColorTempMireds}}
	case *state.ObservedState_Fan:
		speed, err := fanSpeed(value.Fan.Speed)
		if err != nil {
			return nil, err
		}
		out.Value = &api.HistoricalState_FanSpeed{FanSpeed: speed}
	case *state.ObservedState_Climate:
		converted, err := historicalTemperature(value.Climate)
		if err != nil {
			return nil, err
		}
		out.Value = &api.HistoricalState_TemperatureControl{TemperatureControl: converted}
	case *state.ObservedState_Helper:
		converted, err := convertHelperValue(value.Helper)
		if err != nil {
			return nil, err
		}
		out.Value = &api.HistoricalState_Helper{Helper: converted}
	case *state.ObservedState_Solar:
		converted, err := convertSolarValue(value.Solar)
		if err != nil {
			return nil, err
		}
		out.Value = &api.HistoricalState_Solar{Solar: converted}
	case *state.ObservedState_SolarEvent:
		converted, err := convertSolarEventValue(value.SolarEvent)
		if err != nil {
			return nil, err
		}
		out.Value = &api.HistoricalState_SolarEvent{SolarEvent: converted}
	case *state.ObservedState_Cover:
		converted, err := convertCoverValue(value.Cover)
		if err != nil {
			return nil, err
		}
		out.Value = &api.HistoricalState_Cover{Cover: converted}
	case *state.ObservedState_Lock:
		converted, err := convertLockValue(value.Lock)
		if err != nil {
			return nil, err
		}
		out.Value = &api.HistoricalState_Lock{Lock: converted}
	case *state.ObservedState_Alarm:
		converted, err := convertAlarmValue(value.Alarm)
		if err != nil {
			return nil, err
		}
		out.Value = &api.HistoricalState_Alarm{Alarm: converted}
	case *state.ObservedState_Weather:
		converted, err := convertWeatherValue(value.Weather)
		if err != nil {
			return nil, err
		}
		out.Value = &api.HistoricalState_Weather{Weather: converted}
	case *state.ObservedState_Presence:
		converted, err := convertPresenceValue(value.Presence)
		if err != nil {
			return nil, err
		}
		out.Value = &api.HistoricalState_Presence{Presence: converted}
	case *state.ObservedState_PresenceDetails:
		converted, err := convertPresenceDetailsValue(value.PresenceDetails)
		if err != nil {
			return nil, err
		}
		out.Value = &api.HistoricalState_PresenceDetails{PresenceDetails: converted}
	default:
		return nil, ErrProjection
	}
	return out, nil
}

func historicalTemperature(value *state.ClimateValue) (*api.HistoricalTemperatureControl, error) {
	if value == nil {
		return nil, ErrProjection
	}
	out := &api.HistoricalTemperatureControl{
		CurrentTemperatureCelsius: value.CurrentTemperatureCelsius, FanActive: value.FanActive, HumidityPercent: value.HumidityPercent,
	}
	switch setpoint := value.Setpoint.(type) {
	case nil:
	case *state.ClimateValue_SetpointCelsius:
		out.Setpoint = &api.HistoricalTemperatureControl_SetpointCelsius{SetpointCelsius: setpoint.SetpointCelsius}
	case *state.ClimateValue_SetpointRangeCelsius:
		if setpoint.SetpointRangeCelsius == nil || setpoint.SetpointRangeCelsius.LowCelsius == nil || setpoint.SetpointRangeCelsius.HighCelsius == nil {
			return nil, ErrProjection
		}
		out.Setpoint = &api.HistoricalTemperatureControl_SetpointRangeCelsius{SetpointRangeCelsius: &api.TemperatureRange{LowCelsius: setpoint.SetpointRangeCelsius.LowCelsius, HighCelsius: setpoint.SetpointRangeCelsius.HighCelsius}}
	default:
		return nil, ErrProjection
	}
	if value.Mode != nil {
		v, err := mapped(*value.Mode, temperatureModes)
		if err != nil {
			return nil, err
		}
		out.Mode = &v
	}
	if value.Activity != nil {
		v, err := mapped(*value.Activity, temperatureActivities)
		if err != nil {
			return nil, err
		}
		out.Activity = &v
	}
	if value.FanTarget != nil {
		v, err := mapped(*value.FanTarget, temperatureFanTargets)
		if err != nil {
			return nil, err
		}
		out.FanTarget = &v
	}
	return out, nil
}
