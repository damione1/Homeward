package domain

import (
	"math"
	"testing"
)

func TestNHF009CoverPresenceCopiesAndBusinessEquality(t *testing.T) {
	state, position, obstruction := CoverStateClosed, 0.0, false
	cover, err := NewCoverValue(&state, &position, &obstruction)
	if err != nil {
		t.Fatal(err)
	}
	state, position, obstruction = CoverStateOpen, 80, true
	s, p, o, err := cover.Components()
	if err != nil || s == nil || *s != CoverStateClosed || p == nil || *p != 0 || o == nil || *o {
		t.Fatal("caller mutation or absence conflated with zero/false")
	}
	*s, *p, *o = CoverStateOpening, 15, true
	s, p, o, _ = cover.Components()
	if *s != CoverStateClosed || *p != 0 || *o {
		t.Fatal("getter exposes mutable payload")
	}
	first, _ := NewCoverStateValue(cover)
	second, _ := NewCoverStateValue(cover)
	if !sameBusinessValue(first, second) {
		t.Fatal("equal covers depend on pointer identity")
	}
	positionOnly, _ := NewCoverValue(nil, p, nil)
	partial, _ := NewCoverStateValue(positionOnly)
	if sameBusinessValue(first, partial) {
		t.Fatal("absent state/obstruction treated as observed")
	}
	if s, _, o, err := positionOnly.Components(); err != nil || s != nil || o != nil {
		t.Fatal("missing cover observations inferred")
	}
	if got, ok := first.Cover(); !ok || got != cover {
		t.Fatal("cover sum extraction differs")
	}
}

func TestNHF009ControlsRejectInvalidPrivatePayloads(t *testing.T) {
	for _, position := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -0.1, 100.1} {
		if _, err := NewCoverValue(nil, &position, nil); err == nil {
			t.Fatalf("invalid position admitted: %v", position)
		}
	}
	for _, state := range []CoverState{CoverStateUnspecified, 255} {
		if _, err := NewCoverValue(&state, nil, nil); err == nil {
			t.Fatal("invalid cover enum admitted")
		}
	}
	if _, err := NewCoverValue(nil, nil, nil); err == nil {
		t.Fatal("empty cover admitted")
	}
	for _, value := range []StateValue{
		{}, {kind: ValueCover}, {kind: ValueCover, native: (*coverStatePayload)(nil)},
		{kind: ValueCover, native: &coverStatePayload{}}, {kind: ValueLock, native: &lockStatePayload{}},
		{kind: ValueAlarm, native: &alarmStatePayload{}}, {kind: ValueCover, native: &lockStatePayload{LockStateLocked}},
	} {
		if _, err := value.Kind(); err == nil {
			t.Fatal("invalid payload admitted")
		}
		if _, ok := value.Cover(); ok {
			t.Fatal("invalid payload exposed as cover")
		}
		if _, ok := value.Lock(); ok {
			t.Fatal("invalid payload exposed as lock")
		}
		if _, ok := value.Alarm(); ok {
			t.Fatal("invalid payload exposed as alarm")
		}
	}
	for _, state := range []LockState{LockStateUnspecified, 255} {
		if _, err := NewLockStateValue(state); err == nil {
			t.Fatal("invalid lock admitted")
		}
	}
	for _, state := range []AlarmState{AlarmStateUnspecified, 255} {
		if _, err := NewAlarmStateValue(state); err == nil {
			t.Fatal("invalid alarm admitted")
		}
	}
	for first := LockStateLocked; first <= LockStateOpen; first++ {
		a, err := NewLockStateValue(first)
		if err != nil {
			t.Fatal(err)
		}
		for second := LockStateLocked; second <= LockStateOpen; second++ {
			b, _ := NewLockStateValue(second)
			if sameBusinessValue(a, b) != (first == second) {
				t.Fatal("distinct lock observations conflated")
			}
		}
	}
	for first := AlarmStateDisarmed; first <= AlarmStateTriggered; first++ {
		a, err := NewAlarmStateValue(first)
		if err != nil {
			t.Fatal(err)
		}
		for second := AlarmStateDisarmed; second <= AlarmStateTriggered; second++ {
			b, _ := NewAlarmStateValue(second)
			if sameBusinessValue(a, b) != (first == second) {
				t.Fatal("distinct alarm observations conflated")
			}
		}
	}
}
