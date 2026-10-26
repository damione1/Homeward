package domain

import "math"

type CoverState uint8

const (
	CoverStateUnspecified CoverState = iota
	CoverStateOpen
	CoverStateClosed
	CoverStateOpening
	CoverStateClosing
)

func (s CoverState) Valid() bool {
	switch s {
	case CoverStateOpen, CoverStateClosed, CoverStateOpening, CoverStateClosing:
		return true
	case CoverStateUnspecified:
	default:
	}
	return false
}

// CoverValue preserves independent observations, including position zero and
// an explicitly false obstruction reading. No missing field is inferred.
type CoverValue struct {
	valid, hasState, hasPosition, hasObstruction bool
	state                                        CoverState
	position                                     float64
	obstructed                                   bool
}

func NewCoverValue(state *CoverState, position *float64, obstructed *bool) (CoverValue, error) {
	if state == nil && position == nil && obstructed == nil {
		return CoverValue{}, ErrInvalidState
	}
	v := CoverValue{valid: true}
	if state != nil {
		if !state.Valid() {
			return CoverValue{}, ErrInvalidState
		}
		v.state, v.hasState = *state, true
	}
	if position != nil {
		if math.IsNaN(*position) || math.IsInf(*position, 0) || *position < 0 || *position > 100 {
			return CoverValue{}, ErrInvalidState
		}
		v.position, v.hasPosition = *position, true
	}
	if obstructed != nil {
		v.obstructed, v.hasObstruction = *obstructed, true
	}
	return v, nil
}

func (v CoverValue) Components() (*CoverState, *float64, *bool, error) {
	if !v.valid || !v.hasState && !v.hasPosition && !v.hasObstruction || v.hasState && !v.state.Valid() || v.hasPosition && (math.IsNaN(v.position) || math.IsInf(v.position, 0) || v.position < 0 || v.position > 100) {
		return nil, nil, nil, ErrInvalidState
	}
	var state *CoverState
	var position *float64
	var obstruction *bool
	if v.hasState {
		value := v.state
		state = &value
	}
	if v.hasPosition {
		value := v.position
		position = &value
	}
	if v.hasObstruction {
		value := v.obstructed
		obstruction = &value
	}
	return state, position, obstruction, nil
}

type LockState uint8

const (
	LockStateUnspecified LockState = iota
	LockStateLocked
	LockStateUnlocked
	LockStateLocking
	LockStateUnlocking
	LockStateJammed
	LockStateOpening
	LockStateOpen
)

func (s LockState) Valid() bool {
	switch s {
	case LockStateLocked, LockStateUnlocked, LockStateLocking, LockStateUnlocking, LockStateJammed, LockStateOpening, LockStateOpen:
		return true
	case LockStateUnspecified:
	default:
	}
	return false
}

type AlarmState uint8

const (
	AlarmStateUnspecified AlarmState = iota
	AlarmStateDisarmed
	AlarmStateArmedHome
	AlarmStateArmedAway
	AlarmStateArmedNight
	AlarmStateArmedVacation
	AlarmStateArmedCustomBypass
	AlarmStatePending
	AlarmStateArming
	AlarmStateDisarming
	AlarmStateTriggered
)

func (s AlarmState) Valid() bool {
	switch s {
	case AlarmStateDisarmed, AlarmStateArmedHome, AlarmStateArmedAway, AlarmStateArmedNight, AlarmStateArmedVacation, AlarmStateArmedCustomBypass, AlarmStatePending, AlarmStateArming, AlarmStateDisarming, AlarmStateTriggered:
		return true
	case AlarmStateUnspecified:
	default:
	}
	return false
}

type coverStatePayload struct{ value CoverValue }
type lockStatePayload struct{ value LockState }
type alarmStatePayload struct{ value AlarmState }

func (*coverStatePayload) nativeStateMarker() {}
func (*lockStatePayload) nativeStateMarker()  {}
func (*alarmStatePayload) nativeStateMarker() {}

func NewCoverStateValue(value CoverValue) (StateValue, error) {
	if _, _, _, err := value.Components(); err != nil {
		return StateValue{}, err
	}
	return StateValue{kind: ValueCover, native: &coverStatePayload{value}}, nil
}
func NewLockStateValue(value LockState) (StateValue, error) {
	if !value.Valid() {
		return StateValue{}, ErrInvalidState
	}
	return StateValue{kind: ValueLock, native: &lockStatePayload{value}}, nil
}
func NewAlarmStateValue(value AlarmState) (StateValue, error) {
	if !value.Valid() {
		return StateValue{}, ErrInvalidState
	}
	return StateValue{kind: ValueAlarm, native: &alarmStatePayload{value}}, nil
}
func (v StateValue) Cover() (CoverValue, bool) {
	if kind, err := v.Kind(); err != nil || kind != ValueCover {
		return CoverValue{}, false
	}
	return v.native.(*coverStatePayload).value, true
}
func (v StateValue) Lock() (LockState, bool) {
	if kind, err := v.Kind(); err != nil || kind != ValueLock {
		return LockStateUnspecified, false
	}
	return v.native.(*lockStatePayload).value, true
}
func (v StateValue) Alarm() (AlarmState, bool) {
	if kind, err := v.Kind(); err != nil || kind != ValueAlarm {
		return AlarmStateUnspecified, false
	}
	return v.native.(*alarmStatePayload).value, true
}
