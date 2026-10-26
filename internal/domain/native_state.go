package domain

import "math"

// ValueKind is the only discriminator. The private interface admits exactly
// these private pointer wrappers, never an externally mutable payload.
type nativeStatePayload interface{ nativeStateMarker() }
type helperStatePayload struct {
	value    HelperValue
	revision uint64
}
type solarStatePayload struct{ value SolarValue }
type solarEventStatePayload struct{ value SolarEventValue }

func (*helperStatePayload) nativeStateMarker()     {}
func (*solarStatePayload) nativeStateMarker()      {}
func (*solarEventStatePayload) nativeStateMarker() {}

func NewHelperStateValue(value HelperValue, revision uint64) (StateValue, error) {
	if _, err := value.Kind(); err != nil || revision == 0 || revision > math.MaxInt64 {
		return StateValue{}, ErrInvalidState
	}
	return StateValue{kind: ValueHelper, native: &helperStatePayload{value: value, revision: revision}}, nil
}

func NewSolarStateValue(value SolarValue) (StateValue, error) {
	if _, err := value.Components(); err != nil {
		return StateValue{}, ErrInvalidState
	}
	return StateValue{kind: ValueSolar, native: &solarStatePayload{value: value}}, nil
}

func NewSolarEventStateValue(value SolarEventValue) (StateValue, error) {
	if _, _, err := value.Components(); err != nil {
		return StateValue{}, ErrInvalidState
	}
	return StateValue{kind: ValueSolarEvent, native: &solarEventStatePayload{value: value}}, nil
}

func (v StateValue) nativeKind() (ValueKind, error) {
	switch v.kind {
	case ValueCover:
		if p, ok := v.native.(*coverStatePayload); ok && p != nil {
			if _, _, _, err := p.value.Components(); err == nil {
				return ValueCover, nil
			}
		}
	case ValueLock:
		if p, ok := v.native.(*lockStatePayload); ok && p != nil && p.value.Valid() {
			return ValueLock, nil
		}
	case ValueAlarm:
		if p, ok := v.native.(*alarmStatePayload); ok && p != nil && p.value.Valid() {
			return ValueAlarm, nil
		}
	case ValueHelper:
		p, ok := v.native.(*helperStatePayload)
		if ok && p != nil && p.revision > 0 && p.revision <= math.MaxInt64 {
			if _, err := p.value.Kind(); err == nil {
				return ValueHelper, nil
			}
		}
	case ValueSolar:
		p, ok := v.native.(*solarStatePayload)
		if ok && p != nil {
			if _, err := p.value.Components(); err == nil {
				return ValueSolar, nil
			}
		}
	case ValueSolarEvent:
		p, ok := v.native.(*solarEventStatePayload)
		if ok && p != nil {
			if _, _, err := p.value.Components(); err == nil {
				return ValueSolarEvent, nil
			}
		}
	case ValueUnspecified, ValueSwitch, ValueMeasurement, ValueLight, ValueFan, ValueClimate:
	default:
	}
	return ValueUnspecified, ErrInvalidState
}

func (v StateValue) Helper() (HelperValue, uint64, bool) {
	if k, err := v.Kind(); err != nil || k != ValueHelper {
		return HelperValue{}, 0, false
	}
	p := v.native.(*helperStatePayload)
	return p.value, p.revision, true
}
func (v StateValue) Solar() (SolarValue, bool) {
	if k, err := v.Kind(); err != nil || k != ValueSolar {
		return SolarValue{}, false
	}
	return v.native.(*solarStatePayload).value, true
}
func (v StateValue) SolarEvent() (SolarEventValue, bool) {
	if k, err := v.Kind(); err != nil || k != ValueSolarEvent {
		return SolarEventValue{}, false
	}
	return v.native.(*solarEventStatePayload).value, true
}

func sameBusinessValue(a, b StateValue) bool {
	left, err := a.Kind()
	right, otherErr := b.Kind()
	if err != nil || otherErr != nil || left != right {
		return false
	}
	switch left {
	case ValueCover:
		return a.native.(*coverStatePayload).value == b.native.(*coverStatePayload).value
	case ValueLock:
		return a.native.(*lockStatePayload).value == b.native.(*lockStatePayload).value
	case ValueAlarm:
		return a.native.(*alarmStatePayload).value == b.native.(*alarmStatePayload).value
	case ValueSwitch, ValueMeasurement, ValueLight, ValueFan, ValueClimate:
		return a.switchOn == b.switchOn && a.measurement == b.measurement && a.light == b.light && a.fan == b.fan && a.climate == b.climate
	case ValueHelper:
		return a.native.(*helperStatePayload).value == b.native.(*helperStatePayload).value
	case ValueSolar:
		return a.native.(*solarStatePayload).value == b.native.(*solarStatePayload).value
	case ValueSolarEvent:
		return a.native.(*solarEventStatePayload).value == b.native.(*solarEventStatePayload).value
	case ValueUnspecified:
		return false
	default:
		return false
	}
}
func helperRevisionChanged(a, b StateValue) bool {
	_, left, leftOK := a.Helper()
	_, right, rightOK := b.Helper()
	return leftOK && rightOK && left != right
}
