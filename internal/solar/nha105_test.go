package solar

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/domain"
)

func nha105Settings(t testing.TB, latitude, longitude, elevation float64, zone string) Settings {
	t.Helper()
	settings, err := ParseSettings(Input{Latitude: &latitude, Longitude: &longitude, ElevationMeters: &elevation, Timezone: zone})
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

func nha105Calculator(t testing.TB, latitude, longitude, elevation float64, zone string) *Calculator {
	t.Helper()
	calculator, err := NewCalculator(nha105Settings(t, latitude, longitude, elevation, zone))
	if err != nil {
		t.Fatal(err)
	}
	return calculator
}

func nha105Instant(t testing.TB, raw string) time.Time {
	t.Helper()
	instant, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatal(err)
	}
	return instant
}

func nha105Present(t testing.TB, occurrence domain.SolarOccurrence) time.Time {
	t.Helper()
	instant, _, kind, err := occurrence.Components()
	if err != nil || kind != domain.SolarOccurrencePresent || instant.Location() != time.UTC {
		t.Fatalf("expected present UTC solar event: kind %d, error %v", kind, err)
	}
	return instant
}

func TestNHA105PrivateSettingsAdmissionAndCopies(t *testing.T) {
	zero := 0.0
	settings, err := ParseSettings(Input{Latitude: &zero, Longitude: &zero, Timezone: "UTC"})
	if err != nil || !settings.Valid() {
		t.Fatalf("required present zero coordinates rejected: %v", err)
	}
	zero = 25
	observer, err := settings.Observer()
	if err != nil {
		t.Fatal(err)
	}
	latitude, longitude, elevation, err := observer.Components()
	if err != nil || latitude != 0 || longitude != 0 || elevation != 0 {
		t.Fatal("input pointer mutation changed admitted settings or default altitude")
	}
	for _, input := range []Input{
		{}, {Latitude: &zero}, {Longitude: &zero},
		{Latitude: &zero, Longitude: &zero},
		{Latitude: &zero, Longitude: &zero, Timezone: "Local"},
		{Latitude: &zero, Longitude: &zero, Timezone: "unknown/synthetic"},
	} {
		if _, err := ParseSettings(input); err == nil {
			t.Fatal("incomplete private settings admitted")
		}
	}
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -91, 91} {
		if _, err := ParseSettings(Input{Latitude: &bad, Longitude: &zero, Timezone: "UTC"}); err == nil {
			t.Fatal("invalid latitude admitted")
		}
	}
	for _, bad := range []float64{math.NaN(), math.Inf(1), -181, 181} {
		if _, err := ParseSettings(Input{Latitude: &zero, Longitude: &bad, Timezone: "UTC"}); err == nil {
			t.Fatal("invalid longitude admitted")
		}
	}
	badAltitude := math.NaN()
	if _, err := ParseSettings(Input{Latitude: &zero, Longitude: &zero, ElevationMeters: &badAltitude, Timezone: "UTC"}); err == nil {
		t.Fatal("invalid altitude admitted")
	}
	if _, err := NewCalculator(Settings{}); err == nil {
		t.Fatal("zero settings created a calculator")
	}
	if _, _, _, err := (Observer{}).Components(); err == nil {
		t.Fatal("zero observer admitted")
	}
}

func TestNHA105PrivateSettingsDiagnosticsAreRedacted(t *testing.T) {
	settings := nha105Settings(t, 45.125, -75.875, 321.25, "America/Toronto")
	observer, err := settings.Observer()
	if err != nil {
		t.Fatal(err)
	}
	calculator, err := NewCalculator(settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{settings, &settings, observer, &observer, calculator} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			text := fmt.Sprintf(verb, value)
			if !strings.Contains(text, "<private>") || strings.Contains(text, "45.125") || strings.Contains(text, "75.875") || strings.Contains(text, "321.25") || strings.Contains(text, "Toronto") {
				t.Fatal("private settings diagnostic exposed private data")
			}
		}
	}
	if !strings.Contains(LicenseText(), "Apache License") || AstralVersion != "2.2" || len(AstralCommit) != 40 || len(HAProfileCommit) != 40 {
		t.Fatal("source attribution is incomplete")
	}
}

