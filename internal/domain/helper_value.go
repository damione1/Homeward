package domain

import (
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

var ErrInvalidHelperValue = errors.New("invalid helper value")

// HelperKind is the sole operational tag for a persisted helper value.
// Entity identity and select options are open data; value variants are closed.
type HelperKind uint8

const (
	HelperInvalid HelperKind = iota
	HelperBoolean
	HelperNumber
	HelperCounter
	HelperButtonPress
	HelperDate
	HelperTime
	HelperDateTime
	HelperSelect
	HelperText
)

func (k HelperKind) Valid() bool {
	return k >= HelperBoolean && k <= HelperText
}

type CivilDate struct {
	year, month, day int
	valid            bool
}

func NewCivilDate(year, month, day int) (CivilDate, error) {
	if year < 1 || year > 9999 || month < 1 || month > 12 || day < 1 || day > 31 {
		return CivilDate{}, ErrInvalidHelperValue
	}
	last := [...]int{0, 31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}[month]
	if month == 2 && (year%400 == 0 || (year%4 == 0 && year%100 != 0)) {
		last = 29
	}
	if day > last {
		return CivilDate{}, ErrInvalidHelperValue
	}
	return CivilDate{year, month, day, true}, nil
}

func (d CivilDate) Components() (int, int, int, error) {
	if !d.valid {
		return 0, 0, 0, ErrInvalidHelperValue
	}
	if _, err := NewCivilDate(d.year, d.month, d.day); err != nil {
		return 0, 0, 0, err
	}
	return d.year, d.month, d.day, nil
}

type CivilTime struct {
	hour, minute, second int
	valid                bool
}

func NewCivilTime(hour, minute, second int) (CivilTime, error) {
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 || second < 0 || second > 59 {
		return CivilTime{}, ErrInvalidHelperValue
	}
	return CivilTime{hour, minute, second, true}, nil
}

func (t CivilTime) Components() (int, int, int, error) {
	if !t.valid {
		return 0, 0, 0, ErrInvalidHelperValue
	}
	if _, err := NewCivilTime(t.hour, t.minute, t.second); err != nil {
		return 0, 0, 0, err
	}
	return t.hour, t.minute, t.second, nil
}

type CivilDateTime struct {
	date  CivilDate
	clock CivilTime
}

func NewCivilDateTime(date CivilDate, clock CivilTime) (CivilDateTime, error) {
	if _, _, _, err := date.Components(); err != nil {
		return CivilDateTime{}, err
	}
	if _, _, _, err := clock.Components(); err != nil {
		return CivilDateTime{}, err
	}
	return CivilDateTime{date, clock}, nil
}

func (v CivilDateTime) Components() (CivilDate, CivilTime, error) {
	if _, _, _, err := v.date.Components(); err != nil {
		return CivilDate{}, CivilTime{}, err
	}
	if _, _, _, err := v.clock.Components(); err != nil {
		return CivilDate{}, CivilTime{}, err
	}
	return v.date, v.clock, nil
}

// HelperValue is immutable and its Go zero is invalid. A present false, zero,
// or empty text is represented only by an explicit constructor.
type HelperValue struct {
	kind    HelperKind
	boolean bool
	number  float64
	counter int64
	text    string
	date    CivilDate
	clock   CivilTime
	instant time.Time
}

func NewHelperBoolean(value bool) HelperValue {
	return HelperValue{kind: HelperBoolean, boolean: value}
}
func NewHelperCounter(value int64) HelperValue {
	return HelperValue{kind: HelperCounter, counter: value}
}
func NewHelperNumber(value float64) (HelperValue, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return HelperValue{}, ErrInvalidHelperValue
	}
	return HelperValue{kind: HelperNumber, number: value}, nil
}
func NewHelperButtonPress(at time.Time) (HelperValue, error) {
	utc := at.Round(0).UTC().Truncate(time.Microsecond)
	if utc.IsZero() || utc.Year() < 1 || utc.Year() > 9999 {
		return HelperValue{}, ErrInvalidHelperValue
	}
	return HelperValue{kind: HelperButtonPress, instant: utc}, nil
}
func NewHelperDate(date CivilDate) (HelperValue, error) {
	if _, _, _, err := date.Components(); err != nil {
		return HelperValue{}, err
	}
	return HelperValue{kind: HelperDate, date: date}, nil
}
func NewHelperTime(clock CivilTime) (HelperValue, error) {
	if _, _, _, err := clock.Components(); err != nil {
		return HelperValue{}, err
	}
	return HelperValue{kind: HelperTime, clock: clock}, nil
}
func NewHelperDateTime(value CivilDateTime) (HelperValue, error) {
	date, clock, err := value.Components()
	if err != nil {
		return HelperValue{}, err
	}
	return HelperValue{kind: HelperDateTime, date: date, clock: clock}, nil
}
func NewHelperSelect(option string) (HelperValue, error) {
	if option == "" || !utf8.ValidString(option) || strings.ContainsRune(option, '\x00') || len(option) > 16384 {
		return HelperValue{}, ErrInvalidHelperValue
	}
	return HelperValue{kind: HelperSelect, text: option}, nil
}
func NewHelperText(value string) (HelperValue, error) {
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') || len(value) > 16384 {
		return HelperValue{}, ErrInvalidHelperValue
	}
	return HelperValue{kind: HelperText, text: value}, nil
}

