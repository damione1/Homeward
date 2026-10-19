package sequencer_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/sequencer"
)

func TestNHA030AtomicReconciliationBoundaryAndRefusal(t *testing.T) {
	limits, err := sequencer.NewQueueLimits(4, 2, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	sequence, host, _ := nha019Fixture(t, limits)
	publisher := nha019Start(t, host, "nha030-boundary")
	first, err := sequence.CaptureRecorder(nil, sequencer.Cursor{}, 4<<20, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Subscription.Close()
	if len(first.States) != 1 {
		t.Fatalf("fixture's already-admitted state is absent from image: %d", len(first.States))
	}
	for i := 0; i < 3; i++ {
		nha019Publish(t, publisher, i%2 == 0)
	}
	before, err := first.Subscription.Status()
	if err != nil || before.PendingLoss == nil || before.PendingLoss.Count != 1 {
		t.Fatalf("expected one known overflow: %+v %v", before, err)
	}
	if _, err := sequence.CaptureRecorder(first.Subscription, first.Cursor, 512, 4<<20); !errors.Is(err, sequencer.ErrRecorderSnapshotTooLarge) {
		t.Fatalf("small image was admitted or rejected for wrong reason: %v", err)
	}
	afterRefusal, err := first.Subscription.Status()
	if err != nil || afterRefusal.Closed || afterRefusal.Queued != before.Queued || afterRefusal.PendingLoss == nil || afterRefusal.PendingLoss.Count != before.PendingLoss.Count {
		t.Fatalf("refusal mutated live mailbox: %+v %v", afterRefusal, err)
	}
	second, err := sequence.CaptureRecorder(first.Subscription, first.Cursor, 4<<20, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Subscription.Close()
	if len(second.States) != 1 || second.Abandoned == nil || second.Abandoned.PendingLoss == nil || second.Abandoned.PendingLoss.Count != 1 {
		t.Fatalf("replacement lost state or known loss count: %+v", second)
	}
	_, boundary, _ := second.Cursor.Components()
	_, through, _ := second.Abandoned.Through.Components()
	if boundary != through {
		t.Fatalf("abandoned interval did not reach image boundary: %d != %d", through, boundary)
	}
	nha019Publish(t, publisher, false)
	delivery, err := nha019Next(t, second.Subscription)
	entry, ok := delivery.Entry()
	if err != nil || !ok {
		t.Fatalf("new subscription did not receive suffix: %v %+v", err, delivery)
	}
	_, sequenceNumber, _ := entry.Cursor().Components()
	if sequenceNumber != boundary+1 {
		t.Fatalf("live suffix crossed capture boundary: got %d after %d", sequenceNumber, boundary)
	}
	if _, err := sequence.CaptureRecorder(first.Subscription, first.Cursor, 4<<20, 4<<20); !errors.Is(err, sequencer.ErrInvalidInput) {
		t.Fatalf("closed predecessor accepted: %v", err)
	}
}

func TestNHA030RecorderChargeIncludesLongCausality(t *testing.T) {
	limits, err := sequencer.NewQueueLimits(4, 2, 2, 8)
	if err != nil {
		t.Fatal(err)
	}
	sequence, host, _ := nha019Fixture(t, limits)
	publisher := nha019Start(t, host, "nha030-long-causality")
	capture, err := sequence.CaptureRecorder(nil, sequencer.Cursor{}, 4<<20, 2048)
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Subscription.Close()
	cause, err := domain.ParseCausationID(strings.Repeat("c", 4096))
	if err != nil {
		t.Fatal(err)
	}
	causality, err := domain.NewCausality(&cause, nil)
	if err != nil {
		t.Fatal(err)
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	value := domain.NewSwitchValue(true)
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	observation, err = observation.WithCausality(causality)
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.session.Publish(context.Background(), publisher.binding, observation); err != nil {
		t.Fatal(err)
	}
	status, err := capture.Subscription.Status()
	if err != nil || status.Queued != 0 || status.QueuedBytes != 0 || status.Lost != 1 {
		t.Fatalf("long causal identity bypassed Recorder byte cap: %+v %v", status, err)
	}
}

func TestNHA030UnavailableLastKnownUnitCountsAgainstQueue(t *testing.T) {
	_, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	units := domain.NewUnitCatalog()
	longUnit := strings.Repeat("u", 600000)
	if err := units.Register(longUnit, domain.DimensionPower, 1, 0); err != nil {
		t.Fatal(err)
	}
	catalog, err := contracts.NewCapabilityCatalog(units)
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(fixture, fixture, catalog)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha030-long-unit")
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("meter", "power")
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{{
		SchemaId: contracts.MeasurementSchema, Major: 1, DisplayName: "Power", Endpoint: "power",
		Constraints: &registry.CapabilityDescription_Measurement{Measurement: &registry.MeasurementConstraints{
			Unit: longUnit, Precision: 1, Semantic: registry.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT,
			Aggregation: registry.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT,
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		t.Fatalf("fake measurement binding: %v %d", err, len(bindings))
	}
	sequence := fixture.Sequencer()
	capture, err := sequence.CaptureRecorder(nil, sequencer.Cursor{}, 4<<20, 512<<10)
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Subscription.Close()
	unit, err := units.Parse(longUnit)
	if err != nil {
		t.Fatal(err)
	}
	measurement, err := domain.NewMeasurement(1, unit, 1, domain.SemanticMeasurement, domain.AggregationCurrent)
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.NewMeasurementValue(measurement)
	if err != nil {
		t.Fatal(err)
	}
	available, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	unavailable, err := domain.NewQuality(domain.AvailabilityUnavailable, domain.FreshnessStale, domain.ProvenanceEstimated)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	current, err := integrations.NewStateObservation("power", available, &value, nil, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Publish(context.Background(), bindings[0], current); err != nil {
		t.Fatal(err)
	}
	missing, err := integrations.NewStateObservation("power", unavailable, nil, nil, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Publish(context.Background(), bindings[0], missing); err != nil {
		t.Fatal(err)
	}
	entries, _, err := sequence.ReadAfter(capture.Cursor, 2)
	if err != nil || len(entries) != 2 {
		t.Fatalf("expected two state admissions: %d %v", len(entries), err)
	}
	_, projected, _, ok := entries[1].State()
	if !ok {
		t.Fatal("unavailable report did not remain a state")
	}
	if _, present := projected.CurrentValue(); present {
		t.Fatal("unavailable report retained current value")
	}
	if _, display := projected.DisplayValue(); !display || entries[1].EstimatedBytes() <= 512<<10 {
		t.Fatalf("last-known unit was omitted from Recorder charge: display=%v bytes=%d", display, entries[1].EstimatedBytes())
	}
	status, err := capture.Subscription.Status()
	if err != nil || status.QueuedBytes != 0 || status.Lost != 2 {
		t.Fatalf("long-unit states bypassed queue budget: %+v %v", status, err)
	}
}

func TestNHA030RecorderQueueByteLimitReportsLoss(t *testing.T) {
	limits, err := sequencer.NewQueueLimits(4, 2, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	sequence, host, _ := nha019Fixture(t, limits)
	publisher := nha019Start(t, host, "nha030-byte-limit")
	capture, err := sequence.CaptureRecorder(nil, sequencer.Cursor{}, 4<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Subscription.Close()
	nha019Emit(t, publisher, 1)
	status, err := capture.Subscription.Status()
	if err != nil || status.Queued != 0 || status.QueuedBytes != 0 || status.Lost != 1 || status.PendingLoss == nil || status.PendingLoss.Count != 1 {
		t.Fatalf("oversize Recorder entry was not reported: %+v %v", status, err)
	}
	delivery, err := nha019Next(t, capture.Subscription)
	if !errors.Is(err, sequencer.ErrRecorderGap) {
		t.Fatalf("Recorder loss not delivered: %+v %v", delivery, err)
	}
}

func TestNHA030RecorderCursorValidationAndSameBoundary(t *testing.T) {
	sequence, err := sequencer.New(sequencer.RealClock{}, 8)
	if err != nil {
		t.Fatal(err)
	}
	first, err := sequence.CaptureRecorder(nil, sequencer.Cursor{}, 4<<20, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Subscription.Close()
	if _, err := sequence.CaptureRecorder(first.Subscription, sequencer.Cursor{}, 4<<20, 4<<20); !errors.Is(err, sequencer.ErrInvalidInput) {
		t.Fatalf("zero predecessor cursor accepted: %v", err)
	}
	otherEpoch, err := sequencer.ParseCursor("ffffffffffffffffffffffffffffffff", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sequence.CaptureRecorder(first.Subscription, otherEpoch, 4<<20, 4<<20); !errors.Is(err, sequencer.ErrInvalidInput) {
		t.Fatalf("foreign epoch accepted: %v", err)
	}
	status, err := first.Subscription.Status()
	if err != nil || status.Closed {
		t.Fatalf("invalid replacement closed live predecessor: %+v %v", status, err)
	}
	second, err := sequence.CaptureRecorder(first.Subscription, first.Cursor, 4<<20, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Subscription.Close()
	if second.Abandoned != nil || second.Cursor != first.Cursor {
		t.Fatalf("same boundary invented an abandoned interval: %+v", second.Abandoned)
	}
}