func TestNHA105DateAbsenceDoesNotEraseFutureOccurrence(t *testing.T) {
	calculator := nha105Calculator(t, 69.6, 18.8, 0, "Europe/Oslo")
	date, err := calendars.ParseDate("2016-06-01")
	if err != nil {
		t.Fatal(err)
	}
	after := nha105Instant(t, "2016-06-01T00:00:00Z")
	// Published upstream regression, HA tests/helpers/test_sun.py at the pinned
	// profile commit. These are public synthetic fixture inputs, not house data.
	for event, expected := range map[domain.SolarEventKind]string{
		domain.SolarRising:  "2016-07-24T22:59:45.689645Z",
		domain.SolarSetting: "2016-07-25T22:17:13.503932Z",
	} {
		atDate, err := calculator.EventAtDate(context.Background(), event, date)
		if err != nil {
			t.Fatal(err)
		}
		instant, reason, kind, err := atDate.Components()
		if err != nil || !instant.IsZero() || kind != domain.SolarOccurrenceAbsent || reason != domain.SolarNoTransitOnDate {
			t.Fatalf("daily absence was not explicit: %d %d, %v", kind, reason, err)
		}
		next, err := calculator.NextEvent(context.Background(), event, after)
		if err != nil {
			t.Fatal(err)
		}
		got := nha105Present(t, next)
		if difference := got.Sub(nha105Instant(t, expected)); difference < -time.Second || difference > time.Second {
			t.Fatalf("public HA regression differs by %s", difference)
		}
	}
}

func TestNHA105NextEventIsStrictAtOwnEqualityBoundary(t *testing.T) {
	calculator := nha105Calculator(t, 45, -75, 0, "America/Toronto")
	base := nha105Instant(t, "2024-06-21T12:00:00Z")
	for _, event := range domain.SolarEventKinds() {
		first, err := calculator.NextEvent(context.Background(), event, base)
		if err != nil {
			t.Fatal(err)
		}
		anchor := nha105Present(t, first)
		before, err := calculator.NextEvent(context.Background(), event, anchor.Add(-time.Microsecond))
		if err != nil || !nha105Present(t, before).Equal(anchor) {
			t.Fatalf("event immediately before its boundary was skipped: %v", err)
		}
		for _, after := range []time.Time{anchor, anchor.Add(time.Microsecond)} {
			next, err := calculator.NextEvent(context.Background(), event, after)
			if err != nil {
				t.Fatal(err)
			}
			if got := nha105Present(t, next); !got.After(after) || !got.After(anchor) {
				t.Fatal("same event was returned at or after equality")
			}
		}
	}
}

func TestNHA105SearchExhaustionIsTypedAndBounded(t *testing.T) {
	calculator := nha105Calculator(t, 69.6, 18.8, 10000000, "Europe/Oslo")
	request, err := requestFor(domain.SolarRising)
	if err != nil {
		t.Fatal(err)
	}
	evaluated := 0
	result, err := calculator.nextEvent(context.Background(), request, nha105Instant(t, "2016-06-01T00:00:00Z"), &evaluated)
	if err != nil {
		t.Fatal(err)
	}
	instant, reason, kind, err := result.Components()
	if err != nil || !instant.IsZero() || reason != domain.SolarNoEventWithinSearchHorizon || kind != domain.SolarOccurrenceAbsent || evaluated != MaxSearchDates {
		t.Fatalf("exhaustion was not typed/bounded: dates=%d kind=%d reason=%d error=%v", evaluated, kind, reason, err)
	}
}

type nha105NilContext struct{}

func (*nha105NilContext) Deadline() (time.Time, bool) { panic("nil context called") }
func (*nha105NilContext) Done() <-chan struct{}       { panic("nil context called") }
func (*nha105NilContext) Err() error                  { panic("nil context called") }
func (*nha105NilContext) Value(any) any               { panic("nil context called") }

