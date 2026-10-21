package integrations

import (
	"testing"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/observability"
)

func TestNHA056ActionTransitionCountsOnceWithoutSubscriber(t *testing.T) {
	epoch, err := observability.NewProcessObservationEpoch()
	if err != nil {
		t.Fatal(err)
	}
	cells, err := observability.NewCells(epoch)
	if err != nil {
		t.Fatal(err)
	}
	tracker := newActionTracker(realActionClock{})
	tracker.observation = cells
	entry := &actionEntry{snapshot: ActionSnapshot{Status: actions.ActionStatus_ACTION_STATUS_DISPATCHED}}
	tracker.advance(entry, actions.ActionStatus_ACTION_STATUS_INDETERMINATE)
	tracker.advance(entry, actions.ActionStatus_ACTION_STATUS_INDETERMINATE)
	tracker.advance(entry, actions.ActionStatus_ACTION_STATUS_CONFIRMED)
	if cells.ActionCounters().ProgressIndeterminateTotal.GetExact() != 1 {
		t.Fatal("terminal progression recounted without a watcher")
	}
	for _, phase := range []PilotPhase{PilotStarting, PilotRunning, PilotRetrying, PilotDegraded, PilotStopped} {
		converted, err := observationPilotPhase(phase)
		if err != nil || converted == obs.PilotPhase_PILOT_PHASE_UNSPECIFIED {
			t.Fatal("known pilot phase lost", err)
		}
	}
	for _, phase := range []PilotPhase{PilotUnspecified, PilotPhase(255)} {
		if _, err := observationPilotPhase(phase); err == nil {
			t.Fatal("unknown pilot phase admitted")
		}
	}
	if _, err := (*Supervisor)(nil).ObservationPhases(); err == nil {
		t.Fatal("nil Supervisor became an empty owner")
	}
}
