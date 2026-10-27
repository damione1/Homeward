package calendars

import (
	"context"
	"strings"
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
type spanMode uint8

const (
	spanInvalid spanMode = iota
	spanAllDay
	spanExact
	spanPoint
	spanNominal
)

type temporalSource uint8

const (
	sourceInvalid temporalSource = iota
	sourceZoned
	sourceUTC
	sourceTZID
	sourceFloating
	sourceExplicit
)

type Span struct {
	mode               spanMode
	source             temporalSource
	kind               TemporalKind
	startDate, endDate CivilDate
	anchor             LocalDateTime
	start, end         Instant
	zone               Zone
	endZone            Zone
	nominal            NominalLength
}

// NominalLength applies civil days first, then exact seconds, at each
// occurrence. It cannot be confused with an elapsed time.Duration.
type NominalLength struct {
	days, seconds int
	admitted      bool
}

func NewNominalLength(days, seconds int) (NominalLength, error) {
	if days < 0 || days > 366 || seconds < 0 || seconds > 366*86400-days*86400 || days == 0 && seconds == 0 {
		return NominalLength{}, failure(InvalidDate)
	}
	return NominalLength{days: days, seconds: seconds, admitted: true}, nil
}
func (n NominalLength) Valid() bool {
	return n.admitted && n.days >= 0 && n.days <= 366 && n.seconds >= 0 && n.seconds <= 366*86400-n.days*86400 && (n.days > 0 || n.seconds > 0)
}
func (n NominalLength) Days() int    { return n.days }
func (n NominalLength) Seconds() int { return n.seconds }

func nominalEnd(ctx context.Context, anchor LocalDateTime, start Instant, length NominalLength, zone Zone) (Instant, error) {
	if !length.Valid() || !anchor.Valid() || !start.Valid() || !zone.Valid() {
		return Instant{}, failure(InvalidDate)
	}
	end := start.value
	if length.days > 0 {
		wall := anchor.wallUTC().AddDate(0, 0, length.days)
		next := localFromTime(wall)
		if !next.Valid() {
			return Instant{}, failure(InvalidDate)
		}
		resolved, present, err := resolveWall(ctx, next, zone, true)
		if err != nil {
			return Instant{}, err
		}
		if !present {
			return Instant{}, failure(InvalidDate)
		}
		end = resolved.value
	}
	return NewInstant(end.Add(time.Duration(length.seconds) * time.Second))
}

func TimedPoint(ctx context.Context, anchor LocalDateTime, zone Zone) (Span, error) {
	if err := contextValid(ctx); err != nil {
		return Span{}, err
	}
	start, present, err := resolveWall(ctx, anchor, zone, true)
	if err != nil {
		return Span{}, err
	}
	if !present {
		return Span{}, failure(InvalidDate)
	}
	return Span{kind: Timed, mode: spanPoint, source: sourceZoned, anchor: anchor, start: start, end: start, zone: zone}, nil
}
func TimedNominalSpan(ctx context.Context, anchor LocalDateTime, length NominalLength, zone Zone) (Span, error) {
	if err := contextValid(ctx); err != nil {
		return Span{}, err
	}
	start, present, err := resolveWall(ctx, anchor, zone, true)
	if err != nil {
		return Span{}, err
	}
	if !present {
		return Span{}, failure(InvalidDate)
	}
	end, err := nominalEnd(ctx, anchor, start, length, zone)
	if err != nil {
		return Span{}, err
	}
	if !end.value.After(start.value) || end.value.Sub(start.value) > 366*24*time.Hour+52*time.Hour {
		return Span{}, failure(InvalidDate)
	}
	return Span{kind: Timed, mode: spanNominal, source: sourceZoned, anchor: anchor, start: start, end: end, zone: zone, nominal: length}, nil
}

func AllDaySpan(start, end CivilDate) (Span, error) {
	if !end.Valid() {
		return Span{}, failure(MissingEnd)
	}
	if !start.Valid() || end.ordinal() <= start.ordinal() || end.ordinal()-start.ordinal() > 366 {
		return Span{}, failure(InvalidDate)
	}
	return Span{kind: AllDay, mode: spanAllDay, startDate: start, endDate: end}, nil
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
	return Span{kind: Timed, mode: spanExact, source: sourceZoned, anchor: anchor, start: start, end: end, zone: zone}, nil
}

// SelectedTimedSpan preserves an explicit fold choice. It rejects a gap or
// mismatched anchor instead of applying RFC5545's implicit DTSTART resolution.
func SelectedTimedSpan(ctx context.Context, anchor LocalDateTime, start, end Instant, zone Zone) (Span, error) {
	if err := contextValid(ctx); err != nil {
		return Span{}, err
	}
	if !anchor.Valid() || !start.Valid() || !end.Valid() || !zone.Valid() || zone.embedded != nil || !end.value.After(start.value) || end.value.Sub(start.value) > 366*24*time.Hour {
		return Span{}, failure(InvalidDate)
	}
	projected, err := zone.localAt(ctx, start.value)
	if err != nil {
		return Span{}, err
	}
	if projected != anchor {
		return Span{}, failure(InvalidDate)
	}
	return Span{kind: Timed, mode: spanExact, source: sourceExplicit, anchor: anchor, start: start, end: end, zone: zone}, nil
}

func TimedInstants(ctx context.Context, start, end Instant, zone Zone) (Span, error) {
	if !start.Valid() || !zone.Valid() {
		return Span{}, failure(InvalidDate)
	}
	local, err := zone.localAt(ctx, start.value)
	if err != nil {
		return Span{}, err
	}
	span, err := TimedSpan(ctx, local, end, zone)
	if err != nil {
		return Span{}, err
	}
	if span.start.value != start.value {
		return Span{}, failure(InvalidDate)
	}
	return span, nil
}
func (s Span) Valid() bool {
	switch s.mode {
	case spanAllDay:
		return s.kind == AllDay && s.source == sourceInvalid && s.startDate.Valid() && s.endDate.Valid() && s.endDate.ordinal() > s.startDate.ordinal() && s.endDate.ordinal()-s.startDate.ordinal() <= 366 && s.anchor == (LocalDateTime{}) && s.start == (Instant{}) && s.end == (Instant{}) && s.zone == (Zone{}) && s.endZone == (Zone{}) && s.nominal == (NominalLength{})
	case spanExact, spanPoint, spanNominal:
		if s.endZone != (Zone{}) && !s.endZone.Valid() {
			return false
		}
		if s.kind != Timed || !s.anchor.Valid() || !s.start.Valid() || !s.end.Valid() || !s.zone.Valid() || s.startDate != (CivilDate{}) || s.endDate != (CivilDate{}) {
			return false
		}
		switch s.source {
		case sourceZoned, sourceUTC, sourceTZID, sourceFloating:
		case sourceExplicit:
			if s.mode != spanExact || s.zone.embedded != nil || s.endZone != (Zone{}) {
				return false
			}
		case sourceInvalid:
			return false
		default:
			return false
		}
		switch s.mode {
		case spanPoint:
			return s.end == s.start && s.nominal == (NominalLength{}) && s.endZone == (Zone{})
		case spanExact:
			return s.nominal == (NominalLength{}) && s.end.value.After(s.start.value) && s.end.value.Sub(s.start.value) <= 366*24*time.Hour
		case spanNominal:
			return s.nominal.Valid() && s.endZone == (Zone{}) && s.end.value.After(s.start.value) && s.end.value.Sub(s.start.value) <= 366*24*time.Hour+52*time.Hour
		}
	case spanInvalid:
		return false
	}
	return false
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

// EndLocal projects an instant for presentation with the same pinned server
// zone as the start. Clients need no inverse or forward timezone engine.
func (s Span) EndLocal(ctx context.Context) (LocalDateTime, error) {
	if !s.Valid() || s.kind != Timed {
		return LocalDateTime{}, failure(InvalidDate)
	}
	return s.zone.localAt(ctx, s.end.value)
}
func (s Span) Anchor() (LocalDateTime, bool)  { return s.anchor, s.Valid() && s.kind == Timed }
func (s Span) IsPoint() bool                  { return s.Valid() && s.kind == Timed && s.mode == spanPoint }
func (s Span) Nominal() (NominalLength, bool) { return s.nominal, s.Valid() && s.nominal.Valid() }
func (s Span) Floating() bool                 { return s.Valid() && s.source == sourceFloating }
func (s Span) EndZone() (Zone, bool)          { return s.endZone, s.Valid() && s.endZone.Valid() }
func (s Span) point() Point {
	if s.kind == AllDay {
		return Point{kind: AllDay, date: s.startDate}
	}
	return Point{kind: Timed, local: s.anchor}
}

// Point is a recurrence's original date or declared local time. It is scoped
// to a series when paired with EventKey, never identified by its moved start.
// originCivil is the intentional default for existing civil Point literals.
// A resolved origin distinguishes an explicit second-fold RDATE without a
// second recurrence engine or a silent change to its effective instant.
type pointOrigin uint8

const (
	originCivil pointOrigin = iota
	originResolved
)

type Point struct {
	origin   pointOrigin
	resolved Instant
	kind     TemporalKind
	date     CivilDate
	local    LocalDateTime
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
		return p.origin == originCivil && p.resolved == (Instant{}) && p.date.Valid() && p.local == (LocalDateTime{})
	case Timed:
		if !p.local.Valid() || p.date != (CivilDate{}) {
			return false
		}
		switch p.origin {
		case originCivil:
			return p.resolved == (Instant{})
		case originResolved:
			return p.resolved.Valid()
		}
		return false
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
	if p.origin == originResolved {
		return p.local.String() + "@" + p.resolved.String()
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
		if local, instant, ok := strings.Cut(value, "@"); ok {
			wall, e := ParseLocalDateTime(local)
			if e != nil {
				return Point{}, e
			}
			at, e := ParseInstant(instant)
			if e != nil {
				return Point{}, e
			}
			return Point{kind: Timed, origin: originResolved, local: wall, resolved: at}, nil
		}
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
