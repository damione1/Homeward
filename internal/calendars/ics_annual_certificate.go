package calendars

import (
	"errors"
	"time"
)

// This certificate checks satisfiability/anchor membership only. It never
// expands, emits, merges, sorts, or schedules recurrence instances. rrule-go
// remains the sole producer of transitions in the measured path.
type annualSelector uint8

const (
	annualInvalid annualSelector = iota
	annualMonthDay
	annualOrdinalWeekday
)

type annualShape struct {
	month    time.Month
	selector annualSelector
	day      int
	ordinal  int
	weekday  time.Weekday
}
type annualCertificate struct {
	admitted bool
	shape    annualShape
}

var errAnnualInvalid = errors.New("preflight: invalid annual shape")
var errAnnualEmpty = errors.New("preflight: unsatisfiable annual shape")
var errAnnualAnchor = errors.New("preflight: DTSTART does not match annual shape")

func (s annualShape) candidateDay(year int) (int, bool) {
	last := time.Date(year, s.month+1, 0, 0, 0, 0, 0, time.UTC).Day()
	switch s.selector {
	case annualMonthDay:
		day := s.day
		if day < 0 {
			day = last + 1 + day
		}
		return day, day >= 1 && day <= last
	case annualOrdinalWeekday:
		if s.ordinal > 0 {
			first := time.Date(year, s.month, 1, 0, 0, 0, 0, time.UTC)
			day := 1 + (int(s.weekday)-int(first.Weekday())+7)%7 + (s.ordinal-1)*7
			return day, day <= last
		}
		end := time.Date(year, s.month, last, 0, 0, 0, 0, time.UTC)
		day := last - (int(end.Weekday())-int(s.weekday)+7)%7 + (s.ordinal+1)*7
		return day, day >= 1
	case annualInvalid:
		return 0, false
	}
	return 0, false
}
func certifyAnnualShape(s annualShape) (annualCertificate, error) {
	if s.month < 1 || s.month > 12 {
		return annualCertificate{}, errAnnualInvalid
	}
	switch s.selector {
	case annualMonthDay:
		if s.day == 0 || s.day < -31 || s.day > 31 || s.ordinal != 0 {
			return annualCertificate{}, errAnnualInvalid
		}
	case annualOrdinalWeekday:
		if s.day != 0 || s.ordinal == 0 || s.ordinal < -5 || s.ordinal > 5 || s.weekday < time.Sunday || s.weekday > time.Saturday {
			return annualCertificate{}, errAnnualInvalid
		}
	case annualInvalid:
		return annualCertificate{}, errAnnualInvalid
	default:
		return annualCertificate{}, errAnnualInvalid
	}
	// Fixed Gregorian cycle, at most400 scalar predicate checks; no Next.
	for year := 2000; year < 2400; year++ {
		if _, ok := s.candidateDay(year); ok {
			return annualCertificate{admitted: true, shape: s}, nil
		}
	}
	return annualCertificate{}, errAnnualEmpty
}
func (c annualCertificate) checkAnchor(anchor time.Time) error {
	if !c.admitted || anchor.Year() < 1 || anchor.Year() > 9999 || anchor.Month() != c.shape.month || anchor.Nanosecond() != 0 {
		return errAnnualAnchor
	}
	day, ok := c.shape.candidateDay(anchor.Year())
	if !ok || day != anchor.Day() {
		return errAnnualAnchor
	}
	return nil
}
