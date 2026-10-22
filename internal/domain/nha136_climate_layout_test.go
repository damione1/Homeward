package domain

import (
	"math"
	"testing"
	"time"
)

func TestNHA136CompactClimateSetpointAdmission(t *testing.T) {
	zero, err := NewCelsiusTemperature(0)
	if err != nil {
		t.Fatal(err)
	}
	single, err := NewSingleClimateSetpoint(zero)
	if err != nil {
		t.Fatal(err)
	}
	ranged, err := NewRangeClimateSetpoint(zero, zero)
	if err != nil {
		t.Fatal(err)
	}
	if single == (ClimateSetpoint{}) || ranged == single {
		t.Fatal("zero temperature lost its presence or setpoint variant")
	}
	if value, ok := single.Single(); !ok || value != zero {
		t.Fatal("single zero was not reconstructed as an admitted temperature")
	}
	if low, high, ok := ranged.Range(); !ok || low != zero || high != zero {
		t.Fatal("range endpoints at zero were not reconstructed as admitted temperatures")
	}
	if _, _, ok := single.Range(); ok {
		t.Fatal("single exposed a range")
	}
	if _, ok := ranged.Single(); ok {
		t.Fatal("range exposed a single")
	}
	again, err := NewSingleClimateSetpoint(zero)
	if err != nil || again != single {
		t.Fatal("independent constructors changed comparable setpoint equality")
	}
	for _, invalid := range []CelsiusTemperature{
		{}, {value: 1}, {value: math.NaN(), valid: true},
		{value: math.Inf(1), valid: true}, {value: math.Inf(-1), valid: true},
		{value: -273.16, valid: true},
	} {
		if _, err := NewSingleClimateSetpoint(invalid); err == nil {
			t.Fatal("single constructor admitted an invalid source temperature")
		}
		if _, err := NewRangeClimateSetpoint(invalid, zero); err == nil {
			t.Fatal("range constructor admitted an invalid low temperature")
		}
		if _, err := NewRangeClimateSetpoint(zero, invalid); err == nil {
			t.Fatal("range constructor admitted an invalid high temperature")
		}
	}
	for _, test := range []struct {
		name  string
		value ClimateSetpoint
	}{
		{"go-zero", ClimateSetpoint{}},
		{"missing-kind", ClimateSetpoint{low: 1}},
		{"unknown-kind", ClimateSetpoint{kind: ClimateSetpointKind(255)}},
		{"single-nan", ClimateSetpoint{kind: ClimateSetpointSingle, low: math.NaN()}},
		{"single-positive-infinity", ClimateSetpoint{kind: ClimateSetpointSingle, low: math.Inf(1)}},
		{"single-negative-infinity", ClimateSetpoint{kind: ClimateSetpointSingle, low: math.Inf(-1)}},
		{"single-below-absolute-zero", ClimateSetpoint{kind: ClimateSetpointSingle, low: -273.16}},
		{"single-orphan-high", ClimateSetpoint{kind: ClimateSetpointSingle, high: 1}},
		{"single-nan-high", ClimateSetpoint{kind: ClimateSetpointSingle, high: math.NaN()}},
		{"single-infinite-high", ClimateSetpoint{kind: ClimateSetpointSingle, high: math.Inf(1)}},
		{"range-nan-low", ClimateSetpoint{kind: ClimateSetpointRange, low: math.NaN()}},
		{"range-nan-high", ClimateSetpoint{kind: ClimateSetpointRange, high: math.NaN()}},
		{"range-infinite-high", ClimateSetpoint{kind: ClimateSetpointRange, high: math.Inf(1)}},
		{"range-negative-infinite-high", ClimateSetpoint{kind: ClimateSetpointRange, high: math.Inf(-1)}},
		{"range-below-absolute-zero", ClimateSetpoint{kind: ClimateSetpointRange, low: -273.16}},
		{"range-inverted", ClimateSetpoint{kind: ClimateSetpointRange, low: 1, high: 0}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if kind, err := test.value.Kind(); err == nil || kind != ClimateSetpointUnspecified {
				t.Fatal("malformed compact setpoint admitted")
			}
			if value, ok := test.value.Single(); ok || value != (CelsiusTemperature{}) {
				t.Fatal("malformed compact single exposed a temperature")
			}
			if low, high, ok := test.value.Range(); ok || low != (CelsiusTemperature{}) || high != (CelsiusTemperature{}) {
				t.Fatal("malformed compact range exposed temperatures")
			}
			if _, err := NewClimateValue(ClimateValueInput{Setpoint: &test.value}); err == nil {
				t.Fatal("malformed setpoint crossed climate admission")
			}
			if _, err := NewClimateStateValue(ClimateValue{setpoint: test.value, valid: true}); err == nil {
				t.Fatal("forged climate crossed state admission")
			}
		})
	}
}

