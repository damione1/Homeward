package configsimulation

import (
	"testing"
	"time"
)

func TestNHA054FirstOccurrenceTimerCanBeRearmedAfterTriggerWake(t *testing.T) {
	clock := &fixtureClock{start: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC), limit: time.Minute, cancel: func() { t.Error("future recurrence consumed virtual budget") }}
	trigger := &firstTriggerClock{base: clock}
	first := trigger.NewTimer(2 * time.Second)
	// The real trigger engine can select a queued wake before a fired timer.
	// That timer is stopped/discarded; the same due occurrence must rearm.
	first.Stop()
	rearmed := trigger.NewTimer(0)
	select {
	case <-rearmed.C():
	default:
		t.Fatal("first occurrence lost after scheduling wake")
	}
	if clock.MonoNow() != 2*time.Second {
		t.Fatal("rearming advanced fixture time twice")
	}
	recurrence := trigger.NewTimer(24 * time.Hour)
	select {
	case <-recurrence.C():
		t.Fatal("second occurrence claimed covered")
	default:
	}
	if clock.MonoNow() != 2*time.Second {
		t.Fatal("future recurrence advanced virtual time")
	}
}
