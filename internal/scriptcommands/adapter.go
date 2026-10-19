// Package scriptcommands adapts admitted script calls to the existing command
// and scene services. It owns no ledger, scheduler, driver or user identity.
package scriptcommands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	halight "github.com/damione1/Homeward/compat/ha/light"
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/scenes"
	"github.com/damione1/Homeward/internal/sequencer"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type ErrorKind uint8

const (
	ErrorInvalid ErrorKind = iota
	InvalidInput
	Unavailable
	Refused
	Indeterminate
	Partial
)

type Error struct {
	kind     ErrorKind
	cause    error
	scene    scenes.ApplyResult
	hasScene bool
}

func (e *Error) Kind() ErrorKind {
	if e == nil {
		return ErrorInvalid
	}
	return e.kind
}
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}
func (e *Error) SceneResult() (scenes.ApplyResult, bool) {
	if e == nil {
		return scenes.ApplyResult{}, false
	}
	return e.scene, e.hasScene
}
func (e *Error) Error() string {
	if e == nil {
		return "script command: invalid error"
	}
	switch e.kind {
	case InvalidInput:
		return "script command: invalid input"
	case Unavailable:
		return "script command: unavailable"
	case Refused:
		return "script command: refused"
	case Indeterminate:
		return "script command: outcome indeterminate"
	case Partial:
		return "script command: partial scene result"
	case ErrorInvalid:
		return "script command: invalid error"
	}
	return "script command: invalid error"
}
func failure(kind ErrorKind, cause error) error { return &Error{kind: kind, cause: cause} }

type Input struct {
	Commands        *integrations.DurableCommandService
	Scenes          *scenes.Service
	Host            *integrations.Manager
	Projection      *sequencer.Sequencer
	Generation      integrations.GenerationAuthority
	Clock           automationtriggers.Clock
	CommandDeadline time.Duration
}
type Adapter struct{ input Input }

func missing(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}
func New(input Input) (*Adapter, error) {
	if input.Commands == nil || input.Host == nil || input.Projection == nil || missing(input.Generation) || missing(input.Clock) ||
		input.Clock.Now().IsZero() || input.CommandDeadline <= 0 || input.CommandDeadline > 24*time.Hour {
		return nil, failure(InvalidInput, nil)
	}
	return &Adapter{input}, nil
}

// ExecuteCall uses each occurrence exactly once. In particular a relative
// light operation owns one proven NHA-022 PreparationSlot; a refusal or uncertain
// result never creates another slot, recaptures input or retries a driver.
func (a *Adapter) ExecuteCall(ctx context.Context, call ir.Call, execution automationruns.CommandExecution) error {
	if a == nil || missing(ctx) || ctx.Err() != nil {
		return failure(InvalidInput, nil)
	}
	if _, err := execution.Actor().Value(); err != nil {
		return failure(InvalidInput, err)
	}
	cause, err := execution.CausalKey()
	if err != nil {
		return failure(InvalidInput, err)
	}
	execution.Suspend()
	epoch, err := a.input.Projection.CurrentAdmissionEpoch()
	if err != nil || epoch == "" {
		return failure(Unavailable, err)
	}
	expires := a.input.Clock.Now().Add(a.input.CommandDeadline)
	targets := call.Targets()
	if len(targets) == 0 || len(targets) > 256 {
		return failure(InvalidInput, nil)
	}
	switch call.Kind() {
	case ir.CallClimateSetTemperature, ir.CallSwitchTurnOn, ir.CallLightTurnOn, ir.CallLightTurnOff, ir.CallSceneTurnOn:
	case ir.CallScript, ir.CallInvalid:
		return failure(InvalidInput, nil)
	default:
		return failure(InvalidInput, nil)
	}
	for index, target := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", epoch, cause, index)))
		id, err := domain.ParseCommandID("script-" + hex.EncodeToString(sum[:]))
		if err != nil {
			return failure(InvalidInput, err)
		}
		switch call.Kind() {
		case ir.CallLightTurnOn, ir.CallLightTurnOff:
			slot, err := halight.NewPreparationSlot(a.input.Commands, call, index, execution.Actor(), id, expires)
			if err != nil {
				return failure(Refused, err)
			}
			response, err := slot.Dispatch(ctx)
			if err := commandResult(response, err); err != nil {
				return err
			}
		case ir.CallSwitchTurnOn:
			capture, err := a.input.Commands.CaptureDirect(ctx, target.Alias(), integrations.DirectSwitch)
			if err != nil {
				return failure(Refused, err)
			}
			prepared, err := capture.PrepareSwitch(execution.Actor(), id, expires, true)
			if err != nil {
				return failure(Refused, err)
			}
			response, err := a.input.Commands.DispatchPreparedDirect(ctx, prepared)
			if err := commandResult(response, err); err != nil {
				return err
			}
		case ir.CallClimateSetTemperature:
			wire, err := a.climate(ctx, target.Alias(), call.Temperature(), id, epoch, expires)
			if err != nil {
				return err
			}
			response, err := a.input.Commands.Dispatch(ctx, execution.Actor(), wire)
			if err := commandResult(response, err); err != nil {
				return err
			}
		case ir.CallSceneTurnOn:
			if a.input.Scenes == nil {
				return failure(Unavailable, nil)
			}
			ref, err := a.input.Scenes.Declarative(target.Alias())
			if err != nil {
				return failure(Refused, err)
			}
			var transition *float64
			if number := call.TransitionSeconds(); number.Present() {
				seconds := number.Value()
				transition = &seconds
			}
			slot, err := scenes.NewPreparationSlot(a.input.Scenes, ref, execution.Actor(), id, transition, expires)
			if err != nil {
				return failure(Refused, err)
			}
			prepared, err := slot.PrepareOnce(ctx)
			if err != nil {
				return failure(Refused, err)
			}
			result, err := a.input.Scenes.ApplyPrepared(ctx, prepared)
			if err != nil {
				return failure(Refused, err)
			}
			if err := sceneResult(result); err != nil {
				return err
			}
		case ir.CallInvalid, ir.CallScript:
			return failure(InvalidInput, nil)
		default:
			return failure(InvalidInput, nil)
		}
	}
	return nil
}