func TestNHA136ClimateRejectsForgedHumidityAndOrphanPresence(t *testing.T) {
	for _, test := range []struct {
		name  string
		value RelativeHumidity
	}{
		{"go-zero", RelativeHumidity{}},
		{"orphan-number", RelativeHumidity{number: 1}},
		{"nan", RelativeHumidity{number: math.NaN(), valid: true}},
		{"positive-infinity", RelativeHumidity{number: math.Inf(1), valid: true}},
		{"negative-infinity", RelativeHumidity{number: math.Inf(-1), valid: true}},
		{"negative", RelativeHumidity{number: -1, valid: true}},
		{"above-hundred", RelativeHumidity{number: 101, valid: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.value.Value(); err == nil {
				t.Fatal("forged humidity exposed an admitted number")
			}
			if _, err := NewClimateValue(ClimateValueInput{Humidity: &test.value}); err == nil {
				t.Fatal("forged humidity crossed climate admission")
			}
			if _, err := NewClimateStateValue(ClimateValue{humidity: test.value, valid: true}); err == nil {
				t.Fatal("forged humidity crossed state admission")
			}
		})
	}
	for _, value := range []ClimateValue{
		{mode: ClimateModeHeat, humidity: RelativeHumidity{number: 1}, valid: true},
		{mode: ClimateModeHeat, fanTarget: ClimateFanTarget{automatic: true}, valid: true},
		{mode: ClimateModeHeat, fanActive: true, valid: true},
	} {
		if _, err := value.Components(); err == nil {
			t.Fatal("orphan payload without its presence flag escaped climate")
		}
		if _, err := NewClimateStateValue(value); err == nil {
			t.Fatal("orphan payload crossed state admission")
		}
	}
}

func TestNHA136ClimateCopiesEqualityAndZeroPresenceClocks(t *testing.T) {
	zero, err := NewCelsiusTemperature(0)
	if err != nil {
		t.Fatal(err)
	}
	single, err := NewSingleClimateSetpoint(zero)
	if err != nil {
		t.Fatal(err)
	}
	humidity, err := NewRelativeHumidity(0)
	if err != nil {
		t.Fatal(err)
	}
	fan := NewClimateFanTarget(true)
	mode := ClimateModeHeat
	input := ClimateValueInput{Setpoint: &single, Mode: &mode, Humidity: &humidity, FanTarget: &fan}
	climate, err := NewClimateValue(input)
	if err != nil {
		t.Fatal(err)
	}
	single, humidity, fan, mode = ClimateSetpoint{}, RelativeHumidity{}, ClimateFanTarget{}, ClimateModeOff
	parts, err := climate.Components()
	if err != nil || parts.Setpoint == nil || parts.Mode == nil || *parts.Mode != ClimateModeHeat || parts.Humidity == nil || parts.FanTarget == nil {
		t.Fatalf("caller mutation changed admitted fields: %+v %v", parts, err)
	}
	if number, err := parts.Humidity.Value(); err != nil || number != 0 {
		t.Fatal("present zero humidity disappeared")
	}
	if automatic, err := parts.FanTarget.Automatic(); err != nil || !automatic {
		t.Fatal("automatic fan target disappeared")
	}
	rebuilt, err := NewClimateValue(parts)
	if err != nil || rebuilt != climate {
		t.Fatal("fresh component pointers changed climate equality")
	}
	*parts.Setpoint, *parts.Humidity, *parts.FanTarget, *parts.Mode = ClimateSetpoint{}, RelativeHumidity{}, ClimateFanTarget{}, ClimateModeOff
	again, err := climate.Components()
	if err != nil || again.Setpoint == nil || again.Humidity == nil || again.FanTarget == nil || again.Mode == nil || *again.Mode != ClimateModeHeat {
		t.Fatal("returned pointers mutated the stored climate")
	}
	state, err := NewClimateStateValue(climate)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewClimateStateValue(rebuilt)
	if err != nil || state != second || !sameBusinessValue(state, second) {
		t.Fatal("state business equality depended on component pointer identity")
	}
	entity, err := ParseEntityID("nha136-climate-layout")
	if err != nil {
		t.Fatal(err)
	}
	quality, err := NewQuality(AvailabilityAvailable, FreshnessFresh, ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	source, err := NewSourceStamp("nha136-climate-layout", nil)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	var previous *ObservedState
	apply := func(value ClimateValueInput, revision uint64, changed bool) {
		t.Helper()
		climate, err := NewClimateValue(value)
		if err != nil {
			t.Fatal(err)
		}
		state, err := NewClimateStateValue(climate)
		if err != nil {
			t.Fatal(err)
		}
		now := at.Add(time.Duration(revision) * time.Second)
		report, err := NewStateReport(quality, &state, nil, now, source, revision)
		if err != nil {
			t.Fatal(err)
		}
		next, transition, err := ApplyStateReport(entity, previous, report)
		if err != nil || transition.ValueChanged != changed {
			t.Fatalf("revision %d: changed=%v wanted %v: %v", revision, transition.ValueChanged, changed, err)
		}
		_, _, times, _, _, _ := next.Components()
		if changed && times.LastChanged != now {
			t.Fatal("presence change did not advance last_changed")
		}
		if !changed && previous != nil {
			_, _, oldTimes, _, _, _ := previous.Components()
			if times.LastChanged != oldTimes.LastChanged || transition.Updated {
				t.Fatal("equal climate changed transition clocks")
			}
		}
		previous = &next
	}
	input = ClimateValueInput{Setpoint: again.Setpoint, Mode: again.Mode}
	apply(input, 1, true)
	independent, err := NewSingleClimateSetpoint(zero)
	if err != nil {
		t.Fatal(err)
	}
	input.Setpoint = &independent
	apply(input, 2, false)
	input.Humidity = again.Humidity
	apply(input, 3, true)
	input.FanTarget = again.FanTarget
	apply(input, 4, true)
	apply(input, 5, false)
	manual := NewClimateFanTarget(false)
	input.FanTarget = &manual
	apply(input, 6, true)
	input.Humidity = nil
	apply(input, 7, true)
	ranged, err := NewRangeClimateSetpoint(zero, zero)
	if err != nil {
		t.Fatal(err)
	}
	input.Setpoint = &ranged
	apply(input, 8, true)
}
