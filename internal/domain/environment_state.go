package domain

import "reflect"

type weatherStatePayload struct{ value WeatherValue }
type presenceStatePayload struct{ value PresenceValue }
type presenceDetailsStatePayload struct{ value PresenceDetailsValue }

func (*weatherStatePayload) nativeStateMarker()         {}
func (*presenceStatePayload) nativeStateMarker()        {}
func (*presenceDetailsStatePayload) nativeStateMarker() {}

func NewWeatherStateValue(value WeatherValue) (StateValue, error) {
	if !value.valid || !validWeatherParts(value.parts) {
		return StateValue{}, ErrInvalidState
	}
	return StateValue{kind: ValueWeather, native: &weatherStatePayload{value: value}}, nil
}
func NewPresenceStateValue(value PresenceValue) (StateValue, error) {
	if _, err := value.Components(); err != nil {
		return StateValue{}, ErrInvalidState
	}
	return StateValue{kind: ValuePresence, native: &presenceStatePayload{value: value}}, nil
}
func NewPresenceDetailsStateValue(value PresenceDetailsValue) (StateValue, error) {
	if _, err := value.Components(); err != nil {
		return StateValue{}, ErrInvalidState
	}
	return StateValue{kind: ValuePresenceDetails, native: &presenceDetailsStatePayload{value: value}}, nil
}

func (v StateValue) environmentKind() (ValueKind, error) {
	switch v.kind {
	case ValueWeather:
		if payload, ok := v.native.(*weatherStatePayload); ok && payload != nil &&
			payload.value.valid && validWeatherParts(payload.value.parts) {
			return ValueWeather, nil
		}
	case ValuePresence:
		if payload, ok := v.native.(*presenceStatePayload); ok && payload != nil {
			if _, err := payload.value.Components(); err == nil {
				return ValuePresence, nil
			}
		}
	case ValuePresenceDetails:
		if payload, ok := v.native.(*presenceDetailsStatePayload); ok && payload != nil {
			if _, err := payload.value.Components(); err == nil {
				return ValuePresenceDetails, nil
			}
		}
	default:
	}
	return ValueUnspecified, ErrInvalidState
}

func (v StateValue) Weather() (WeatherValue, bool) {
	if kind, err := v.Kind(); err != nil || kind != ValueWeather {
		return WeatherValue{}, false
	}
	return v.native.(*weatherStatePayload).value, true
}
func (v StateValue) Presence() (PresenceValue, bool) {
	if kind, err := v.Kind(); err != nil || kind != ValuePresence {
		return PresenceValue{}, false
	}
	return v.native.(*presenceStatePayload).value, true
}
func (v StateValue) PresenceDetails() (PresenceDetailsValue, bool) {
	if kind, err := v.Kind(); err != nil || kind != ValuePresenceDetails {
		return PresenceDetailsValue{}, false
	}
	return v.native.(*presenceDetailsStatePayload).value, true
}

func sameEnvironmentValue(a, b StateValue) bool {
	switch a.kind {
	case ValueWeather:
		return reflect.DeepEqual(a.native.(*weatherStatePayload).value.parts, b.native.(*weatherStatePayload).value.parts)
	case ValuePresence:
		return a.native.(*presenceStatePayload).value.parts == b.native.(*presenceStatePayload).value.parts
	case ValuePresenceDetails:
		return reflect.DeepEqual(a.native.(*presenceDetailsStatePayload).value.parts, b.native.(*presenceDetailsStatePayload).value.parts)
	default:
		return false
	}
}
