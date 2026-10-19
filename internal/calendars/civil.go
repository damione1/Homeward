package calendars

import (
	"fmt"
	"strconv"
	"time"
)

// CivilDate is a calendar date, never a midnight instant in an inferred zone.
type CivilDate struct {
	year       uint16
	month, day uint8
}

func leap(year int) bool { return year%4 == 0 && (year%100 != 0 || year%400 == 0) }
func monthDays(year, month int) int {
	switch month {
	case 2:
		if leap(year) {
			return 29
		}
		return 28
	case 4, 6, 9, 11:
		return 30
	case 1, 3, 5, 7, 8, 10, 12:
		return 31
	default:
		return 0
	}
}
func (d CivilDate) Valid() bool {
	return d.year >= 1 && d.year <= 9999 && d.month >= 1 && d.month <= 12 && d.day >= 1 && int(d.day) <= monthDays(int(d.year), int(d.month))
}
func (d CivilDate) String() string {
	if !d.Valid() {
		return ""
	}
	return fmt.Sprintf("%04d-%02d-%02d", d.year, d.month, d.day)
}
func digits(value string) (int, error) {
	if len(value) == 0 || len(value) > 9 {
		return 0, failure(InvalidInput)
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, failure(InvalidInput)
		}
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, failure(InvalidInput)
	}
	return n, nil
}
func ParseDate(value string) (CivilDate, error) {
	if len(value) != 10 || value[4] != '-' || value[7] != '-' {
		return CivilDate{}, failure(InvalidDate)
	}
	y, ey := digits(value[:4])
	m, em := digits(value[5:7])
	d, ed := digits(value[8:])
	if ey != nil || em != nil || ed != nil || y < 1 || y > 9999 || m < 1 || m > 12 || d < 1 || d > monthDays(y, m) {
		return CivilDate{}, failure(InvalidDate)
	}
	return CivilDate{uint16(y), uint8(m), uint8(d)}, nil
}
func (d CivilDate) ordinal() int {
	y := int(d.year) - 1
	n := y*365 + y/4 - y/100 + y/400
	for m := 1; m < int(d.month); m++ {
		n += monthDays(int(d.year), m)
	}
	return n + int(d.day) - 1
}
func (d CivilDate) add(days int) (CivilDate, error) {
	if !d.Valid() || days < -366 || days > 366 {
		return CivilDate{}, failure(InvalidDate)
	}
	// UTC conversion is safe only after civil admission, with no DST resolver.
	t := time.Date(int(d.year), time.Month(d.month), int(d.day), 0, 0, 0, 0, time.UTC).AddDate(0, 0, days)
	if t.Year() < 1 || t.Year() > 9999 {
		return CivilDate{}, failure(InvalidDate)
	}
	return CivilDate{uint16(t.Year()), uint8(t.Month()), uint8(t.Day())}, nil
}

// LocalDateTime preserves the declared DTSTART wall clock, including an initial
// nonexistent time. Resolution and generated-occurrence admission are separate.
type LocalDateTime struct {
	date                 CivilDate
	hour, minute, second uint8
}

func (v LocalDateTime) Valid() bool {
	return v.date.Valid() && v.hour < 24 && v.minute < 60 && v.second < 60
}
func (v LocalDateTime) String() string {
	if !v.Valid() {
		return ""
	}
	return fmt.Sprintf("%sT%02d:%02d:%02d", v.date.String(), v.hour, v.minute, v.second)
}
func ParseLocalDateTime(value string) (LocalDateTime, error) {
	if len(value) != 19 || value[10] != 'T' || value[13] != ':' || value[16] != ':' {
		return LocalDateTime{}, failure(InvalidDate)
	}
	d, err := ParseDate(value[:10])
	h, eh := digits(value[11:13])
	m, em := digits(value[14:16])
	s, es := digits(value[17:19])
	if err != nil || eh != nil || em != nil || es != nil || h > 23 || m > 59 || s > 59 {
		return LocalDateTime{}, failure(InvalidDate)
	}
	return LocalDateTime{d, uint8(h), uint8(m), uint8(s)}, nil
}
func localFromTime(t time.Time) LocalDateTime {
	return LocalDateTime{CivilDate{uint16(t.Year()), uint8(t.Month()), uint8(t.Day())}, uint8(t.Hour()), uint8(t.Minute()), uint8(t.Second())}
}
func (v LocalDateTime) wallUTC() time.Time {
	return time.Date(int(v.date.year), time.Month(v.date.month), int(v.date.day), int(v.hour), int(v.minute), int(v.second), 0, time.UTC)
}
func (v LocalDateTime) compare(other LocalDateTime) int {
	a, b := v.wallUTC(), other.wallUTC()
	if a.Before(b) {
		return -1
	}
	if a.After(b) {
		return 1
	}
	return 0
}

// Instant has no valid Go zero, fractional second, infinity, or year outside1..9999.
type Instant struct{ value time.Time }

func NewInstant(value time.Time) (Instant, error) {
	value = value.UTC()
	if value.IsZero() || value.Year() < 1 || value.Year() > 9999 || value.Nanosecond() != 0 {
		return Instant{}, failure(InvalidDate)
	}
	return Instant{value.UTC()}, nil
}
func ParseInstant(value string) (Instant, error) {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return Instant{}, failure(InvalidDate)
	}
	return NewInstant(t)
}
func (v Instant) Valid() bool {
	return !v.value.IsZero() && v.value.Year() >= 1 && v.value.Year() <= 9999 && v.value.Nanosecond() == 0
}
func (v Instant) Time() (time.Time, bool) { return v.value, v.Valid() }
func (v Instant) String() string {
	if !v.Valid() {
		return ""
	}
	return v.value.Format(time.RFC3339)
}