// Climate uses the existing public route and expected-revision fence. The
// stable entity is resolved once; final 024 dispatch revalidates epoch, config,
// capability, authority and current state before the one physical effect.
func (a *Adapter) climate(ctx context.Context, alias domain.HAAlias, temperature ir.Number, id domain.CommandID, epoch string, expires time.Time) (*actions.CommandRequest, error) {
	if !temperature.Present() {
		return nil, failure(InvalidInput, nil)
	}
	var wire *actions.CommandRequest
	err := a.input.Generation.WithCurrent(ctx, func(revision uint64) error {
		if revision == 0 {
			return failure(Unavailable, nil)
		}
		entity, err := a.input.Projection.ResolveCommandAlias(alias)
		if err != nil {
			return failure(Refused, err)
		}
		descriptions, err := a.input.Host.Capabilities(entity)
		if err != nil {
			return failure(Refused, err)
		}
		var description *registry.CapabilityDescription
		for _, candidate := range descriptions {
			if candidate == nil || candidate.SchemaId != contracts.ClimateSchema {
				continue
			}
			if description != nil {
				return failure(Refused, integrations.ErrCommandRouteAmbiguous)
			}
			description = candidate
		}
		if description == nil {
			return failure(Refused, integrations.ErrCommandRouteUnavailable)
		}
		observed, present, err := a.input.Projection.State(entity)
		if err != nil || !present {
			return failure(Refused, err)
		}
		value, critical := observed.CriticalValue()
		if !critical {
			return failure(Refused, integrations.ErrCommandRouteUnavailable)
		}
		if _, present := value.Climate(); !present {
			return failure(Refused, integrations.ErrCommandRouteUnavailable)
		}
		_, _, _, _, expected, err := observed.Components()
		if err != nil || expected == 0 {
			return failure(Refused, err)
		}
		entityText, _ := entity.Value()
		commandText, _ := id.Value()
		setpoint := temperature.Value()
		wire = &actions.CommandRequest{CommandId: commandText, AdmissionEpoch: epoch, ConfigRevision: revision,
			Target:             &registry.EntityRef{Reference: &registry.EntityRef_EntityId{EntityId: entityText}},
			CapabilitySchemaId: contracts.ClimateSchema, Endpoint: description.Endpoint, ExpectedRevision: &expected,
			ExpiresAt: timestamppb.New(expires), Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetClimate{SetClimate: &actions.SetClimate{SetpointCelsius: &setpoint}}}}
		if wire.ExpiresAt.CheckValid() != nil {
			return failure(InvalidInput, nil)
		}
		return nil
	})
	return wire, err
}
func commandResult(response *actions.CommandResponse, err error) error {
	if errors.Is(err, integrations.ErrDurableCommitUnknown) || errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) ||
		response != nil && response.Status == actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
		return failure(Indeterminate, err)
	}
	if err != nil {
		return failure(Refused, err)
	}
	if response == nil {
		return failure(Indeterminate, nil)
	}
	switch response.Status {
	case actions.ActionStatus_ACTION_STATUS_ACCEPTED, actions.ActionStatus_ACTION_STATUS_DISPATCHED,
		actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED, actions.ActionStatus_ACTION_STATUS_CONFIRMED:
		return nil
	case actions.ActionStatus_ACTION_STATUS_INDETERMINATE:
		return failure(Indeterminate, nil)
	case actions.ActionStatus_ACTION_STATUS_FAILED, actions.ActionStatus_ACTION_STATUS_CANCELLED:
		return failure(Refused, nil)
	case actions.ActionStatus_ACTION_STATUS_UNSPECIFIED:
		return failure(Indeterminate, nil)
	default:
		return failure(Indeterminate, nil)
	}
}

func sceneResult(result scenes.ApplyResult) error {
	switch result.Aggregate() {
	case scenes.AggregateComplete:
		return nil
	case scenes.AggregatePartial, scenes.AggregateNone:
		kind := Refused
		if result.Aggregate() == scenes.AggregatePartial {
			kind = Partial
		}
		for _, member := range result.Members() {
			if member.Outcome() == scenes.OutcomeIndeterminate {
				kind = Indeterminate
				break
			}
		}
		return &Error{kind: kind, scene: result, hasScene: true}
	case scenes.AggregateInvalid:
		return failure(Indeterminate, nil)
	}
	return failure(Indeterminate, nil)
}

var _ automationruns.CommandExecutor = (*Adapter)(nil)
