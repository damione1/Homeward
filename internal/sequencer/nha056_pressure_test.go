package sequencer_test

import (
	"context"
	"errors"
	"testing"
	"time"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/sequencer"
)

func TestNHA056RealEdgeAndViewOverflowPreserveCumulativeLossWithoutAdmission(t *testing.T) {
	limits, err := sequencer.NewQueueLimits(2, 2, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	sequence, host, _ := nha019Fixture(t, limits)
	publisher := nha019Start(t, host, "nha056-edge-view-pressure")
	edge, _, err := sequence.SubscribeEdge()
	if err != nil {
		t.Fatal(err)
	}
	defer edge.Close()
	before, err := sequence.Capture()
	if err != nil {
		t.Fatal(err)
	}
	view, _, _, err := sequence.SubscribeViewAfter(before.Cursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	for range 3 {
		event, err := integrations.NewDeviceEvent("fixture.relay.pressed", time.Now().UTC(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := publisher.session.Emit(context.Background(), publisher.binding, event); err != nil {
			t.Fatal(err)
		}
	}
	after, err := sequence.Capture()
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		queues, lags, err := sequence.ObservationMetrics()
		if err != nil {
			t.Fatal(err)
		}
		for _, index := range []int{1, 3} {
			q := queues[index].GetInstalled()
			if q == nil || q.ActiveQueues.GetValue() != 1 || q.CapacityEntries.GetValue() != 2 || q.QueuedEntries.GetValue() != 0 || q.DegradedQueues.GetValue() != 1 || q.LostDeliveriesTotal.GetExact() != 3 || lags[index].GetInstalled().GetInvalidated() == nil {
				t.Fatal("actual edge/view overflow not represented exactly")
			}
			if index == 3 && q.QueuedAccountedBytes.GetValue() != 0 {
				t.Fatal("overflow retained view bytes")
			}
		}
	}
	observed, err := sequence.Capture()
	if err != nil || observed.Cursor != after.Cursor {
		t.Fatal("metrics read admitted an event", err)
	}
	if _, err := edge.Next(context.Background()); !errors.Is(err, sequencer.ErrEdgeOverflow) {
		t.Fatal("edge overflow semantics changed", err)
	}
	if _, err := view.Next(context.Background()); !errors.Is(err, sequencer.ErrViewOverflow) {
		t.Fatal("view overflow semantics changed", err)
	}
	if err := edge.Close(); err != nil {
		t.Fatal(err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	newEdge, _, err := sequence.SubscribeEdge()
	if err != nil {
		t.Fatal(err)
	}
	defer newEdge.Close()
	newView, _, _, err := sequence.SubscribeViewAfter(after.Cursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer newView.Close()
	queues, lags, err := sequence.ObservationMetrics()
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{1, 3} {
		if queues[index].GetInstalled().LostDeliveriesTotal.GetExact() != 3 || lags[index].GetInstalled().GetSameEpochPositions() != 0 {
			t.Fatal("replacement reset totals or retained stale lag")
		}
	}
	if queues[1].QueueClass != obs.QueueClass_QUEUE_CLASS_EDGE || queues[3].QueueClass != obs.QueueClass_QUEUE_CLASS_VIEW {
		t.Fatal("queue catalogue changed")
	}
}
