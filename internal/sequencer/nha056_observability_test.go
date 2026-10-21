package sequencer

import (
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/observability"
)

func TestNHA056SequencerFactsAndCumulativeQueues(t *testing.T) {
	epoch, err := observability.NewProcessObservationEpoch()
	if err != nil {
		t.Fatal(err)
	}
	cells, err := observability.NewCells(epoch)
	if err != nil {
		t.Fatal(err)
	}
	limits, err := NewQueueLimits(1, 1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	sequence, err := NewWithObservation(RealClock{}, 32, limits, cells)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha056-sequence")
	if err != nil {
		t.Fatal(err)
	}
	absent, err := sequence.IntegrationHealth(instance)
	if err != nil || absent.GetNoReport() == nil {
		t.Fatal("missing report invented", err)
	}
	sub, _, err := sequence.SubscribeUI()
	if err != nil {
		t.Fatal(err)
	}
	sequence.mu.Lock()
	sequence.publish(Entry{cursor: Cursor{epoch: sequence.epoch, seq: 1}, kind: KindHealth, receivedAt: time.Now()})
	sequence.publish(Entry{cursor: Cursor{epoch: sequence.epoch, seq: 2}, kind: KindHealth, receivedAt: time.Now()})
	sequence.mu.Unlock()
	queues, lags, err := sequence.ObservationMetrics()
	if err != nil {
		t.Fatal(err)
	}
	if queues[0].GetInstalled().GetLostDeliveriesTotal().GetExact() != 1 || lags[0].GetInstalled().GetInvalidated() == nil {
		t.Fatal("real UI overflow not cumulative or invalidated")
	}
	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _, err := sequence.SubscribeUI()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	queues, _, err = sequence.ObservationMetrics()
	if err != nil || queues[0].GetInstalled().GetLostDeliveriesTotal().GetExact() != 1 {
		t.Fatal("close erased queue totals", err)
	}
	if _, err := sequence.IntegrationHealth(domain.IntegrationInstanceID{}); err == nil {
		t.Fatal("zero instance admitted")
	}
}
