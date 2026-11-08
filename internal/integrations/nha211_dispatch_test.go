package integrations_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	controls "github.com/damione1/Homeward/gen/home/controls/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

func TestNHA211SentTerminalCannotBecomeAnACKOrReplay(t *testing.T) {
	driver := &nha025Driver{}
	rig := nha025Setup(t, driver, nil)
	clock := &nha025Clock{armed: make(chan struct{}, 16)}
	if err := rig.host.SetActionClock(clock); err != nil {
		t.Fatal(err)
	}
	var saved integrations.AdmittedCommand
	driver.call = func(_ context.Context, command integrations.AdmittedCommand) error {
		saved = command
		return command.SentUnverified()
	}
	response, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if err != nil || response.Status != actions.ActionStatus_ACTION_STATUS_SENT_UNVERIFIED {
		t.Fatal("dispatch not terminal", err, response)
	}
	id := nha025ID(t, rig.request)
	nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_SENT_UNVERIFIED)
	clock.Advance(2 * time.Minute)
	if err := saved.Acknowledge(); !errors.Is(err, integrations.ErrActionTerminal) {
		t.Fatal("late ACK changed terminal", err)
	}
	nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_SENT_UNVERIFIED)
	response, err = rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if err != nil || response.Status != actions.ActionStatus_ACTION_STATUS_SENT_UNVERIFIED || driver.Count() != 1 {
		t.Fatal("duplicate replayed")
	}
}

func TestNHA211EphemeralCredentialRejectsWrongBindingAndClears(t *testing.T) {
	driver := &nha025Driver{}
	rig := nha025Setup(t, driver, nil)
	id := nha025ID(t, rig.request)
	_, entity, _ := rig.binding.IDs()
	ctx, release, err := integrations.WithEphemeralAlarmCode(context.Background(), id, nha022Actor(t), entity, controls.AlarmOperation_ALARM_OPERATION_DISARM, []byte("582901"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	// This is an admitted switch, so the exact alarm scope must reject extraction.
	driver.call = func(ctx context.Context, command integrations.AdmittedCommand) error {
		code, err := command.TakeEphemeralAlarmCode(ctx)
		if err == nil || len(code) != 0 {
			t.Error("foreign intent obtained credential")
		}
		return command.SentUnverified()
	}
	if _, err = rig.service.Dispatch(ctx, nha022Actor(t), rig.request); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", ctx, ctx, ctx), "582901") {
		t.Fatal("context formatting disclosed credential")
	}
	release()
	for _, code := range [][]byte{nil, []byte("12x"), []byte(strings.Repeat("1", 33))} {
		if _, _, err := integrations.WithEphemeralAlarmCode(context.Background(), id, nha022Actor(t), entity, controls.AlarmOperation_ALARM_OPERATION_DISARM, code); err == nil {
			t.Fatal("invalid credential admitted")
		}
	}
	_, _, err = integrations.WithEphemeralAlarmCode(context.Background(), domain.CommandID{}, nha022Actor(t), entity, controls.AlarmOperation_ALARM_OPERATION_DISARM, []byte("582901"))
	if err == nil {
		t.Fatal("missing command identity admitted")
	}
}

func TestNHA211SendCompletionPreservesEarlierACKAndConfirmation(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(fmt.Sprint(confirmed), func(t *testing.T) {
			driver := &nha025Driver{}
			rig := nha025Setup(t, driver, nil)
			want := actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED
			driver.call = func(_ context.Context, command integrations.AdmittedCommand) error {
				if err := command.Acknowledge(); err != nil {
					return err
				}
				if confirmed {
					want = actions.ActionStatus_ACTION_STATUS_CONFIRMED
					if err := command.ConfirmProtocol(&nha025Proof{qualified: true}); err != nil {
						return err
					}
				}
				return command.SentUnverified()
			}
			response, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request)
			if err != nil || response.Status == actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
				t.Fatal("send degraded prior proof", response, err)
			}
			nha025Status(t, rig.host, nha025ID(t, rig.request), want)
		})
	}
}
