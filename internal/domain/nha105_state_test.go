package domain

import (
	"testing"
	"time"
	"unsafe"
)

type nha105FuturePayload struct{}

func (*nha105FuturePayload) nativeStateMarker() {}

func nha105StateSolar(t *testing.T) SolarValue {
	t.Helper()
	input := SolarValueInput{Horizon: SolarAboveHorizon, Rising: true}
	for i := range input.Next {
		input.Next[i], _ = SolarOccurrenceAt(time.Unix(int64(i+1)*100, 0))
	}
	input.Next[2], _ = SolarOccurrenceAt(time.Unix(1000, 0))
	value, err := NewSolarValue(input)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestNHA105NativePayloadCopiesEqualityAndAdmission(t *testing.T) {
	solar := nha105StateSolar(t)
	state, err := NewSolarStateValue(solar)
	if err != nil {
		t.Fatal(err)
	}
	solar.input.ElevationDegrees = 17
	copy, ok := state.Solar()
	if !ok {
		t.Fatal("solar copy absent")
	}
	copy.input.ElevationDegrees = 23
	again, _ := state.Solar()
	if again.input.ElevationDegrees != 0 {
		t.Fatal("mutable solar alias escaped")
	}
	second, _ := NewSolarStateValue(again)
	if state.native == second.native || !sameBusinessValue(state, second) {
		t.Fatal("business equality depended on wrapper identity")
	}
	for _, invalid := range []StateValue{
		{}, {kind: ValueSolar}, {kind: ValueSolar, native: (*solarStatePayload)(nil)},
		{kind: ValueSolar, native: &solarStatePayload{}}, {kind: ValueSolar, native: &helperStatePayload{value: NewHelperBoolean(false), revision: 1}},
		{kind: ValueSolar, native: &nha105FuturePayload{}}, {kind: ValueSolarEvent, native: (*solarEventStatePayload)(nil)},
		{kind: ValueHelper, native: (*helperStatePayload)(nil)}, {kind: ValueSwitch, native: &solarStatePayload{value: again}},
		{kind: ValueKind(99), native: &solarStatePayload{value: again}},
	} {
		if _, err := invalid.Kind(); err == nil {
			t.Fatal("invalid native payload admitted")
		}
		if _, ok := invalid.Solar(); ok {
			t.Fatal("invalid solar getter admitted")
		}
		if _, ok := invalid.SolarEvent(); ok {
			t.Fatal("invalid event getter admitted")
		}
	}
	firstHelper, _ := NewHelperStateValue(NewHelperBoolean(false), 1)
	nextHelper, _ := NewHelperStateValue(NewHelperBoolean(false), 2)
	if !sameBusinessValue(firstHelper, nextHelper) || !helperRevisionChanged(firstHelper, nextHelper) {
		t.Fatal("helper revision changed business equality")
	}
	if unsafe.Sizeof(uintptr(0)) == 8 && (unsafe.Sizeof(StateValue{}) != 208 || unsafe.Sizeof(ObservedState{}) != 600 || unsafe.Sizeof(helperStatePayload{}) != 136) {
		t.Fatal("production layout changed from ratified B")
	}
	entity, _ := ParseEntityID("nha105-native")
	quality, _ := NewQuality(AvailabilityAvailable, FreshnessFresh, ProvenanceObserved)
	source, _ := NewSourceStamp("nha105", nil)
	at := time.Unix(2000, 0).UTC()
	report, _ := NewStateReport(quality, &state, &at, at, source, 1)
	old, _, err := ApplyStateReport(entity, nil, report)
	if err != nil {
		t.Fatal(err)
	}
	report, _ = NewStateReport(quality, &second, nil, at.Add(time.Second), source, 2)
	current, transition, err := ApplyStateReport(entity, &old, report)
	if err != nil || transition.ValueChanged {
		t.Fatal("equal solar values changed last_changed")
	}
	_, _, oldTimes, _, _, _ := old.Components()
	_, _, newTimes, _, _, _ := current.Components()
	if oldTimes.LastChanged != newTimes.LastChanged || newTimes.ObservedAt == nil || *newTimes.ObservedAt != at {
		t.Fatal("equal report lost capture semantics")
	}
}