func (v HelperValue) Kind() (HelperKind, error) {
	switch v.kind {
	case HelperBoolean, HelperCounter:
		return v.kind, nil
	case HelperNumber:
		if !math.IsNaN(v.number) && !math.IsInf(v.number, 0) {
			return v.kind, nil
		}
	case HelperButtonPress:
		if _, err := NewHelperButtonPress(v.instant); err == nil {
			return v.kind, nil
		}
	case HelperDate:
		if _, _, _, err := v.date.Components(); err == nil {
			return v.kind, nil
		}
	case HelperTime:
		if _, _, _, err := v.clock.Components(); err == nil {
			return v.kind, nil
		}
	case HelperDateTime:
		if _, err := NewCivilDateTime(v.date, v.clock); err == nil {
			return v.kind, nil
		}
	case HelperSelect:
		if _, err := NewHelperSelect(v.text); err == nil {
			return v.kind, nil
		}
	case HelperText:
		if _, err := NewHelperText(v.text); err == nil {
			return v.kind, nil
		}
	}
	return HelperInvalid, ErrInvalidHelperValue
}

func (v HelperValue) Valid() bool { _, err := v.Kind(); return err == nil }

func (v HelperValue) Boolean() (bool, bool) { return v.boolean, v.kind == HelperBoolean }
func (v HelperValue) Number() (float64, bool) {
	kind, err := v.Kind()
	return v.number, err == nil && kind == HelperNumber
}
func (v HelperValue) Counter() (int64, bool) { return v.counter, v.kind == HelperCounter }
func (v HelperValue) ButtonPress() (time.Time, bool) {
	kind, err := v.Kind()
	return v.instant, err == nil && kind == HelperButtonPress
}
func (v HelperValue) Date() (CivilDate, bool) {
	kind, err := v.Kind()
	return v.date, err == nil && kind == HelperDate
}
func (v HelperValue) Time() (CivilTime, bool) {
	kind, err := v.Kind()
	return v.clock, err == nil && kind == HelperTime
}
func (v HelperValue) DateTime() (CivilDateTime, bool) {
	kind, err := v.Kind()
	return CivilDateTime{v.date, v.clock}, err == nil && kind == HelperDateTime
}
func (v HelperValue) Select() (string, bool) {
	kind, err := v.Kind()
	return v.text, err == nil && kind == HelperSelect
}
func (v HelperValue) Text() (string, bool) {
	kind, err := v.Kind()
	return v.text, err == nil && kind == HelperText
}
