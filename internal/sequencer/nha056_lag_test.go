package sequencer_test

import (
	"context"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
)

func TestNHA056EdgeLagStartsAtRealUndeliveredHeadAfterFilteredPositions(t *testing.T) {
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha056-lag-head")
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := fake.NewDriver(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	binding, err := driver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	sequence := fixture.Sequencer()
	edge, _, err := sequence.SubscribeEdge()
	if err != nil {
		t.Fatal(err)
	}
	defer edge.Close()
	report, err := integrations.NewHealthReport(integrations.HealthDegraded, "NHA056_PRIVATE_HEALTH_DETAIL_CANARY")
	if err != nil {
		t.Fatal(err)
	}
	for range 89 {
		if err := session.ReportHealth(context.Background(), report); err != nil {
			t.Fatal(err)
		}
	}
	_, lags, err := sequence.ObservationMetrics()
	if err != nil || lags[1].GetInstalled().GetSameEpochPositions() != 0 {
		t.Fatal("filtered positions gave an empty edge queue lag", err)
	}
	event, err := integrations.NewDeviceEvent("fixture.relay.observed", time.Now().UTC(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Emit(context.Background(), binding, event); err != nil {
		t.Fatal(err)
	}
	_, lags, err = sequence.ObservationMetrics()
	if err != nil || lags[1].GetInstalled().GetSameEpochPositions() != 1 {
		t.Fatal("lag used lastDelivered instead of actual undelivered head", err)
	}
	if _, err := edge.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, lags, err = sequence.ObservationMetrics()
	if err != nil || lags[1].GetInstalled().GetSameEpochPositions() != 0 {
		t.Fatal("empty healthy queue retained lag", err)
	}
}

func TestNHA056ActualUIPublicationsCoalesceAndRetainTheirTotals(t *testing.T) {
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha056-ui-coalescence")
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := fake.NewDriver(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	binding, err := driver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	sequence := fixture.Sequencer()
	sub, _, err := sequence.SubscribeUI()
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	for i := range 2 {
		if err := session.Publish(context.Background(), binding, nha018Observation(t, i+1, "nha056-ui")); err != nil {
			t.Fatal(err)
		}
	}
	queues, lags, err := sequence.ObservationMetrics()
	if err != nil {
		t.Fatal(err)
	}
	queue := queues[0].GetInstalled()
	if queue == nil || queue.QueuedEntries.GetValue() != 1 || queue.CoalescedTotal.GetExact() != 1 || queue.LostDeliveriesTotal.GetExact() != 1 || lags[0].GetInstalled().GetInvalidated() == nil {
		t.Fatal("actual UI coalescence did not publish its loss and cumulative total")
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
	if err != nil || queues[0].GetInstalled().QueuedEntries.GetValue() != 0 || queues[0].GetInstalled().CoalescedTotal.GetExact() != 1 || queues[0].GetInstalled().LostDeliveriesTotal.GetExact() != 1 {
		t.Fatal("replacing the UI subscription erased producer totals", err)
	}
}
