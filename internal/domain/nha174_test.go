package domain

import (
	"math"
	"testing"
	"time"
)

func nha174Pointer[T any](value T) *T { return &value }

func nha174Temperature(t *testing.T, value float64) CelsiusTemperature {
	t.Helper()
	temperature, err := NewCelsiusTemperature(value)
	if err != nil {
		t.Fatal(err)
	}
	return temperature
}

func TestNHA174PrivateClimateConstructionPreservesAbsenceAndCopies(t *testing.T) {
	if _, err := (CelsiusTemperature{}).Value(); err == nil {
		t.Fatal("Go zero admitted as a present Celsius reading")
	}
	for _, number := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -273.15001} {
		if _, err := NewCelsiusTemperature(number); err == nil {
			t.Fatalf("invalid Celsius value admitted: %v", number)
		}
	}
	for _, number := range []float64{-273.15, 0, 1e100} {
		if _, err := NewCelsiusTemperature(number); err != nil {
			t.Fatalf("physical value was given an invented device bound: %v", err)
		}
	}
	zero := nha174Temperature(t, 0)
	mode, activity, fan := ClimateModeOff, ClimateActivityIdle, false
	input := ClimateValueInput{CurrentTemperature: &zero, Mode: &mode, Activity: &activity, FanActive: &fan}
	climate, err := NewClimateValue(input)
	if err != nil {
		t.Fatal(err)
	}
	zero, mode, activity, fan = CelsiusTemperature{}, ClimateModeHeat, ClimateActivityHeating, true
	parts, err := climate.Components()
	if err != nil || parts.CurrentTemperature == nil || parts.Mode == nil || *parts.Mode != ClimateModeOff || parts.Activity == nil || *parts.Activity != ClimateActivityIdle || parts.FanActive == nil || *parts.FanActive || parts.Setpoint != nil {
		t.Fatalf("presence or caller copy lost: %+v %v", parts, err)
	}
	if number, err := parts.CurrentTemperature.Value(); err != nil || number != 0 {
		t.Fatalf("present zero lost: %v %v", number, err)
	}
	*parts.CurrentTemperature = CelsiusTemperature{}
	*parts.Mode, *parts.Activity, *parts.FanActive = ClimateModeHeat, ClimateActivityHeating, true
	again, err := climate.Components()
	if err != nil || *again.Mode != ClimateModeOff || *again.Activity != ClimateActivityIdle || *again.FanActive {
		t.Fatal("returned fields mutate the admitted value")
	}
	value, err := NewClimateStateValue(climate)
	if err != nil {
		t.Fatal(err)
	}
	if kind, err := value.Kind(); err != nil || kind != ValueClimate {
		t.Fatalf("climate not in closed state sum: %v %v", kind, err)
	}
	if extracted, ok := value.Climate(); !ok || extracted != climate {
		t.Fatal("climate state copy changed")
	}
	if _, err := NewClimateStateValue(ClimateValue{}); err == nil {
		t.Fatal("zero climate admitted")
	}
	if _, ok := (StateValue{}).Climate(); ok {
		t.Fatal("zero state is climate")
	}
}

func TestNHA174ClimateRejectsZeroEnumsAndMalformedSetpoints(t *testing.T) {
	for _, input := range []ClimateValueInput{
		{}, {CurrentTemperature: &CelsiusTemperature{}}, {Setpoint: &ClimateSetpoint{}},
		{Mode: nha174Pointer(ClimateModeUnspecified)}, {Mode: nha174Pointer(ClimateMode(255))},
		{Activity: nha174Pointer(ClimateActivityUnspecified)}, {Activity: nha174Pointer(ClimateActivity(255))},
	} {
		if _, err := NewClimateValue(input); err == nil {
			t.Fatalf("invalid construction admitted: %+v", input)
		}
	}
	low, high := nha174Temperature(t, 18), nha174Temperature(t, 23)
	if _, err := NewSingleClimateSetpoint(CelsiusTemperature{}); err == nil {
		t.Fatal("zero single temperature admitted")
	}
	if _, err := NewRangeClimateSetpoint(high, low); err == nil {
		t.Fatal("inverted range admitted")
	}
	if _, err := NewRangeClimateSetpoint(low, CelsiusTemperature{}); err == nil {
		t.Fatal("absent high admitted")
	}
	setpoint, err := NewRangeClimateSetpoint(low, high)
	if err != nil {
		t.Fatal(err)
	}
	climate, err := NewClimateValue(ClimateValueInput{Setpoint: &setpoint, Mode: nha174Pointer(ClimateModeHeat)})
	if err != nil {
		t.Fatal(err)
	}
	parts, err := climate.Components()
	if err != nil || parts.CurrentTemperature != nil || parts.Activity != nil || parts.FanActive != nil {
		t.Fatal("requested mode or setpoint manufactured an observation")
	}
	if lower, upper, ok := parts.Setpoint.Range(); !ok || lower != low || upper != high {
		t.Fatal("range value not retained")
	}
	if _, ok := parts.Setpoint.Single(); ok {
		t.Fatal("range also exposed as a single setpoint")
	}
}

