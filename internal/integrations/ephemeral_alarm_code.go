package integrations

import (
	"context"
	"sync"

	"github.com/damione1/Homeward/contracts"
	controls "github.com/damione1/Homeward/gen/home/controls/v1"
	"github.com/damione1/Homeward/internal/domain"
)

// The value exists only for a single credential-bound admission. No wire
// CommandRequest, durable fingerprint, response or tracker contains it.
type alarmCodeKey struct{}
type ephemeralAlarmCode struct {
	mu        sync.Mutex
	command   domain.CommandID
	actor     domain.ActorID
	entity    domain.EntityID
	operation controls.AlarmOperation
	value     []byte
}

func (*ephemeralAlarmCode) String() string   { return "[ephemeral alarm credential]" }
func (*ephemeralAlarmCode) GoString() string { return "[ephemeral alarm credential]" }
func (v *ephemeralAlarmCode) clear()         { v.mu.Lock(); defer v.mu.Unlock(); clear(v.value); v.value = nil }

// WithEphemeralAlarmCode copies a bounded numeric credential into one retained
// admission context. The caller must call release on every completion path.
func WithEphemeralAlarmCode(ctx context.Context, id domain.CommandID, actor domain.ActorID, entity domain.EntityID, operation controls.AlarmOperation, code []byte) (context.Context, func(), error) {
	if ctx == nil || len(code) < 1 || len(code) > 32 {
		return nil, nil, ErrInvalidInput
	}
	if _, err := id.Value(); err != nil {
		return nil, nil, ErrInvalidInput
	}
	if _, err := actor.Value(); err != nil {
		return nil, nil, ErrInvalidInput
	}
	if _, err := entity.Value(); err != nil {
		return nil, nil, ErrInvalidInput
	}
	switch operation {
	case controls.AlarmOperation_ALARM_OPERATION_ARM_HOME, controls.AlarmOperation_ALARM_OPERATION_ARM_AWAY, controls.AlarmOperation_ALARM_OPERATION_ARM_NIGHT, controls.AlarmOperation_ALARM_OPERATION_DISARM:
	default:
		return nil, nil, ErrInvalidInput
	}
	for _, b := range code {
		if b < '0' || b > '9' {
			return nil, nil, ErrInvalidInput
		}
	}
	value := &ephemeralAlarmCode{command: id, actor: actor, entity: entity, operation: operation, value: append([]byte(nil), code...)}
	return context.WithValue(ctx, alarmCodeKey{}, value), value.clear, nil
}

// TakeEphemeralAlarmCode transfers the value at most once to its exact admitted
// alarm occurrence. The caller clears returned bytes immediately after sending.
// Authorization, owner generation and expiry are rechecked by the dispatch path.
func (c AdmittedCommand) TakeEphemeralAlarmCode(ctx context.Context) ([]byte, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, ErrInvalidInput
	}
	value, _ := ctx.Value(alarmCodeKey{}).(*ephemeralAlarmCode)
	if value == nil {
		return nil, nil
	}
	if !c.ReadyForTransport() {
		return nil, ErrCommandAuthorizationDenied
	}
	value.mu.Lock()
	defer value.mu.Unlock()
	if c.internal != nil || c.id != value.command || c.actor != value.actor || c.entity != value.entity || c.request.GetCapabilitySchemaId() != contracts.AlarmSchema || c.request.GetArguments().GetAlarm().GetOperation() != value.operation || len(value.value) == 0 {
		return nil, ErrInvalidInput
	}
	code := append([]byte(nil), value.value...)
	clear(value.value)
	value.value = nil
	return code, nil
}
