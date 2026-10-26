package contracts

import (
	control "github.com/damione1/Homeward/gen/home/controls/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
)

var coverStates = map[control.CoverState]domain.CoverState{
	control.CoverState_COVER_STATE_OPEN:    domain.CoverStateOpen,
	control.CoverState_COVER_STATE_CLOSED:  domain.CoverStateClosed,
	control.CoverState_COVER_STATE_OPENING: domain.CoverStateOpening,
	control.CoverState_COVER_STATE_CLOSING: domain.CoverStateClosing,
}
var lockStates = map[control.LockState]domain.LockState{
	control.LockState_LOCK_STATE_LOCKED:    domain.LockStateLocked,
	control.LockState_LOCK_STATE_UNLOCKED:  domain.LockStateUnlocked,
	control.LockState_LOCK_STATE_LOCKING:   domain.LockStateLocking,
	control.LockState_LOCK_STATE_UNLOCKING: domain.LockStateUnlocking,
	control.LockState_LOCK_STATE_JAMMED:    domain.LockStateJammed,
	control.LockState_LOCK_STATE_OPENING:   domain.LockStateOpening,
	control.LockState_LOCK_STATE_OPEN:      domain.LockStateOpen,
}
var alarmStates = map[control.AlarmState]domain.AlarmState{
	control.AlarmState_ALARM_STATE_DISARMED:            domain.AlarmStateDisarmed,
	control.AlarmState_ALARM_STATE_ARMED_HOME:          domain.AlarmStateArmedHome,
	control.AlarmState_ALARM_STATE_ARMED_AWAY:          domain.AlarmStateArmedAway,
	control.AlarmState_ALARM_STATE_ARMED_NIGHT:         domain.AlarmStateArmedNight,
	control.AlarmState_ALARM_STATE_ARMED_VACATION:      domain.AlarmStateArmedVacation,
	control.AlarmState_ALARM_STATE_ARMED_CUSTOM_BYPASS: domain.AlarmStateArmedCustomBypass,
	control.AlarmState_ALARM_STATE_PENDING:             domain.AlarmStatePending,
	control.AlarmState_ALARM_STATE_ARMING:              domain.AlarmStateArming,
	control.AlarmState_ALARM_STATE_DISARMING:           domain.AlarmStateDisarming,
	control.AlarmState_ALARM_STATE_TRIGGERED:           domain.AlarmStateTriggered,
}

