package workday

import (
	"context"

	"github.com/damione1/Homeward/internal/calendars"
)

// CalendarReader grants only bounded, calculated date reads. It carries no
// repository or mutating capability into later transport composition.
type CalendarReader interface {
	CalendarID() (calendars.CalendarID, error)
	Dates(context.Context, calendars.CivilDate, calendars.CivilDate) ([]calendars.Span, error)
}

type calendarReader struct {
	id       calendars.CalendarID
	settings Settings
}

func ValidateReader(reader CalendarReader) error {
	if nilInterface(reader) {
		return failure(InvalidIdentity)
	}
	id, err := reader.CalendarID()
	if err != nil || !id.Valid() || id.Origin() != calendars.Calculated {
		return failure(InvalidIdentity)
	}
	alias, ok := id.Alias()
	name, aliasErr := alias.Value()
	if !ok || aliasErr != nil || name != "calendar.workday_sensor_calendar" {
		return failure(InvalidIdentity)
	}
	return nil
}

func NewCalendarReader(id calendars.CalendarID, settings Settings) (CalendarReader, error) {
	alias, ok := id.Alias()
	name, err := alias.Value()
	if !ok || err != nil || name != "calendar.workday_sensor_calendar" || id.Origin() != calendars.Calculated {
		return nil, failure(InvalidIdentity)
	}
	if !settings.Valid() {
		return nil, failure(InvalidProfile)
	}
	return &calendarReader{id: id, settings: settings}, nil
}

func (r *calendarReader) CalendarID() (calendars.CalendarID, error) {
	if r == nil || !r.id.Valid() || r.id.Origin() != calendars.Calculated || !r.settings.Valid() {
		return calendars.CalendarID{}, failure(InvalidIdentity)
	}
	return r.id, nil
}

func (r *calendarReader) Dates(ctx context.Context, from, to calendars.CivilDate) ([]calendars.Span, error) {
	if r == nil {
		return nil, failure(InvalidIdentity)
	}
	if _, err := r.CalendarID(); err != nil {
		return nil, err
	}
	if err := validContext(ctx); err != nil {
		return nil, err
	}
	if _, err := calendars.DateWindow(from, to); err != nil {
		return nil, failure(InvalidWindow)
	}
	// A valid DateWindow is at most 366 days. No unbounded slice or cursor is
	// needed; the returned spans are fresh value copies.
	result := make([]calendars.Span, 0, 366)
	for date := from; date != to; {
		if err := validContext(ctx); err != nil {
			return nil, err
		}
		included, err := r.settings.IsWorkday(date)
		if err != nil {
			return nil, err
		}
		next, err := date.AddDays(1)
		if err != nil {
			return nil, failure(InvalidDate)
		}
		if included {
			span, err := calendars.AllDaySpan(date, next)
			if err != nil {
				return nil, failure(InvalidDate)
			}
			result = append(result, span)
		}
		date = next
	}
	return result, nil
}
