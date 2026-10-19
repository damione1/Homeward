package domain

import (
	"testing"
	"time"
	"unsafe"
)

func TestNHA097HelperPayloadDoesNotEnlargeEveryObservedState(t *testing.T) {
	// Recorder retains and copies a suffix of 1024 entries on eviction.
	// A helper payload must not be embedded in every switch observation.
	if got := unsafe.Sizeof(StateValue{}); got > 256 {
		t.Fatalf("StateValue copies helper payload in every variant: %d bytes", got)
	}
	if got := unsafe.Sizeof(ObservedState{}); got > 700 {
		t.Fatalf("ObservedState doubled helper payload in every entry: %d bytes", got)
	}
	for _, invalid := range []StateValue{
		{kind: ValueHelper},
		{kind: ValueHelper, helper: &HelperValue{}, helperRevision: 1},
	} {
		if _, err := invalid.Kind(); err == nil {
			t.Fatal("nil or zero helper payload admitted")
		}
		if _, _, present := invalid.Helper(); present {
			t.Fatal("invalid helper exposed as a value")
		}
	}
}

func TestNHA097HelperProjectionCopiesAndComparesContent(t *testing.T) {
	input := NewHelperBoolean(false)
	firstValue, err := NewHelperStateValue(input, 1)
	if err != nil {
		t.Fatal(err)
	}
	input.boolean = true // The admitted payload is an owned copy.
	if got, _, ok := firstValue.Helper(); !ok {
		t.Fatal("admitted false helper disappeared")
	} else if boolean, ok := got.Boolean(); !ok || boolean {
		t.Fatal("caller mutation changed the admitted helper")
	}
	secondValue, err := NewHelperStateValue(NewHelperBoolean(false), 1)
	if err != nil {
		t.Fatal(err)
	}
	quality := testNHA017Quality(t, AvailabilityAvailable, FreshnessFresh, ProvenanceObserved)
	entity := testNHA017Entity(t)
	t0 := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	first, _, err := ApplyStateReport(entity, nil, testNHA017Report(t, quality, &firstValue, &t0, t0, 1))
	if err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(time.Second)
	second, transition, err := ApplyStateReport(entity, &first, testNHA017Report(t, quality, &secondValue, nil, t1, 2))
	if err != nil || transition.ValueChanged || transition.Updated {
		t.Fatalf("equal helper values became pointer-identity changes: %+v %v", transition, err)
	}
	_, _, times, _, _, err := second.Components()
	if err != nil || times.ObservedAt == nil || !times.ObservedAt.Equal(t0) || !times.LastChanged.Equal(t0) {
		t.Fatalf("equal helper lost transition clocks: %+v %v", times, err)
	}
	thirdValue, err := NewHelperStateValue(NewHelperBoolean(false), 2)
	if err != nil {
		t.Fatal(err)
	}
	t2 := t1.Add(time.Second)
	third, transition, err := ApplyStateReport(entity, &second, testNHA017Report(t, quality, &thirdValue, nil, t2, 3))
	if err != nil || transition.ValueChanged || !transition.Updated {
		t.Fatalf("SQL token changed business value or missed update: %+v %v", transition, err)
	}
	_, _, times, _, _, err = third.Components()
	if err != nil || !times.LastChanged.Equal(t0) || !times.LastUpdated.Equal(t2) || times.ObservedAt == nil || !times.ObservedAt.Equal(t0) {
		t.Fatalf("SQL token shifted business-change clock: %+v %v", times, err)
	}
	t3 := t2.Add(time.Second)
	stale := testNHA017Quality(t, AvailabilityAvailable, FreshnessStale, ProvenanceEstimated)
	qualityOnly, transition, err := ApplyStateReport(entity, &third, testNHA017Report(t, stale, &thirdValue, nil, t3, 4))
	if err != nil || transition.ValueChanged || !transition.Updated {
		t.Fatalf("quality-only helper report changed business value: %+v %v", transition, err)
	}
	_, _, times, _, _, err = qualityOnly.Components()
	if err != nil || !times.LastChanged.Equal(t0) || !times.LastUpdated.Equal(t3) || times.ObservedAt == nil || !times.ObservedAt.Equal(t0) {
		t.Fatalf("quality-only report lost capture clock: %+v %v", times, err)
	}
	fourthValue, err := NewHelperStateValue(NewHelperBoolean(true), 3)
	if err != nil {
		t.Fatal(err)
	}
	changed, transition, err := ApplyStateReport(entity, &third, testNHA017Report(t, quality, &fourthValue, nil, t3, 4))
	if err != nil || !transition.ValueChanged || !transition.Updated {
		t.Fatalf("changed helper business value was ignored: %+v %v", transition, err)
	}
	_, _, times, _, _, err = changed.Components()
	if err != nil || !times.LastChanged.Equal(t3) || times.ObservedAt != nil {
		t.Fatalf("changed helper reused previous capture: %+v %v", times, err)
	}
}