func admitControlEnum[W, D comparable](value W, mapping map[W]D) (D, error) {
	if admitted, ok := mapping[value]; ok {
		return admitted, nil
	}
	var zero D
	return zero, ErrInvalidWireState
}
func wireControlEnum[W, D comparable](value D, mapping map[W]D) (W, error) {
	for wire, admitted := range mapping {
		if admitted == value {
			return wire, nil
		}
	}
	var zero W
	return zero, ErrInvalidWireState
}
func validControlOptions[T comparable](values []T, known func(T) bool, maximum int) bool {
	if len(values) > maximum {
		return false
	}
	seen := make(map[T]bool, len(values))
	for _, value := range values {
		if !known(value) || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}
func coverOperation(value control.CoverOperation) bool {
	switch value {
	case control.CoverOperation_COVER_OPERATION_OPEN, control.CoverOperation_COVER_OPERATION_CLOSE, control.CoverOperation_COVER_OPERATION_STOP:
		return true
	default:
		return false
	}
}
func lockOperation(value control.LockOperation) bool {
	switch value {
	case control.LockOperation_LOCK_OPERATION_LOCK, control.LockOperation_LOCK_OPERATION_UNLOCK:
		return true
	default:
		return false
	}
}
func alarmOperation(value control.AlarmOperation) bool {
	switch value {
	case control.AlarmOperation_ALARM_OPERATION_ARM_HOME, control.AlarmOperation_ALARM_OPERATION_ARM_AWAY, control.AlarmOperation_ALARM_OPERATION_DISARM:
		return true
	default:
		return false
	}
}
func percentBounds(value *control.PercentBounds) *registry.NumberBounds {
	if value == nil {
		return nil
	}
	return &registry.NumberBounds{Minimum: value.Minimum, Maximum: value.Maximum, Step: value.Step}
}
func validCoverConstraints(value *control.CoverConstraints) bool {
	if value == nil || !knownFieldsOnly(value.ProtoReflect()) || !validControlOptions(value.Operations, coverOperation, 3) || !validControlOptions(value.ObservedStates, func(state control.CoverState) bool { _, ok := coverStates[state]; return ok }, 4) {
		return false
	}
	if value.SetPositionPercent != nil {
		bounds := percentBounds(value.SetPositionPercent)
		if !validBounds(bounds) || bounds.Minimum < 0 || bounds.Maximum > 100 {
			return false
		}
	}
	return len(value.Operations) != 0 || value.SetPositionPercent != nil || len(value.ObservedStates) != 0 || value.ObservesPositionPercent || value.ObservesObstruction
}
func validLockConstraints(value *control.LockConstraints) bool {
	return value != nil && knownFieldsOnly(value.ProtoReflect()) && validControlOptions(value.Operations, lockOperation, 2) && validControlOptions(value.ObservedStates, func(state control.LockState) bool { _, ok := lockStates[state]; return ok }, 7) && (len(value.Operations) != 0 || len(value.ObservedStates) != 0)
}
func validAlarmConstraints(value *control.AlarmConstraints) bool {
	return value != nil && knownFieldsOnly(value.ProtoReflect()) && validControlOptions(value.Operations, alarmOperation, 3) && validControlOptions(value.ObservedStates, func(state control.AlarmState) bool { _, ok := alarmStates[state]; return ok }, 10) && (len(value.Operations) != 0 || len(value.ObservedStates) != 0)
}
func admitCoverAction(value *control.CoverAction, limits *control.CoverConstraints) bool {
	if value == nil || !knownFieldsOnly(value.ProtoReflect()) || !validCoverConstraints(limits) {
		return false
	}
	switch command := value.Command.(type) {
	case *control.CoverAction_Operation:
		return command != nil && coverOperation(command.Operation) && contains(limits.Operations, command.Operation)
	case *control.CoverAction_PositionPercent:
		return command != nil && withinBounds(command.PositionPercent, percentBounds(limits.SetPositionPercent))
	default:
		return false
	}
}
func admitLockAction(value *control.LockAction, limits *control.LockConstraints) bool {
	return value != nil && knownFieldsOnly(value.ProtoReflect()) && validLockConstraints(limits) && lockOperation(value.Operation) && contains(limits.Operations, value.Operation)
}
func admitAlarmAction(value *control.AlarmAction, limits *control.AlarmConstraints) bool {
	return value != nil && knownFieldsOnly(value.ProtoReflect()) && validAlarmConstraints(limits) && alarmOperation(value.Operation) && contains(limits.Operations, value.Operation)
}

func AdmitWireCoverValue(value *control.CoverValue) (domain.CoverValue, error) {
	if value == nil || !knownFieldsOnly(value.ProtoReflect()) {
		return domain.CoverValue{}, ErrInvalidWireState
	}
	var state *domain.CoverState
	if value.State != nil {
		admitted, err := admitControlEnum(*value.State, coverStates)
		if err != nil {
			return domain.CoverValue{}, err
		}
		state = &admitted
	}
	return domain.NewCoverValue(state, value.PositionPercent, value.Obstructed)
}
func EncodeCoverValue(value domain.CoverValue) (*control.CoverValue, error) {
	state, position, obstruction, err := value.Components()
	if err != nil {
		return nil, err
	}
	out := &control.CoverValue{PositionPercent: position, Obstructed: obstruction}
	if state != nil {
		encoded, err := wireControlEnum(*state, coverStates)
		if err != nil {
			return nil, err
		}
		out.State = &encoded
	}
	return out, nil
}
func AdmitWireLockValue(value *control.LockValue) (domain.LockState, error) {
	if value == nil || !knownFieldsOnly(value.ProtoReflect()) {
		return domain.LockStateUnspecified, ErrInvalidWireState
	}
	return admitControlEnum(value.State, lockStates)
}
func EncodeLockValue(value domain.LockState) (*control.LockValue, error) {
	state, err := wireControlEnum(value, lockStates)
	if err != nil {
		return nil, err
	}
	return &control.LockValue{State: state}, nil
}
func AdmitWireAlarmValue(value *control.AlarmValue) (domain.AlarmState, error) {
	if value == nil || !knownFieldsOnly(value.ProtoReflect()) {
		return domain.AlarmStateUnspecified, ErrInvalidWireState
	}
	return admitControlEnum(value.State, alarmStates)
}
func EncodeAlarmValue(value domain.AlarmState) (*control.AlarmValue, error) {
	state, err := wireControlEnum(value, alarmStates)
	if err != nil {
		return nil, err
	}
	return &control.AlarmValue{State: state}, nil
}
func (c *CapabilityCatalog) controlDescription(entity domain.EntityID, endpoint, schema string) (*registry.CapabilityDescription, error) {
	if !c.ready() || endpoint == "" {
		return nil, ErrInvalidCapability
	}
	id, err := entity.Value()
	if err != nil {
		return nil, err
	}
	c.mu.RLock()
	description := c.instances[id][capabilityKey(schema, endpoint)]
	c.mu.RUnlock()
	if description == nil {
		return nil, ErrInvalidCapability
	}
	return description, nil
}
func (c *CapabilityCatalog) AdmitDomainCover(entity domain.EntityID, endpoint string, value domain.CoverValue) error {
	description, err := c.controlDescription(entity, endpoint, CoverSchema)
	if err != nil {
		return err
	}
	limits := description.GetCover()
	if !validCoverConstraints(limits) {
		return ErrInvalidCapability
	}
	state, position, obstruction, err := value.Components()
	if err != nil {
		return err
	}
	if state != nil {
		wire, err := wireControlEnum(*state, coverStates)
		if err != nil || !contains(limits.ObservedStates, wire) {
			return ErrInvalidWireState
		}
	}
	if position != nil && !limits.ObservesPositionPercent || obstruction != nil && !limits.ObservesObstruction {
		return ErrInvalidWireState
	}
	return nil
}
func (c *CapabilityCatalog) AdmitDomainLock(entity domain.EntityID, endpoint string, value domain.LockState) error {
	description, err := c.controlDescription(entity, endpoint, LockSchema)
	if err != nil {
		return err
	}
	limits := description.GetLock()
	state, err := wireControlEnum(value, lockStates)
	if err != nil || !validLockConstraints(limits) || !contains(limits.ObservedStates, state) {
		return ErrInvalidWireState
	}
	return nil
}
func (c *CapabilityCatalog) AdmitDomainAlarm(entity domain.EntityID, endpoint string, value domain.AlarmState) error {
	description, err := c.controlDescription(entity, endpoint, AlarmSchema)
	if err != nil {
		return err
	}
	limits := description.GetAlarm()
	state, err := wireControlEnum(value, alarmStates)
	if err != nil || !validAlarmConstraints(limits) || !contains(limits.ObservedStates, state) {
		return ErrInvalidWireState
	}
	return nil
}
