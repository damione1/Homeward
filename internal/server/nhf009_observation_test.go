package server

import (
	"context"
	"errors"
	"time"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	control "github.com/damione1/Homeward/gen/home/controls/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

// A separate explicit control publishes the double's observed effect with real
// causality. It deliberately does not issue a protocol-confirmation receipt.
func nhf009ObserveCommand(ctx context.Context, driver *nhf008Driver, expected *domain.CommandID, source *integrations.Session, bindings []integrations.Binding, devices []nhf009Device) error {
	last, err := driver.nhf009Last(expected)
	if err != nil {
		return err
	}
	_, entity, err := last.RouteIdentity()
	if err != nil {
		return err
	}
	request, err := last.Request()
	if err != nil {
		return err
	}
	for index, binding := range bindings {
		_, candidate, err := binding.IDs()
		if err != nil {
			return err
		}
		if candidate != entity {
			continue
		}
		value, err := nhf009CommandValue(devices[index].kind, request.Arguments)
		if err != nil {
			return err
		}
		quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		if err != nil {
			return err
		}
		at := time.Now().UTC()
		observation, err := integrations.NewStateObservation(devices[index].endpoint, quality, &value, &at, at, nil)
		if err != nil {
			return err
		}
		cause, err := last.Causality()
		if err != nil {
			return err
		}
		observation, err = observation.WithCausality(cause)
		if err != nil {
			return err
		}
		return source.Publish(ctx, binding, observation)
	}
	return errors.New("synthetic command binding no longer exists")
}
func nhf009CommandValue(kind nhf009DeviceKind, args *actions.ActionArgs) (domain.StateValue, error) {
	if args == nil {
		return domain.StateValue{}, errors.New("missing synthetic arguments")
	}
	switch kind {
	case nhf009Cover, nhf009Garage:
		action := args.GetCover()
		if action == nil {
			return domain.StateValue{}, errors.New("crossed synthetic cover arguments")
		}
		state := domain.CoverStateOpen
		var position *float64
		switch command := action.Command.(type) {
		case *control.CoverAction_PositionPercent:
			position = &command.PositionPercent
		case *control.CoverAction_Operation:
			switch command.Operation {
			case control.CoverOperation_COVER_OPERATION_OPEN:
				state = domain.CoverStateOpen
			case control.CoverOperation_COVER_OPERATION_CLOSE:
				state = domain.CoverStateClosed
				position = nhf008Pointer(0.)
			case control.CoverOperation_COVER_OPERATION_STOP:
				position = nhf008Pointer(35.)
			case control.CoverOperation_COVER_OPERATION_UNSPECIFIED:
				return domain.StateValue{}, errors.New("unspecified synthetic cover operation")
			default:
				return domain.StateValue{}, errors.New("future synthetic cover operation")
			}
		default:
			return domain.StateValue{}, errors.New("absent synthetic cover command")
		}
		cover, err := domain.NewCoverValue(&state, position, nhf008Pointer(kind == nhf009Garage))
		if err != nil {
			return domain.StateValue{}, err
		}
		return domain.NewCoverStateValue(cover)
	case nhf009Lock:
		action := args.GetLock()
		if action == nil {
			return domain.StateValue{}, errors.New("crossed synthetic lock arguments")
		}
		switch action.Operation {
		case control.LockOperation_LOCK_OPERATION_LOCK:
			return domain.NewLockStateValue(domain.LockStateLocked)
		case control.LockOperation_LOCK_OPERATION_UNLOCK:
			return domain.NewLockStateValue(domain.LockStateUnlocked)
		default:
			return domain.StateValue{}, errors.New("unknown synthetic lock operation")
		}
	case nhf009Alarm:
		action := args.GetAlarm()
		if action == nil {
			return domain.StateValue{}, errors.New("crossed synthetic alarm arguments")
		}
		switch action.Operation {
		case control.AlarmOperation_ALARM_OPERATION_ARM_HOME:
			return domain.NewAlarmStateValue(domain.AlarmStateArmedHome)
		case control.AlarmOperation_ALARM_OPERATION_ARM_AWAY:
			return domain.NewAlarmStateValue(domain.AlarmStateArmedAway)
		case control.AlarmOperation_ALARM_OPERATION_DISARM:
			return domain.NewAlarmStateValue(domain.AlarmStateDisarmed)
		default:
			return domain.StateValue{}, errors.New("unknown synthetic alarm operation")
		}
	default:
		return domain.StateValue{}, errors.New("unparsed synthetic device kind")
	}
}
