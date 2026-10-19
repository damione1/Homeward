package domain_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
)

func nha105Occurrence(t *testing.T, value time.Time) domain.SolarOccurrence {
	t.Helper()
	result, err := domain.SolarOccurrenceAt(value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func nha105SolarInput(t *testing.T) domain.SolarValueInput {
	t.Helper()
	absent, err := domain.SolarOccurrenceMissing(domain.SolarNoEventWithinSearchHorizon)
	if err != nil {
		t.Fatal(err)
	}
	input := domain.SolarValueInput{Horizon: domain.SolarAboveHorizon, Rising: true}
	for i := range input.Next {
		input.Next[i] = absent
	}
	noon, _ := domain.SolarNoon.Index()
	midnight, _ := domain.SolarMidnight.Index()
	input.Next[noon] = nha105Occurrence(t, time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC))
	input.Next[midnight] = nha105Occurrence(t, time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))
	return input
}

func TestNHA105SolarSumRejectsZeroAndUnknownAndPreservesPresentZero(t *testing.T) {
	if _, _, _, err := (domain.SolarOccurrence{}).Components(); !errors.Is(err, domain.ErrInvalidSolarValue) {
		t.Fatalf("zero occurrence: %v", err)
	}
	if _, err := domain.SolarOccurrenceAt(time.Time{}); err == nil {
		t.Fatal("zero timestamp admitted")
	}
	for _, reason := range []domain.SolarAbsenceReason{domain.SolarAbsenceUnspecified, 255} {
		if _, err := domain.SolarOccurrenceMissing(reason); err == nil {
			t.Fatal("invalid absence admitted")
		}
	}
	for _, event := range []domain.SolarEventKind{domain.SolarEventUnspecified, 255} {
		if _, err := event.Index(); err == nil {
			t.Fatal("invalid event index admitted")
		}
		if _, err := event.Name(); err == nil {
			t.Fatal("invalid event name admitted")
		}
	}
	seen := map[int]bool{}
	for _, event := range domain.SolarEventKinds() {
		index, err := event.Index()
		if err != nil || index < 0 || index >= domain.SolarEventCount || seen[index] {
			t.Fatal("event set is not six distinct admitted indices")
		}
		seen[index] = true
		name, _ := event.Name()
		parsed, err := domain.ParseSolarEventKind(name)
		if err != nil || parsed != event {
			t.Fatal("event name roundtrip failed")
		}
	}
	input := nha105SolarInput(t)
	value, err := domain.NewSolarValue(input)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := value.Components()
	if err != nil || parts.ElevationDegrees != 0 || parts.AzimuthDegrees != 0 {
		t.Fatalf("present zero angle lost: %v", err)
	}
	parts.Next = [domain.SolarEventCount]domain.SolarOccurrence{}
	input.Rising = false
	if copied, err := value.Components(); err != nil || !copied.Rising || copied.Next[0] == (domain.SolarOccurrence{}) {
		t.Fatal("returned or input copy mutated the admitted solar value")
	}
}

func TestNHA105SolarValueRejectsIncompleteContradictoryAndDailyAbsence(t *testing.T) {
	base := nha105SolarInput(t)
	for name, mutate := range map[string]func(*domain.SolarValueInput){
		"missing-event": func(v *domain.SolarValueInput) { v.Next[0] = domain.SolarOccurrence{} },
		"wrong-rising":  func(v *domain.SolarValueInput) { v.Rising = false },
		"wrong-horizon": func(v *domain.SolarValueInput) { v.Horizon = domain.SolarBelowHorizon },
		"unknown-state": func(v *domain.SolarValueInput) { v.Horizon = 255 },
		"nan":           func(v *domain.SolarValueInput) { v.ElevationDegrees = math.NaN() },
		"infinity":      func(v *domain.SolarValueInput) { v.AzimuthDegrees = math.Inf(1) },
		"bad-azimuth":   func(v *domain.SolarValueInput) { v.AzimuthDegrees = -1 },
		"daily-absence": func(v *domain.SolarValueInput) {
			v.Next[0], _ = domain.SolarOccurrenceMissing(domain.SolarNoTransitOnDate)
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := base
			mutate(&input)
			if _, err := domain.NewSolarValue(input); err == nil {
				t.Fatal("invalid solar value admitted")
			}
		})
	}
	daily, _ := domain.SolarOccurrenceMissing(domain.SolarNoTransitOnDate)
	if _, err := domain.NewSolarEventValue(domain.SolarDawn, daily); err == nil {
		t.Fatal("daily absence was published as a next-event absence")
	}
	if _, _, err := (domain.SolarEventValue{}).Components(); err == nil {
		t.Fatal("zero event value admitted")
	}
	if _, err := (domain.SolarValue{}).Components(); err == nil {
		t.Fatal("zero solar value admitted")
	}
}
