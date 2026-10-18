package domain

import (
	"errors"
	"testing"
	"time"
)

func testNHA017Quality(t *testing.T, a Availability, f Freshness, p Provenance) Quality {
	t.Helper()
	q, err := NewQuality(a, f, p)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func testNHA017Report(t *testing.T, q Quality, value *StateValue, observed *time.Time, received time.Time, revision uint64) StateReport {
	t.Helper()
	seq := revision - 1
	source, err := NewSourceStamp("synthetic-session", &seq)
	if err != nil {
		t.Fatal(err)
	}
	report, err := NewStateReport(q, value, observed, received, source, revision)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func testNHA017Entity(t *testing.T) EntityID {
	t.Helper()
	id, err := ParseEntityID("Host:synthetic/switch")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestNHA017EqualReportAndIndependentQuality(t *testing.T) {
	id := testNHA017Entity(t)
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	falseValue := NewSwitchValue(false)
	fresh := testNHA017Quality(t, AvailabilityAvailable, FreshnessFresh, ProvenanceObserved)
	first, transition, err := ApplyStateReport(id, nil, testNHA017Report(t, fresh, &falseValue, &t0, t0, 1))
	if err != nil || !transition.ValueChanged || !transition.Updated {
		t.Fatalf("first report: %+v %v", transition, err)
	}
	if current, ok := first.CurrentValue(); !ok {
		t.Fatal("false switch treated as absent")
	} else if on, valid := current.Switch(); !valid || on {
		t.Fatal("present false switch changed")
	}
	if _, ok := first.CriticalValue(); !ok {
		t.Fatal("fresh observed value not available to a critical policy")
	}
	t1 := t0.Add(time.Second)
	same, transition, err := ApplyStateReport(id, &first, testNHA017Report(t, fresh, &falseValue, &t1, t1, 2))
	if err != nil || transition.ValueChanged || transition.Updated {
		t.Fatalf("equal report became a state change: %+v %v", transition, err)
	}
	_, _, times, _, revision, err := same.Components()
	if err != nil || revision != 2 || !times.ObservedAt.Equal(t1) || !times.ReceivedAt.Equal(t1) ||
		!times.LastReported.Equal(t1) || !times.LastChanged.Equal(t0) || !times.LastUpdated.Equal(t0) {
		t.Fatalf("equal report timestamps: %+v %v", times, err)
	}
	estimatedStale := testNHA017Quality(t, AvailabilityAvailable, FreshnessStale, ProvenanceEstimated)
	t2 := t1.Add(time.Second)
	degraded, transition, err := ApplyStateReport(id, &same, testNHA017Report(t, estimatedStale, &falseValue, nil, t2, 3))
	if err != nil || transition.ValueChanged || !transition.Updated {
		t.Fatalf("quality-only transition: %+v %v", transition, err)
	}
	_, q, times, _, _, _ := degraded.Components()
	a, f, p, err := q.Components()
	if err != nil || a != AvailabilityAvailable || f != FreshnessStale || p != ProvenanceEstimated ||
		!times.LastChanged.Equal(t0) || !times.LastUpdated.Equal(t2) || !times.LastReported.Equal(t2) || times.ObservedAt == nil || !times.ObservedAt.Equal(t1) {
		t.Fatalf("estimated and stale not preserved: %+v %+v %v", q, times, err)
	}
	if _, ok := degraded.CriticalValue(); ok {
		t.Fatal("estimated stale value admitted to critical decision")
	}
	unavailable := testNHA017Quality(t, AvailabilityUnavailable, FreshnessStale, ProvenanceObserved)
	t3 := t2.Add(time.Second)
	lost, transition, err := ApplyStateReport(id, &degraded, testNHA017Report(t, unavailable, nil, nil, t3, 4))
	if err != nil || !transition.ValueChanged || !transition.Updated {
		t.Fatalf("unavailable transition: %+v %v", transition, err)
	}
	if _, ok := lost.CurrentValue(); ok {
		t.Fatal("unavailable state has a current value")
	}
	if display, ok := lost.DisplayValue(); !ok {
		t.Fatal("last-known display value lost")
	} else if on, valid := display.Switch(); !valid || on {
		t.Fatal("last-known false switch changed")
	}
	if _, ok := lost.CriticalValue(); ok {
		t.Fatal("unavailable last-known value admitted to critical decision")
	}
	unknown := testNHA017Quality(t, AvailabilityUnknown, FreshnessStale, ProvenanceObserved)
	unknownState, _, err := ApplyStateReport(id, &lost, testNHA017Report(t, unknown, nil, nil, t3.Add(time.Second), 5))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := unknownState.CurrentValue(); ok {
		t.Fatal("unknown state became a numeric/boolean sentinel")
	}
}

func TestNHA017ZeroMeasurementAndWallClockRegression(t *testing.T) {
	id := testNHA017Entity(t)
	unit, err := NewUnitCatalog().Parse("W")
	if err != nil {
		t.Fatal(err)
	}
	measurement, err := NewMeasurement(0, unit, 1, SemanticMeasurement, AggregationCurrent)
	if err != nil {
		t.Fatal(err)
	}
	zero, err := NewMeasurementValue(measurement)
	if err != nil {
		t.Fatal(err)
	}
	q := testNHA017Quality(t, AvailabilityAvailable, FreshnessFresh, ProvenanceObserved)
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	first, _, err := ApplyStateReport(id, nil, testNHA017Report(t, q, &zero, &t0, t0, 1))
	if err != nil {
		t.Fatal(err)
	}
	backward := t0.Add(-time.Second)
	second, change, err := ApplyStateReport(id, &first, testNHA017Report(t, q, &zero, &backward, backward, 2))
	if err != nil || change.ValueChanged || change.Updated {
		t.Fatalf("NTP backward step rejected or changed value: %+v %v", change, err)
	}
	current, present := second.CurrentValue()
	if !present {
		t.Fatal("numeric zero treated as absent")
	}
	m, present := current.Measurement()
	parts, err := m.Components()
	if !present || err != nil || parts.Value != 0 {
		t.Fatalf("zero measurement lost: %+v %v", parts, err)
	}
	_, _, times, source, revision, err := second.Components()
	if err != nil || revision != 2 || !times.LastReported.Equal(backward) || !times.LastChanged.Equal(t0) {
		t.Fatalf("revision/source clocks: %+v %v", times, err)
	}
	_, seq, err := source.Components()
	if err != nil || seq == nil || *seq != 1 {
		t.Fatalf("source sequence lost: %v %v", seq, err)
	}
}

func TestNHA017InvalidReportsAndZeroValues(t *testing.T) {
	id := testNHA017Entity(t)
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	q := testNHA017Quality(t, AvailabilityAvailable, FreshnessFresh, ProvenanceObserved)
	value := NewSwitchValue(true)
	source, _ := NewSourceStamp("session", nil)
	for _, invalid := range []struct {
		quality Quality
		value   *StateValue
		time    time.Time
		source  SourceStamp
		rev     uint64
	}{
		{Quality{}, &value, t0, source, 1},
		{q, nil, t0, source, 1},
		{testNHA017Quality(t, AvailabilityUnknown, FreshnessStale, ProvenanceObserved), &value, t0, source, 1},
		{q, &value, time.Time{}, source, 1},
		{q, &value, t0, SourceStamp{}, 1},
		{q, &value, t0, source, 0},
		{q, &StateValue{}, t0, source, 1},
	} {
		if _, err := NewStateReport(invalid.quality, invalid.value, nil, invalid.time, invalid.source, invalid.rev); err == nil {
			t.Fatalf("invalid report admitted: %+v", invalid)
		}
	}
	if _, err := NewQuality(Availability(99), FreshnessFresh, ProvenanceObserved); err == nil {
		t.Fatal("future availability admitted")
	}
	if _, err := NewQuality(AvailabilityAvailable, Freshness(99), ProvenanceObserved); err == nil {
		t.Fatal("future freshness admitted")
	}
	if _, err := NewQuality(AvailabilityAvailable, FreshnessFresh, Provenance(99)); err == nil {
		t.Fatal("future provenance admitted")
	}
	if _, _, _, _, _, err := (ObservedState{}).Components(); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("zero state admitted: %v", err)
	}
	if _, _, err := ApplyStateReport(EntityID{}, nil, testNHA017Report(t, q, &value, nil, t0, 1)); err == nil {
		t.Fatal("zero entity admitted")
	}
	first, _, err := ApplyStateReport(id, nil, testNHA017Report(t, q, &value, nil, t0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ApplyStateReport(id, &first, testNHA017Report(t, q, &value, nil, t0.Add(time.Second), 1)); !errors.Is(err, ErrOutOfOrderReport) {
		t.Fatalf("repeated revision admitted: %v", err)
	}
	other, _ := ParseEntityID("Host:other")
	if _, _, err := ApplyStateReport(other, &first, testNHA017Report(t, q, &value, nil, t0.Add(time.Second), 2)); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("wrong previous entity admitted: %v", err)
	}
}

func TestNHA017SnapshotNormalizesAllClocksToUTC(t *testing.T) {
	id := testNHA017Entity(t)
	quality := testNHA017Quality(t, AvailabilityAvailable, FreshnessFresh, ProvenanceObserved)
	value := NewSwitchValue(true)
	source, err := NewSourceStamp("synthetic-session", nil)
	if err != nil {
		t.Fatal(err)
	}
	local := time.Date(2026, 10, 18, 8, 0, 0, 0, time.FixedZone("EDT", -4*60*60))
	input := StateTimes{ObservedAt: &local, ReceivedAt: local, LastChanged: local, LastUpdated: local, LastReported: local}
	snapshot, err := NewObservedStateSnapshot(id, quality, &value, input, source, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, times, _, _, err := snapshot.Components()
	if err != nil {
		t.Fatal(err)
	}
	for _, stamp := range []time.Time{*times.ObservedAt, times.ReceivedAt, times.LastChanged, times.LastUpdated, times.LastReported} {
		if stamp.Location() != time.UTC || !stamp.Equal(local) {
			t.Fatalf("snapshot stamp did not preserve instant in UTC: %v", stamp)
		}
	}
}
