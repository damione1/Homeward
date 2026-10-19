// Package solar computes the HA 2026.10/Astral 2.2 solar profile locally.
// Private settings never become observations, errors, or fixture diagnostics.
package solar

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"

	"github.com/damione1/Homeward/internal/calendars"
)

type ErrorCode uint8

const (
	ErrorUnspecified ErrorCode = iota
	InvalidSettings
	InvalidDate
	InvalidEvent
	InvalidContext
	CalculationFailed
	Canceled
	BudgetExceeded
)

type Error struct {
	code  ErrorCode
	cause error
}

func failure(code ErrorCode) *Error { return &Error{code: code} }
func (e *Error) Code() ErrorCode {
	if e == nil {
		return ErrorUnspecified
	}
	return e.code
}
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}
func (e *Error) GoString() string { return e.Error() }
func (e *Error) Error() string {
	switch e.Code() {
	case InvalidSettings:
		return "solar: invalid private settings"
	case InvalidDate:
		return "solar: date or instant outside the admitted range"
	case InvalidEvent:
		return "solar: invalid event kind"
	case InvalidContext:
		return "solar: missing context"
	case CalculationFailed:
		return "solar: numerical calculation failed"
	case Canceled:
		return "solar: calculation canceled"
	case BudgetExceeded:
		return "solar: calculation budget exceeded"
	case ErrorUnspecified:
		return "solar: invalid error"
	default:
		return "solar: invalid error"
	}
}

// Input is untrusted admission data. Pointer presence preserves required zero
// coordinates and an explicitly configured zero altitude.
type Input struct {
	Latitude        *float64
	Longitude       *float64
	ElevationMeters *float64
	Timezone        string
}

type Observer struct {
	latitude, longitude, elevation float64
	valid                          bool
}

func (o Observer) Format(s fmt.State, _ rune) { _, _ = s.Write([]byte("solar.Observer(<private>)")) }
func (o Observer) String() string             { return "solar.Observer(<private>)" }
func (o Observer) GoString() string           { return o.String() }

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func (o Observer) Components() (float64, float64, float64, error) {
	if !o.valid || !finite(o.latitude) || !finite(o.longitude) || !finite(o.elevation) || o.latitude < -90 || o.latitude > 90 || o.longitude < -180 || o.longitude > 180 {
		return 0, 0, 0, failure(InvalidSettings)
	}
	return o.latitude, o.longitude, o.elevation, nil
}

type Settings struct {
	observer Observer
	zone     calendars.Zone
}

func (s Settings) Format(out fmt.State, _ rune) {
	_, _ = out.Write([]byte("solar.Settings(<private>)"))
}
func (s Settings) String() string   { return "solar.Settings(<private>)" }
func (s Settings) GoString() string { return s.String() }

func ParseSettings(input Input) (Settings, error) {
	if input.Latitude == nil || input.Longitude == nil {
		return Settings{}, failure(InvalidSettings)
	}
	observer := Observer{latitude: *input.Latitude, longitude: *input.Longitude, valid: true}
	if input.ElevationMeters != nil {
		observer.elevation = *input.ElevationMeters
	}
	if _, _, _, err := observer.Components(); err != nil {
		return Settings{}, err
	}
	zone, err := calendars.ParseZone(input.Timezone)
	if err != nil {
		return Settings{}, failure(InvalidSettings)
	}
	return Settings{observer: observer, zone: zone}, nil
}

func (s Settings) Valid() bool {
	_, _, _, err := s.observer.Components()
	return err == nil && s.zone.Valid()
}
func (s Settings) Observer() (Observer, error) {
	if !s.Valid() {
		return Observer{}, failure(InvalidSettings)
	}
	return s.observer, nil
}
func (s Settings) Zone() (calendars.Zone, error) {
	if !s.Valid() {
		return calendars.Zone{}, failure(InvalidSettings)
	}
	return s.zone, nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return failure(InvalidContext)
	}
	v := reflect.ValueOf(ctx)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if v.IsNil() {
			return failure(InvalidContext)
		}
	}
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return &Error{code: BudgetExceeded, cause: err}
		}
		return &Error{code: Canceled, cause: err}
	}
	return nil
}
