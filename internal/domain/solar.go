package domain

import (
	"errors"
	"math"
	"time"
)

var ErrInvalidSolarValue = errors.New("invalid solar value")

// SolarEventKind is the operational authority for the six solar events.
// Wire representations must be admitted explicitly by the contracts boundary.
type SolarEventKind uint8

const (
	SolarEventUnspecified SolarEventKind = iota
	SolarDawn
	SolarDusk
	SolarMidnight
	SolarNoon
	SolarRising
	SolarSetting
)

const SolarEventCount = 6

func SolarEventKinds() [SolarEventCount]SolarEventKind {
	return [SolarEventCount]SolarEventKind{SolarDawn, SolarDusk, SolarMidnight, SolarNoon, SolarRising, SolarSetting}
}

func (k SolarEventKind) Index() (int, error) {
	switch k {
	case SolarDawn:
		return 0, nil
	case SolarDusk:
		return 1, nil
	case SolarMidnight:
		return 2, nil
	case SolarNoon:
		return 3, nil
	case SolarRising:
		return 4, nil
	case SolarSetting:
		return 5, nil
	case SolarEventUnspecified:
		return 0, ErrInvalidSolarValue
	default:
		return 0, ErrInvalidSolarValue
	}
}

func (k SolarEventKind) Name() (string, error) {
	switch k {
	case SolarDawn:
		return "dawn", nil
	case SolarDusk:
		return "dusk", nil
	case SolarMidnight:
		return "midnight", nil
	case SolarNoon:
		return "noon", nil
	case SolarRising:
		return "rising", nil
	case SolarSetting:
		return "setting", nil
	case SolarEventUnspecified:
		return "", ErrInvalidSolarValue
	default:
		return "", ErrInvalidSolarValue
	}
}

func ParseSolarEventKind(raw string) (SolarEventKind, error) {
	for _, kind := range SolarEventKinds() {
		name, _ := kind.Name()
		if raw == name {
			return kind, nil
		}
	}
	return SolarEventUnspecified, ErrInvalidSolarValue
}

type SolarAbsenceReason uint8

const (
	SolarAbsenceUnspecified SolarAbsenceReason = iota
	SolarNoTransitOnDate
	SolarNoEventWithinSearchHorizon
)

func (r SolarAbsenceReason) Valid() bool {
	switch r {
	case SolarNoTransitOnDate, SolarNoEventWithinSearchHorizon:
		return true
	case SolarAbsenceUnspecified:
		return false
	default:
		return false
	}
}

type SolarOccurrenceKind uint8

const (
	SolarOccurrenceUnspecified SolarOccurrenceKind = iota
	SolarOccurrencePresent
	SolarOccurrenceAbsent
)

// SolarOccurrence is a comparable sum. Absent does not contain a timestamp;
// zero is invalid, and an ordinary zero-valued angle is never an absence.
type SolarOccurrence struct {
	kind    SolarOccurrenceKind
	instant time.Time
	absence SolarAbsenceReason
}

func SolarOccurrenceAt(instant time.Time) (SolarOccurrence, error) {
	instant = instant.UTC()
	if instant.IsZero() || instant.Year() < 1 || instant.Year() > 9999 {
		return SolarOccurrence{}, ErrInvalidSolarValue
	}
	return SolarOccurrence{kind: SolarOccurrencePresent, instant: instant}, nil
}

func SolarOccurrenceMissing(reason SolarAbsenceReason) (SolarOccurrence, error) {
	if !reason.Valid() {
		return SolarOccurrence{}, ErrInvalidSolarValue
	}
	return SolarOccurrence{kind: SolarOccurrenceAbsent, absence: reason}, nil
}

func (o SolarOccurrence) Components() (time.Time, SolarAbsenceReason, SolarOccurrenceKind, error) {
	switch o.kind {
	case SolarOccurrencePresent:
		if o.absence == SolarAbsenceUnspecified && !o.instant.IsZero() && o.instant.Year() >= 1 && o.instant.Year() <= 9999 && o.instant.Location() == time.UTC {
			return o.instant, SolarAbsenceUnspecified, SolarOccurrencePresent, nil
		}
	case SolarOccurrenceAbsent:
		if o.instant.IsZero() && o.absence.Valid() {
			return time.Time{}, o.absence, SolarOccurrenceAbsent, nil
		}
	case SolarOccurrenceUnspecified:
	default:
	}
	return time.Time{}, SolarAbsenceUnspecified, SolarOccurrenceUnspecified, ErrInvalidSolarValue
}

