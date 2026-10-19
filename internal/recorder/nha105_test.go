package recorder

import (
	"context"
	"github.com/damione1/Homeward/contracts"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	solarwire "github.com/damione1/Homeward/gen/home/solar/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"testing"
	"time"
)

func TestNHA105RecorderIncludesPresentAndAbsentSolarEvents(t *testing.T) {
	host, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	writer := &nha030Writer{}
	recording, err := Start(context.Background(), fixture.Sequencer(), writer, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := recording.Stop(ctx); err != nil {
				t.Error(err)
			}
		}
	})
	instance, _ := domain.ParseIntegrationInstanceID("nha105-recorder")
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := integrations.NewSourceKey("synthetic", "dawn")
	description := &registrywire.CapabilityDescription{SchemaId: contracts.SolarEventSchema, Major: 1, Endpoint: "value", DisplayName: "Synthetic dawn", Constraints: &registrywire.CapabilityDescription_SolarEvent{SolarEvent: &solarwire.SolarEventConstraints{Event: solarwire.SolarEventKind_SOLAR_EVENT_KIND_DAWN}}}
	candidate, err := integrations.NewCandidate(key, []*registrywire.CapabilityDescription{description})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	present, _ := domain.SolarOccurrenceAt(time.Now().Add(time.Hour))
	missing, _ := domain.SolarOccurrenceMissing(domain.SolarNoEventWithinSearchHorizon)
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	for _, occurrence := range []domain.SolarOccurrence{present, missing} {
		event, _ := domain.NewSolarEventValue(domain.SolarDawn, occurrence)
		value, _ := domain.NewSolarEventStateValue(event)
		at := time.Now().UTC()
		observation, err := integrations.NewStateObservation("value", quality, &value, &at, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Publish(context.Background(), bindings[0], observation); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := recording.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	stopped = true
	writer.mu.Lock()
	defer writer.mu.Unlock()
	states, encoded := 0, 0
	for _, batch := range writer.batches {
		states += batch.StateCount()
		encoded += batch.EncodedStateBytes()
	}
	if states != 2 || encoded == 0 {
		t.Fatal("Recorder omitted solar history before drain")
	}
}
