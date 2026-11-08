package domain

import (
	"testing"
	"time"
)

func TestNHA212LightContentsPresenceAndRepeatedObservationClocks(t *testing.T) {
	brightness := 0.
	rgb := RGB{Red: 0, Green: 137, Blue: 255}
	preset := "scene:night/7"
	first, err := NewLightValue(LightValueInput{On: false, Brightness: &brightness, RGB: &rgb, Preset: &preset})
	if err != nil {
		t.Fatal(err)
	}
	parts, _ := first.Components()
	rebuilt, err := NewLightValue(parts)
	if err != nil || !first.Equal(rebuilt) {
		t.Fatal("new component pointers changed value", err)
	}
	*parts.Brightness = 50
	parts.RGB.Green = 42
	*parts.Preset = "other"
	rgb.Blue = 0
	preset = "changed"
	if !first.Equal(rebuilt) {
		t.Fatal("caller modified admitted value")
	}
	absent, _ := NewLightValue(LightValueInput{On: false})
	if first.Equal(absent) {
		t.Fatal("present zero/color/preset erased")
	}
	id := testNHA017Entity(t)
	quality := testNHA017Quality(t, AvailabilityAvailable, FreshnessFresh, ProvenanceObserved)
	a, _ := NewLightStateValue(first)
	b, _ := NewLightStateValue(rebuilt)
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	before, _, err := ApplyStateReport(id, nil, testNHA017Report(t, quality, &a, &at, at, 1))
	if err != nil {
		t.Fatal(err)
	}
	later := at.Add(time.Second)
	after, transition, err := ApplyStateReport(id, &before, testNHA017Report(t, quality, &b, &later, later, 2))
	if err != nil || transition.ValueChanged || transition.Updated {
		t.Fatal("identical report changed business clocks", transition, err)
	}
	_, _, times, _, _, _ := after.Components()
	if !times.LastChanged.Equal(at) || !times.LastUpdated.Equal(at) || !times.LastReported.Equal(later) {
		t.Fatal("incorrect stable-value timestamps", times)
	}
	changed := parts
	newValue, _ := NewLightValue(changed)
	if first.Equal(newValue) {
		t.Fatal("actual channel/preset change ignored")
	}
}
