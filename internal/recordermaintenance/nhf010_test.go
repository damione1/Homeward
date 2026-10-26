package recordermaintenance

import (
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
)

func TestNHF010EnvironmentPointsCannotBecomeNumericAggregates(t *testing.T) {
	base := point(t, 1, time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC), 0, domain.SemanticMeasurement).Input()
	for _, kind := range []domain.ValueKind{domain.ValueWeather, domain.ValuePresence, domain.ValuePresenceDetails} {
		input := base
		input.Kind = kind
		input.Numeric = nil
		accepted, err := NewPoint(input)
		if err != nil || !accepted.Valid() {
			t.Fatal("non-numeric environment point rejected", kind, err)
		}
		input.Numeric = base.Numeric
		if _, err := NewPoint(input); err == nil {
			t.Fatal("environment point accepted a numeric descriptor", kind)
		}
	}
}