type SolarHorizon uint8

const (
	SolarHorizonUnspecified SolarHorizon = iota
	SolarAboveHorizon
	SolarBelowHorizon
)

// SolarValueInput carries only computed data; it cannot contain an observer.
type SolarValueInput struct {
	Horizon          SolarHorizon
	ElevationDegrees float64
	AzimuthDegrees   float64
	Rising           bool
	Next             [SolarEventCount]SolarOccurrence
}

type SolarValue struct {
	valid bool
	input SolarValueInput
}

func NewSolarValue(input SolarValueInput) (SolarValue, error) {
	if math.IsNaN(input.ElevationDegrees) || math.IsInf(input.ElevationDegrees, 0) || input.ElevationDegrees < -90 || input.ElevationDegrees > 90 ||
		math.IsNaN(input.AzimuthDegrees) || math.IsInf(input.AzimuthDegrees, 0) || input.AzimuthDegrees < 0 || input.AzimuthDegrees > 360 {
		return SolarValue{}, ErrInvalidSolarValue
	}
	switch input.Horizon {
	case SolarAboveHorizon:
		if input.ElevationDegrees <= -0.833 {
			return SolarValue{}, ErrInvalidSolarValue
		}
	case SolarBelowHorizon:
		if input.ElevationDegrees > -0.833 {
			return SolarValue{}, ErrInvalidSolarValue
		}
	case SolarHorizonUnspecified:
		return SolarValue{}, ErrInvalidSolarValue
	default:
		return SolarValue{}, ErrInvalidSolarValue
	}
	for _, event := range input.Next {
		_, reason, kind, err := event.Components()
		if err != nil || (kind == SolarOccurrenceAbsent && reason != SolarNoEventWithinSearchHorizon) {
			return SolarValue{}, ErrInvalidSolarValue
		}
	}
	noonIndex, _ := SolarNoon.Index()
	midnightIndex, _ := SolarMidnight.Index()
	noon, _, noonKind, _ := input.Next[noonIndex].Components()
	midnight, _, midnightKind, _ := input.Next[midnightIndex].Components()
	if noonKind != SolarOccurrencePresent || midnightKind != SolarOccurrencePresent || input.Rising != noon.Before(midnight) {
		return SolarValue{}, ErrInvalidSolarValue
	}
	return SolarValue{valid: true, input: input}, nil
}

func (v SolarValue) Components() (SolarValueInput, error) {
	if !v.valid {
		return SolarValueInput{}, ErrInvalidSolarValue
	}
	if _, err := NewSolarValue(v.input); err != nil {
		return SolarValueInput{}, err
	}
	return v.input, nil
}

// SolarEventValue specializes a timestamp or absence as an astronomical event.
// Civil helper dates and times are not admitted by this type.
type SolarEventValue struct {
	event      SolarEventKind
	occurrence SolarOccurrence
}

func NewSolarEventValue(event SolarEventKind, occurrence SolarOccurrence) (SolarEventValue, error) {
	if _, err := event.Index(); err != nil {
		return SolarEventValue{}, err
	}
	if _, _, _, err := occurrence.Components(); err != nil {
		return SolarEventValue{}, err
	}
	_, reason, kind, _ := occurrence.Components()
	if kind == SolarOccurrenceAbsent && reason != SolarNoEventWithinSearchHorizon {
		return SolarEventValue{}, ErrInvalidSolarValue
	}
	return SolarEventValue{event, occurrence}, nil
}

func (v SolarEventValue) Components() (SolarEventKind, SolarOccurrence, error) {
	if _, err := NewSolarEventValue(v.event, v.occurrence); err != nil {
		return SolarEventUnspecified, SolarOccurrence{}, err
	}
	return v.event, v.occurrence, nil
}
