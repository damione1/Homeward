package calendars

import (
	"context"
	"time"
)

// Components returns scalar copies. It never exposes a calendar or zone cache.
func (d CivilDate) Components() (int, time.Month, int, error) {
	if !d.Valid() {
		return 0, 0, 0, failure(InvalidDate)
	}
	return int(d.year), time.Month(d.month), int(d.day), nil
}

// AddDays advances civil dates, independently of daylight saving transitions.
// The existing calendar boundary limits a single addition to +/-366 days.
func (d CivilDate) AddDays(days int) (CivilDate, error) { return d.add(days) }

// LocalDate projects an instant using the admitted, pinned timezone data.
// Fractional seconds are allowed; no mutable *time.Location escapes the Zone.
func (z Zone) LocalDate(instant time.Time) (CivilDate, error) {
	return z.LocalDateContext(context.Background(), instant)
}
func (z Zone) LocalDateContext(ctx context.Context, instant time.Time) (CivilDate, error) {
	if !z.Valid() || instant.IsZero() || instant.Year() < 1 || instant.Year() > 9999 {
		return CivilDate{}, failure(InvalidDate)
	}
	local, err := z.localAt(ctx, instant)
	if err != nil {
		return CivilDate{}, err
	}
	if !local.Valid() {
		return CivilDate{}, failure(InvalidDate)
	}
	return local.date, nil
}
