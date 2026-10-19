package automationtriggers

import (
	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/domain"
	"testing"
	"time"
)

func TestNHA105SolarStateProjectionHasNoNumericEpochFallback(t *testing.T) {
	missing, _ := domain.SolarOccurrenceMissing(domain.SolarNoEventWithinSearchHorizon)
	event, _ := domain.NewSolarEventValue(domain.SolarDawn, missing)
	value, _ := domain.NewSolarEventStateValue(event)
	id, _ := domain.ParseEntityID("nha105-trigger")
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	source, _ := domain.NewSourceStamp("nha105", nil)
	at := time.Now().UTC()
	state, err := domain.NewObservedStateSnapshot(id, quality, &value, domain.StateTimes{ReceivedAt: at, LastChanged: at, LastUpdated: at, LastReported: at}, source, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if text, present := observedText(ir.AttributeUnspecified, state); !present || text != "unknown" {
		t.Fatal("solar absence became invented state time")
	}
	if _, measurement := value.Measurement(); measurement {
		t.Fatal("solar event became numeric epoch")
	}
}
