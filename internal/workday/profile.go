// Package workday preserves the imported Home Assistant Workday profile.
// The retained exclusions are Saturday and Sunday only: a weekday holiday is
// still a workday. Holiday rules and provincial inference are not admitted.
package workday

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/damione1/Homeward/internal/calendars"
)

// ProfileVersion identifies the exact retained CA/Mon-Fri/Sat-Sun/offset-zero
// rules, independently of any future Workday profile.
const ProfileVersion = "ha-workday-ca-weekdays-v1"

type ErrorCode uint8

const (
	ErrorUnspecified ErrorCode = iota
	InvalidProfile
	InvalidDate
	InvalidWindow
	InvalidIdentity
	InvalidContext
	Canceled
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
func (e *Error) Error() string {
	if e == nil {
		return "workday: invalid error"
	}
	switch e.code {
	case InvalidProfile:
		return "workday: unsupported profile or timezone"
	case InvalidDate:
		return "workday: invalid civil date or instant"
	case InvalidWindow:
		return "workday: date window must contain 1 to 366 days"
	case InvalidIdentity:
		return "workday: invalid calculated calendar identity"
	case InvalidContext:
		return "workday: missing context"
	case Canceled:
		return "workday: canceled"
	default:
		return "workday: invalid error"
	}
}
func (e *Error) GoString() string { return e.Error() }

// Input is an admission boundary. No holiday, subdivision, offset, or custom
// weekday fields exist; YAML with those fields is rejected by config.
type Input struct{ Timezone string }

type profileKind uint8

const retainedCAWeekdays profileKind = 1 // CA, no subdivision; Mon-Fri; exclude Sat/Sun; offset 0.

type Settings struct {
	zone    calendars.Zone
	profile profileKind
}

func (Settings) Format(out fmt.State, _ rune) {
	_, _ = out.Write([]byte("workday.Settings(<private>)"))
}
func (s Settings) String() string   { return "workday.Settings(<private>)" }
func (s Settings) GoString() string { return s.String() }

func ParseSettings(input Input) (Settings, error) {
	zone, err := calendars.ParseZone(input.Timezone)
	if err != nil {
		return Settings{}, failure(InvalidProfile)
	}
	return Settings{zone: zone, profile: retainedCAWeekdays}, nil
}
func (s Settings) Valid() bool { return s.profile == retainedCAWeekdays && s.zone.Valid() }
func (s Settings) Zone() (calendars.Zone, error) {
	if !s.Valid() {
		return calendars.Zone{}, failure(InvalidProfile)
	}
	return s.zone, nil
}

// IsWorkday is the single predicate used by the binary sensor and calendar.
// The country is CA with absent subdivision; neither affects this result
// because the imported HA profile has no `holiday` include/exclude token.
func (s Settings) IsWorkday(date calendars.CivilDate) (bool, error) {
	if !s.Valid() {
		return false, failure(InvalidProfile)
	}
	y, m, d, err := date.Components()
	if err != nil {
		return false, failure(InvalidDate)
	}
	weekday := time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Weekday()
	return weekday != time.Saturday && weekday != time.Sunday, nil
}

func validContext(ctx context.Context) error {
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
		return &Error{code: Canceled, cause: err}
	}
	return nil
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
