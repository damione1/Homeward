package integrations_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	control "github.com/damione1/Homeward/gen/home/controls/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"google.golang.org/protobuf/proto"
)

func TestNHF009ControlHostAdmissionAndProtocolConfirmation(t *testing.T) {
	cover, _ := domain.NewCoverValue(nil, nha174Pointer(0.0), nha174Pointer(false))
	coverState, _ := domain.NewCoverStateValue(cover)
	lockState, _ := domain.NewLockStateValue(domain.LockStateJammed)
	alarmState, _ := domain.NewAlarmStateValue(domain.AlarmStateTriggered)
	for _, item := range []struct {
		description    *registry.CapabilityDescription
		value          domain.StateValue
		valid, invalid *actions.ActionArgs
	}{
		{&registry.CapabilityDescription{SchemaId: contracts.CoverSchema, Major: 1, Endpoint: "relay", DisplayName: "Synthetic cover", Constraints: &registry.CapabilityDescription_Cover{Cover: &control.CoverConstraints{Operations: []control.CoverOperation{control.CoverOperation_COVER_OPERATION_STOP}, ObservesPositionPercent: true, ObservesObstruction: true}}}, coverState,
			&actions.ActionArgs{Arguments: &actions.ActionArgs_Cover{Cover: &control.CoverAction{Command: &control.CoverAction_Operation{Operation: control.CoverOperation_COVER_OPERATION_STOP}}}},
			&actions.ActionArgs{Arguments: &actions.ActionArgs_Cover{Cover: &control.CoverAction{Command: &control.CoverAction_Operation{Operation: control.CoverOperation_COVER_OPERATION_OPEN}}}}},
		{&registry.CapabilityDescription{SchemaId: contracts.LockSchema, Major: 1, Endpoint: "relay", DisplayName: "Synthetic lock", Constraints: &registry.CapabilityDescription_Lock{Lock: &control.LockConstraints{Operations: []control.LockOperation{control.LockOperation_LOCK_OPERATION_LOCK}, ObservedStates: []control.LockState{control.LockState_LOCK_STATE_JAMMED}}}}, lockState,
			&actions.ActionArgs{Arguments: &actions.ActionArgs_Lock{Lock: &control.LockAction{Operation: control.LockOperation_LOCK_OPERATION_LOCK}}},
			&actions.ActionArgs{Arguments: &actions.ActionArgs_Lock{Lock: &control.LockAction{Operation: control.LockOperation_LOCK_OPERATION_UNLOCK}}}},
		{&registry.CapabilityDescription{SchemaId: contracts.AlarmSchema, Major: 1, Endpoint: "relay", DisplayName: "Synthetic alarm", Constraints: &registry.CapabilityDescription_Alarm{Alarm: &control.AlarmConstraints{Operations: []control.AlarmOperation{control.AlarmOperation_ALARM_OPERATION_ARM_HOME}, ObservedStates: []control.AlarmState{control.AlarmState_ALARM_STATE_TRIGGERED}}}}, alarmState,
			&actions.ActionArgs{Arguments: &actions.ActionArgs_Alarm{Alarm: &control.AlarmAction{Operation: control.AlarmOperation_ALARM_OPERATION_ARM_HOME}}},
			&actions.ActionArgs{Arguments: &actions.ActionArgs_Alarm{Alarm: &control.AlarmAction{Operation: control.AlarmOperation_ALARM_OPERATION_DISARM}}}},
	} {
		t.Run(item.description.SchemaId, func(t *testing.T) {
			driver := &nha025Driver{}
			rig := nha025Setup(t, driver, nil)
			_, key, _ := rig.binding.Source()
			candidate, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{item.description})
			if err != nil {
				t.Fatal(err)
			}
			bindings, err := rig.session.Discover(context.Background(), []integrations.Candidate{candidate})
			if err != nil || len(bindings) != 1 {
				t.Fatal("typed control discovery", err)
			}
			rig.binding = bindings[0]
			publish := func(value domain.StateValue, cause *domain.Causality) error {
				quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
				at := time.Now().UTC()
				observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
				if err != nil {
					return err
				}
				if cause != nil {
					observation, err = observation.WithCausality(*cause)
					if err != nil {
						return err
					}
				}
				return rig.session.Publish(context.Background(), rig.binding, observation)
			}
			before, _ := rig.fixture.Sequencer().Capture()
			if err := publish(domain.NewSwitchValue(false), nil); !errors.Is(err, integrations.ErrInvalidInput) {
				t.Fatalf("foreign observation admitted: %v", err)
			}
			after, _ := rig.fixture.Sequencer().Capture()
			if after.Cursor != before.Cursor {
				t.Fatal("rejected state partially published")
			}
			if err := publish(item.value, nil); err != nil {
				t.Fatal(err)
			}
			rig.request.CapabilitySchemaId, rig.request.Arguments = item.description.SchemaId, item.invalid
			if response, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request); err == nil && response != nil && response.Status == actions.ActionStatus_ACTION_STATUS_DISPATCHED {
				t.Fatal("unadvertised operation dispatched")
			}
			if driver.Count() != 0 {
				t.Fatal("unadvertised operation reached driver")
			}
			rig.request.CommandId += "-valid"
			rig.request.Arguments = item.valid
			id := nha025ID(t, rig.request)
			driver.call = func(_ context.Context, command integrations.AdmittedCommand) error {
				request, err := command.Request()
				if err != nil {
					return err
				}
				if !proto.Equal(request.Arguments, item.valid) {
					t.Fatal("driver received different typed arguments")
				}
				if err := command.Acknowledge(); err != nil {
					return err
				}
				cause, err := command.Causality()
				if err != nil {
					return err
				}
				if err := publish(item.value, &cause); err != nil {
					return err
				}
				nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
				// This fixture's explicit protocol double is distinct from an
				// observation or a nil dispatch result. No physical I/O exists.
				return command.ConfirmProtocol(&nha025Proof{qualified: true})
			}
			if response, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request); err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
				t.Fatalf("typed dispatch: %v %v", response, err)
			}
			nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_CONFIRMED)
			if driver.Count() != 1 {
				t.Fatal("typed observation replayed action")
			}
			if err := rig.session.Withdraw(context.Background(), rig.binding); err != nil {
				t.Fatal(err)
			}
			rig.request.CommandId += "-withdrawn"
			if response, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request); err == nil && response != nil && response.Status == actions.ActionStatus_ACTION_STATUS_DISPATCHED {
				t.Fatal("withdrawn capability dispatched")
			}
			if driver.Count() != 1 {
				t.Fatal("withdrawn operation reached driver")
			}
		})
	}
}
