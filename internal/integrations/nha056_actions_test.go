package integrations_test

import (
	"context"
	"testing"
	"time"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/integrations"
)

func TestNHA056RealActionExpiresOnceWithoutWatchOrReplay(t *testing.T) {
	driver := &nha025Driver{}
	rig := nha025Setup(t, driver, nil)
	clock := &nha025Clock{armed: make(chan struct{}, 16)}
	if err := rig.host.SetActionClock(clock); err != nil {
		t.Fatal(err)
	}
	var saved integrations.AdmittedCommand
	driver.call = func(_ context.Context, command integrations.AdmittedCommand) error {
		saved = command
		return command.Acknowledge()
	}
	response, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if err != nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatal("real dispatch failed", err)
	}
	clock.Advance(2 * time.Minute)
	metrics, err := rig.host.ObservationActions()
	if err != nil || metrics.ProgressIndeterminateTotal.GetExact() != 1 {
		t.Fatal("expiration depended on a human Watch", err)
	}
	_ = saved.Acknowledge()
	for range 10 {
		metrics, err = rig.host.ObservationActions()
		if err != nil || metrics.ProgressIndeterminateTotal.GetExact() != 1 {
			t.Fatal("read or late ACK recounted progression", err)
		}
	}
	response, err = rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if err != nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || driver.Count() != 1 {
		t.Fatal("observation introduced physical replay", err)
	}
}
func TestNHA056RealFirstAttemptIndeterminateSurvivesReplay(t *testing.T) {
	entered := make(chan struct{})
	driver := &nha025Driver{call: func(ctx context.Context, _ integrations.AdmittedCommand) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}}
	rig := nha025Setup(t, driver, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan *actions.CommandResponse, 1)
	go func() { response, _ := rig.service.Dispatch(ctx, nha022Actor(t), rig.request); result <- response }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("real dispatch barrier not reached")
	}
	cancel()
	select {
	case response := <-result:
		if response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
			t.Fatal("uncertain attempt was not preserved")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("real attempt did not return")
	}
	for range 3 {
		if _, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request); err != nil && driver.Count() != 1 {
			t.Fatal("replay changed effects", err)
		}
	}
	metrics, err := rig.host.ObservationActions()
	if err != nil {
		t.Fatal(err)
	}
	for _, ledger := range metrics.FirstAttempts {
		if ledger.Ledger == obs.AttemptLedger_ATTEMPT_LEDGER_VOLATILE && ledger.IndeterminateTotal.GetExact() != 1 {
			t.Fatal("first attempt counted its replay")
		}
	}
	if driver.Count() != 1 {
		t.Fatal("metrics introduced a resend")
	}
}