func TestNHA105ContextAndInvalidValuesNeverBecomeAstronomicalAbsence(t *testing.T) {
	calculator := nha105Calculator(t, 0, 0, 0, "UTC")
	after := nha105Instant(t, "2024-01-01T00:00:00Z")
	var nilContext *nha105NilContext
	for _, ctx := range []context.Context{nil, nilContext} {
		if _, err := calculator.NextEvent(ctx, domain.SolarSetting, after); err == nil {
			t.Fatal("nil/typed-nil context admitted")
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := calculator.NextEvent(canceled, domain.SolarSetting, after); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation converted to absence: %v", err)
	}
	expired, cancel := context.WithDeadline(context.Background(), time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	defer cancel()
	_, err := calculator.NextEvent(expired, domain.SolarSetting, after)
	var typed *Error
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &typed) || typed.Code() != BudgetExceeded {
		t.Fatalf("deadline was not a typed budget failure: %v", err)
	}
	if _, err := calculator.NextEvent(context.Background(), 255, after); err == nil {
		t.Fatal("unknown event admitted")
	}
	if _, err := calculator.NextEvent(context.Background(), domain.SolarSetting, time.Time{}); err == nil {
		t.Fatal("zero instant admitted")
	}
	var uninitialized *Calculator
	if _, err := uninitialized.Snapshot(context.Background(), after); err == nil {
		t.Fatal("nil calculator admitted")
	}
}

func TestNHA105ComputeBoundedPhasesAndPrivateFreeOutput(t *testing.T) {
	for _, latitude := range []float64{0, 45, -35, 90, -90} {
		calculator := nha105Calculator(t, latitude, 0, 0, "UTC")
		computed, err := calculator.Compute(context.Background(), nha105Instant(t, "2024-06-21T12:00:00Z"))
		if err != nil {
			t.Fatal(err)
		}
		value, now, phase, change, err := computed.Components()
		if err != nil || !change.After(now) {
			t.Fatalf("invalid phase computation: %v", err)
		}
		if _, err := phase.PositionInterval(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := computed.NextPosition(); err != nil {
			t.Fatal(err)
		}
		if dates, err := computed.DateEvaluations(); err != nil || dates < 1 || dates > 12*MaxSearchDates {
			t.Fatalf("unbounded date evaluations: %d %v", dates, err)
		}
		parts, err := value.Components()
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range parts.Next {
			instant, reason, kind, err := event.Components()
			if err != nil || (kind == domain.SolarOccurrencePresent && !instant.After(now)) || (kind == domain.SolarOccurrenceAbsent && reason != domain.SolarNoEventWithinSearchHorizon) {
				t.Fatal("snapshot event is stale or has a daily absence")
			}
		}
		text := fmt.Sprintf("%#v", computed)
		if strings.Contains(text, "latitude") || strings.Contains(text, "longitude") || strings.Contains(text, "observer") || strings.Contains(text, "settings") {
			t.Fatal("calculation output retained private settings")
		}
	}
	for _, test := range []struct{ input, want float64 }{{2.675, 2.67}, {-2.675, -2.67}, {0.005, 0.01}} {
		got, err := roundHA2(test.input)
		if err != nil || got != test.want {
			t.Fatalf("Python-compatible decimal rounding: %v, %v", got, err)
		}
	}
}

func BenchmarkNHA105Compute(b *testing.B) {
	for _, test := range []struct {
		name      string
		latitude  float64
		elevation float64
	}{{"equator", 0, 0}, {"temperate", 45, 0}, {"polar", 90, 0}, {"exhausted", 69.6, 10000000}} {
		b.Run(test.name, func(b *testing.B) {
			calculator := nha105Calculator(b, test.latitude, 0, test.elevation, "UTC")
			now := nha105Instant(b, "2024-06-21T12:00:00Z")
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := calculator.Compute(context.Background(), now); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestNHA105PureCalculationResourceEvidence(t *testing.T) {
	for _, test := range []struct {
		name      string
		latitude  float64
		elevation float64
	}{{"temperate", 45, 0}, {"polar", 90, 0}, {"exhausted", 69.6, 10000000}} {
		t.Run(test.name, func(t *testing.T) {
			calculator := nha105Calculator(t, test.latitude, 0, test.elevation, "UTC")
			now := nha105Instant(t, "2024-06-21T12:00:00Z")
			var maximum time.Duration
			var dates int
			for i := 0; i < 32; i++ {
				started := time.Now()
				computed, err := calculator.Compute(context.Background(), now)
				elapsed := time.Since(started)
				if err != nil {
					t.Fatal(err)
				}
				if elapsed > maximum {
					maximum = elapsed
				}
				dates, err = computed.DateEvaluations()
				if err != nil || dates > 12*MaxSearchDates {
					t.Fatalf("date bound exceeded: %d, %v", dates, err)
				}
			}
			// A sample maximum is evidence for calibrating the proposed budget,
			// not a universal deadline guarantee or a heap-retention measurement.
			t.Logf("samples=32 date_evaluations=%d max_elapsed_ns=%d proposed_budget_ns=%d", dates, maximum.Nanoseconds(), (250 * time.Millisecond).Nanoseconds())
		})
	}
}
