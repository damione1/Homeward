package calendars

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

type storedSpan struct {
	kind               int16
	startDate, endDate pgtype.Date
	anchor             pgtype.Timestamp
	start, end         pgtype.Timestamptz
	zone, data         pgtype.Text
}

func (s *storedSpan) destinations() []any {
	return []any{&s.kind, &s.startDate, &s.endDate, &s.anchor, &s.start, &s.end, &s.zone, &s.data}
}
func (s storedSpan) arguments() []any {
	return []any{s.kind, s.startDate, s.endDate, s.anchor, s.start, s.end, s.zone, s.data}
}
func encodeSpan(span Span) storedSpan {
	if span.kind == AllDay {
		return storedSpan{kind: 1, startDate: pgtype.Date{Time: LocalDateTime{date: span.startDate}.wallUTC(), Valid: true}, endDate: pgtype.Date{Time: LocalDateTime{date: span.endDate}.wallUTC(), Valid: true}}
	}
	return storedSpan{kind: 2, anchor: pgtype.Timestamp{Time: span.anchor.wallUTC(), Valid: true}, start: pgtype.Timestamptz{Time: span.start.value, Valid: true}, end: pgtype.Timestamptz{Time: span.end.value, Valid: true}, zone: pgtype.Text{String: span.zone.name, Valid: true}, data: pgtype.Text{String: TimezoneDataID, Valid: true}}
}
func dateFromSQL(value pgtype.Date) (CivilDate, error) {
	if !value.Valid || value.InfinityModifier != pgtype.Finite || value.Time.Year() < 1 || value.Time.Year() > 9999 {
		return CivilDate{}, failure(Corrupt)
	}
	date, err := ParseDate(value.Time.Format("2006-01-02"))
	if err != nil {
		return CivilDate{}, failure(Corrupt)
	}
	return date, nil
}
func localFromSQL(value pgtype.Timestamp) (LocalDateTime, error) {
	if !value.Valid || value.InfinityModifier != pgtype.Finite || value.Time.Year() < 1 || value.Time.Year() > 9999 || value.Time.Nanosecond() != 0 {
		return LocalDateTime{}, failure(Corrupt)
	}
	local := localFromTime(value.Time)
	if !local.Valid() {
		return LocalDateTime{}, failure(Corrupt)
	}
	return local, nil
}
func instantFromSQL(value pgtype.Timestamptz) (Instant, error) {
	if !value.Valid || value.InfinityModifier != pgtype.Finite {
		return Instant{}, failure(Corrupt)
	}
	instant, err := NewInstant(value.Time)
	if err != nil {
		return Instant{}, failure(Corrupt)
	}
	return instant, nil
}
func decodeSpan(ctx context.Context, raw storedSpan) (Span, error) {
	switch raw.kind {
	case 1:
		if raw.anchor.Valid || raw.start.Valid || raw.end.Valid || raw.zone.Valid || raw.data.Valid {
			return Span{}, failure(Corrupt)
		}
		start, err := dateFromSQL(raw.startDate)
		if err != nil {
			return Span{}, err
		}
		end, err := dateFromSQL(raw.endDate)
		if err != nil {
			return Span{}, err
		}
		span, err := AllDaySpan(start, end)
		if err != nil {
			return Span{}, failure(Corrupt)
		}
		return span, nil
	case 2:
		if raw.startDate.Valid || raw.endDate.Valid || !raw.zone.Valid || !raw.data.Valid {
			return Span{}, failure(Corrupt)
		}
		if raw.data.String != TimezoneDataID {
			return Span{}, failure(Incompatible)
		}
		anchor, err := localFromSQL(raw.anchor)
		if err != nil {
			return Span{}, err
		}
		start, err := instantFromSQL(raw.start)
		if err != nil {
			return Span{}, err
		}
		end, err := instantFromSQL(raw.end)
		if err != nil {
			return Span{}, err
		}
		zone, err := ParseZone(raw.zone.String)
		if err != nil {
			return Span{}, decodeFailure(err)
		}
		span, err := TimedSpan(ctx, anchor, end, zone)
		if err != nil {
			return Span{}, decodeFailure(err)
		}
		if span.start != start {
			return Span{}, failure(Incompatible)
		}
		return span, nil
	default:
		return Span{}, failure(Corrupt)
	}
}

type storedPoint struct {
	kind  int16
	key   string
	date  pgtype.Date
	local pgtype.Timestamp
}

func encodePoint(point Point) storedPoint {
	if point.kind == AllDay {
		return storedPoint{kind: 1, key: point.String(), date: pgtype.Date{Time: point.wall().wallUTC(), Valid: true}}
	}
	return storedPoint{kind: 2, key: point.String(), local: pgtype.Timestamp{Time: point.local.wallUTC(), Valid: true}}
}
func (p *storedPoint) destinations() []any { return []any{&p.kind, &p.key, &p.date, &p.local} }
func (p storedPoint) arguments() []any     { return []any{p.kind, p.key, p.date, p.local} }
func decodePoint(raw storedPoint) (Point, error) {
	var point Point
	switch raw.kind {
	case 1:
		if raw.local.Valid {
			return Point{}, failure(Corrupt)
		}
		date, err := dateFromSQL(raw.date)
		if err != nil {
			return Point{}, err
		}
		point = Point{kind: AllDay, date: date}
	case 2:
		if raw.date.Valid {
			return Point{}, failure(Corrupt)
		}
		local, err := localFromSQL(raw.local)
		if err != nil {
			return Point{}, err
		}
		point = Point{kind: Timed, local: local}
	default:
		return Point{}, failure(Corrupt)
	}
	if point.String() != raw.key {
		return Point{}, failure(Corrupt)
	}
	return point, nil
}

func presenceFromSQL(value int16) (Presence, error) {
	switch value {
	case 1:
		return Present, nil
	case 2:
		return Removed, nil
	default:
		return PresenceInvalid, failure(Corrupt)
	}
}
func validUpdated(value pgtype.Timestamptz) (time.Time, error) {
	if !value.Valid || value.InfinityModifier != pgtype.Finite || value.Time.IsZero() || value.Time.Year() < 1 || value.Time.Year() > 9999 {
		return time.Time{}, failure(Corrupt)
	}
	return value.Time.UTC(), nil
}

func decodeFailure(err error) error {
	var e *Error
	if errors.As(err, &e) {
		switch e.Code() {
		case Canceled, BudgetExceeded, CapacityExceeded, Incompatible:
			return err
		}
	}
	return failure(Corrupt)
}
