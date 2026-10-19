package calendars

import (
	"context"
	"time"
)

type TemporalKind uint8

const (
	TemporalInvalid TemporalKind = iota
	AllDay
	Timed
)

// Span is a private temporal sum. All-day dates do not acquire an inferred
// timezone. Timed spans retain both their instants and declared local anchor.
type Span struct {
	kind               TemporalKind
	startDate, endDate CivilDate
	anchor             LocalDateTime
	start, end         Instant
	zone               Zone
}

func AllDaySpan(start, end CivilDate) (Span, error) {
	if !end.Valid() {
		return Span{}, failure(MissingEnd)
	}
	if !start.Valid() || end.ordinal() <= start.ordinal() || end.ordinal()-start.ordinal() > 366 {
		return Span{}, failure(InvalidDate)
	}
	return Span{kind: AllDay, startDate: start, endDate: end}, nil
}
func TimedSpan(ctx context.Context, anchor LocalDateTime, end Instant, zone Zone) (Span, error) {
	if err := contextValid(ctx); err != nil {
		return Span{}, err
	}
	if !end.Valid() {
		return Span{}, failure(MissingEnd)
	}
	if !anchor.Valid() || !zone.Valid() {
		return Span{}, failure(InvalidDate)
	}
	start, present, err := resolveWall(ctx, anchor, zone, true)
	if err != nil {
		return Span{}, err
	}
	if !present {
		return Span{}, failure(InvalidDate)
	}
	if !end.value.After(start.value) || end.value.Sub(start.value) > 366*24*time.Hour {
		return Span{}, failure(InvalidDate)
	}
	return Span{kind: Timed, anchor: anchor, start: start, end: end, zone: zone}, nil
}
func TimedInstants(ctx context.Context, start, end Instant, zone Zone) (Span, error) {
	if !start.Valid() || !zone.Valid() {
		return Span{}, failure(InvalidDate)
	}
	span, err := TimedSpan(ctx, localFromTime(start.value.In(zone.location)), end, zone)
	if err != nil {
		return Span{}, err
	}
	if span.start.value != start.value {
		return Span{}, failure(InvalidDate)
	}
	return span, nil
}
func (s Span) Valid() bool {
	switch s.kind {
	case AllDay:
		return s.startDate.Valid() && s.endDate.Valid() && s.endDate.ordinal() > s.startDate.ordinal() && s.endDate.ordinal()-s.startDate.ordinal() <= 366 && s.anchor == (LocalDateTime{}) && s.start == (Instant{}) && s.end == (Instant{}) && !s.zone.Valid()
	case Timed:
		return s.anchor.Valid() && s.start.Valid() && s.end.Valid() && s.zone.Valid() && s.end.value.After(s.start.value) && s.end.value.Sub(s.start.value) <= 366*24*time.Hour && s.startDate == (CivilDate{}) && s.endDate == (CivilDate{})
	default:
		return false
	}
}
func (s Span) Kind() TemporalKind {
	if !s.Valid() {
		return TemporalInvalid
	}
	return s.kind
}
func (s Span) Dates() (CivilDate, CivilDate, bool) {
	return s.startDate, s.endDate, s.Valid() && s.kind == AllDay
}
func (s Span) Instants() (Instant, Instant, Zone, bool) {
	return s.start, s.end, s.zone, s.Valid() && s.kind == Timed
}
func (s Span) Anchor() (LocalDateTime, bool) { return s.anchor, s.Valid() && s.kind == Timed }
func (s Span) point() Point {
	if s.kind == AllDay {
		return Point{kind: AllDay, date: s.startDate}
	}
	return Point{kind: Timed, local: s.anchor}
}

// Point is a recurrence's original date or declared local time. It is scoped
// to a series when paired with EventKey, never identified by its moved start.
type Point struct {
	kind  TemporalKind
	date  CivilDate
	local LocalDateTime
}

func DatePoint(value CivilDate) (Point, error) {
	if !value.Valid() {
		return Point{}, failure(InvalidDate)
	}
	return Point{kind: AllDay, date: value}, nil
}
func TimePoint(value LocalDateTime) (Point, error) {
	if !value.Valid() {
		return Point{}, failure(InvalidDate)
	}
	return Point{kind: Timed, local: value}, nil
}
func (p Point) Valid() bool {
	switch p.kind {
	case AllDay:
		return p.date.Valid() && p.local == (LocalDateTime{})
	case Timed:
		return p.local.Valid() && p.date == (CivilDate{})
	default:
		return false
	}
}
func (p Point) Kind() TemporalKind {
	if !p.Valid() {
		return TemporalInvalid
	}
	return p.kind
}
func (p Point) String() string {
	if !p.Valid() {
		return ""
	}
	if p.kind == AllDay {
		return p.date.String()
	}
	return p.local.String()
}
func ParsePoint(kind TemporalKind, value string) (Point, error) {
	switch kind {
	case AllDay:
		d, err := ParseDate(value)
		if err != nil {
			return Point{}, err
		}
		return DatePoint(d)
	case Timed:
		t, err := ParseLocalDateTime(value)
		if err != nil {
			return Point{}, err
		}
		return TimePoint(t)
	default:
		return Point{}, failure(InvalidInput)
	}
}
func (p Point) datePart() CivilDate {
	if p.kind == AllDay {
		return p.date
	}
	return p.local.date
}
func (p Point) wall() LocalDateTime {
	if p.kind == AllDay {
		return LocalDateTime{date: p.date}
	}
	return p.local
}

type RecurrenceID struct {
	event EventKey
	point Point
}

func OccurrenceIdentity(event EventKey, point Point) (RecurrenceID, error) {
	if !event.Valid() || !point.Valid() {
		return RecurrenceID{}, failure(InvalidInput)
	}
	return RecurrenceID{event, point}, nil
}
func (r RecurrenceID) Valid() bool     { return r.event.Valid() && r.point.Valid() }
func (r RecurrenceID) Event() EventKey { return r.event }
func (r RecurrenceID) Point() Point    { return r.point }
