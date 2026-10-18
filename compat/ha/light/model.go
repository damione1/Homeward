// Package light prepares one HA light action per run occurrence. It does not
// own a scheduler, a durable ledger, or any physical driver.
package light

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

var (
	ErrInvalidSlot       = errors.New("invalid HA light preparation slot")
	ErrUnsupportedAction = errors.New("unsupported HA light action")
)

// Service is a single existing volatile or durable CommandRouter service.
// Both methods must come from the same service instance; its private token
// rejects dispatch through another router.
type Service interface {
	CaptureLight(context.Context, domain.HAAlias) (integrations.LightCapture, error)
	DispatchPrepared(context.Context, integrations.PreparedLight) (*actions.CommandResponse, error)
}

// PreparationSlot belongs to exactly one occurrence and one light target.
// sync.Once retains either its immutable result or its first refusal. There is
// no global ID map and no retry path that reconstructs a lost slot.
type PreparationSlot struct {
	once    sync.Once
	service Service
	call    ir.Call
	alias   domain.HAAlias
	actor   domain.ActorID
	id      domain.CommandID
	expires time.Time
	result  PreparedAction
	err     error
}

type PreparedAction struct {
	command integrations.PreparedLight
	source  ir.Source
	kind    ir.BrightnessKind
}

func (p PreparedAction) Request() (*actions.CommandRequest, error) { return p.command.Request() }
func (p PreparedAction) Source() ir.Source                         { return p.source }
func (p PreparedAction) BrightnessKind() ir.BrightnessKind         { return p.kind }
func (p PreparedAction) HABrightness() (uint8, bool)               { return p.command.HABrightness() }
func (p PreparedAction) MQTTTarget() (uint32, bool)                { return p.command.MQTTTarget() }
func (p PreparedAction) NativePercent() (float64, bool)            { return p.command.NativePercent() }

func NewPreparationSlot(service Service, call ir.Call, targetIndex int, actor domain.ActorID, id domain.CommandID, expires time.Time) (*PreparationSlot, error) {
	if nilService(service) || expires.IsZero() || targetIndex < 0 {
		return nil, ErrInvalidSlot
	}
	if call.Kind() != ir.CallLightTurnOn && call.Kind() != ir.CallLightTurnOff {
		return nil, ErrUnsupportedAction
	}
	targets := call.Targets()
	if targetIndex >= len(targets) {
		return nil, ErrInvalidSlot
	}
	alias := targets[targetIndex].Alias()
	if _, err := alias.Value(); err != nil {
		return nil, ErrInvalidSlot
	}
	if _, err := actor.Value(); err != nil {
		return nil, ErrInvalidSlot
	}
	if _, err := id.Value(); err != nil {
		return nil, ErrInvalidSlot
	}
	return &PreparationSlot{service: service, call: call, alias: alias, actor: actor, id: id, expires: expires}, nil
}

func nilService(service Service) bool {
	if service == nil {
		return true
	}
	v := reflect.ValueOf(service)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