func TestNHA174ClimateStateTransitionsCompareEveryPresence(t *testing.T) {
	id, err := ParseEntityID("Host:nha174")
	if err != nil {
		t.Fatal(err)
	}
	quality, err := NewQuality(AvailabilityAvailable, FreshnessFresh, ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	source, err := NewSourceStamp("nha174-source", nil)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	var previous *ObservedState
	apply := func(q Quality, input *ClimateValueInput, revision uint64) (ObservedState, StateTransition) {
		t.Helper()
		var value *StateValue
		if input != nil {
			climate, err := NewClimateValue(*input)
			if err != nil {
				t.Fatal(err)
			}
			stateValue, err := NewClimateStateValue(climate)
			if err != nil {
				t.Fatal(err)
			}
			value = &stateValue
		}
		report, err := NewStateReport(q, value, nil, at.Add(time.Duration(revision)*time.Second), source, revision)
		if err != nil {
			t.Fatal(err)
		}
		next, transition, err := ApplyStateReport(id, previous, report)
		if err != nil {
			t.Fatal(err)
		}
		previous = &next
		return next, transition
	}
	input := ClimateValueInput{CurrentTemperature: nha174Pointer(nha174Temperature(t, 0)), Mode: nha174Pointer(ClimateModeHeat)}
	first, _ := apply(quality, &input, 1)
	_, _, firstTimes, _, _, _ := first.Components()
	equal, transition := apply(quality, &input, 2)
	_, _, equalTimes, _, _, _ := equal.Components()
	if transition.ValueChanged || transition.Updated || equalTimes.LastChanged != firstTimes.LastChanged || equalTimes.LastReported != at.Add(2*time.Second) {
		t.Fatal("equal climate moved a transition clock")
	}
	input.FanActive = nha174Pointer(false)
	changed, transition := apply(quality, &input, 3)
	_, _, changedTimes, _, _, _ := changed.Components()
	if !transition.ValueChanged || changedTimes.LastChanged != at.Add(3*time.Second) {
		t.Fatal("absent fan to observed off did not change the value")
	}
	stale, err := NewQuality(AvailabilityAvailable, FreshnessStale, ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	qualityOnly, transition := apply(stale, &input, 4)
	_, _, staleTimes, _, _, _ := qualityOnly.Components()
	if transition.ValueChanged || !transition.Updated || staleTimes.LastChanged != changedTimes.LastChanged || staleTimes.LastUpdated != at.Add(4*time.Second) {
		t.Fatal("quality-only transition changed climate value time")
	}
	for index, availability := range []Availability{AvailabilityUnknown, AvailabilityUnavailable} {
		q, err := NewQuality(availability, FreshnessStale, ProvenanceObserved)
		if err != nil {
			t.Fatal(err)
		}
		unavailable, _ := apply(q, nil, uint64(index+5))
		if _, present := unavailable.CurrentValue(); present {
			t.Fatal("quality report invented current climate")
		}
		if _, present := unavailable.CriticalValue(); present {
			t.Fatal("unknown/unavailable climate became usable")
		}
		if display, present := unavailable.DisplayValue(); !present {
			t.Fatal("last-known display lost")
		} else if climate, ok := display.Climate(); !ok {
			t.Fatal("last-known climate lost its variant")
		} else if parts, err := climate.Components(); err != nil || parts.FanActive == nil || *parts.FanActive {
			t.Fatal("last-known climate lost observed false")
		}
	}
}
