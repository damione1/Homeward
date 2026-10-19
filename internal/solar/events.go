package solar

import (
	"context"
	"fmt"
	"time"

	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/domain"
)

// MaxSearchDates is HA helpers.sun's inclusive day offsets -1 through +366.
const MaxSearchDates = 368

type Calculator struct{ settings Settings }

func (c *Calculator) Format(out fmt.State, _ rune) {
	_, _ = out.Write([]byte("solar.Calculator(<private>)"))
}
func (c *Calculator) String() string   { return "solar.Calculator(<private>)" }
func (c *Calculator) GoString() string { return c.String() }

func NewCalculator(settings Settings) (*Calculator, error) {
	if !settings.Valid() {
		return nil, failure(InvalidSettings)
	}
	return &Calculator{settings: settings}, nil
}

type eventRequest struct {
	event     domain.SolarEventKind
	zenith    float64
	direction traversal
}

func requestFor(kind domain.SolarEventKind) (eventRequest, error) {
	switch kind {
	case domain.SolarDawn:
		return eventRequest{kind, 96, traversalRising}, nil
	case domain.SolarDusk:
		return eventRequest{kind, 96, traversalSetting}, nil
	case domain.SolarMidnight, domain.SolarNoon:
		return eventRequest{event: kind}, nil
	case domain.SolarRising:
		return eventRequest{kind, 90 + 32.0/(60*2), traversalRising}, nil
	case domain.SolarSetting:
		return eventRequest{kind, 90 + 32.0/(60*2), traversalSetting}, nil
	case domain.SolarEventUnspecified:
		return eventRequest{}, failure(InvalidEvent)
	default:
		return eventRequest{}, failure(InvalidEvent)
	}
}

func (c *Calculator) ready(ctx context.Context) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if c == nil || !c.settings.Valid() {
		return failure(InvalidSettings)
	}
	return nil
}

func dateUTC(date calendars.CivilDate) (time.Time, error) {
	year, month, day, err := date.Components()
	if err != nil {
		return time.Time{}, failure(InvalidDate)
	}
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC), nil
}

func instantValid(instant time.Time) bool {
	return !instant.IsZero() && instant.UTC().Year() >= 2 && instant.UTC().Year() <= 9998
}

func (c *Calculator) eventAtDate(ctx context.Context, request eventRequest, date calendars.CivilDate) (domain.SolarOccurrence, error) {
	if err := c.ready(ctx); err != nil {
		return domain.SolarOccurrence{}, err
	}
	base, err := dateUTC(date)
	if err != nil {
		return domain.SolarOccurrence{}, err
	}
	var instant time.Time
	var present bool
	switch request.event {
	case domain.SolarNoon:
		instant, err = meridianTime(c.settings.observer, base, false)
		present = err == nil
	case domain.SolarMidnight:
		instant, err = meridianTime(c.settings.observer, base, true)
		present = err == nil
	case domain.SolarDawn, domain.SolarDusk, domain.SolarRising, domain.SolarSetting:
		instant, present, err = transit(c.settings.observer, base, request.zenith, request.direction)
	case domain.SolarEventUnspecified:
		return domain.SolarOccurrence{}, failure(InvalidEvent)
	default:
		return domain.SolarOccurrence{}, failure(InvalidEvent)
	}
	if err != nil {
		return domain.SolarOccurrence{}, err
	}
	if err := contextError(ctx); err != nil {
		return domain.SolarOccurrence{}, err
	}
	if !present {
		return domain.SolarOccurrenceMissing(domain.SolarNoTransitOnDate)
	}
	return domain.SolarOccurrenceAt(instant)
}

// EventAtDate preserves the requested civil date. The returned instant is UTC
// and can lie on another UTC day, including the previous solar midnight.
func (c *Calculator) EventAtDate(ctx context.Context, kind domain.SolarEventKind, date calendars.CivilDate) (domain.SolarOccurrence, error) {
	request, err := requestFor(kind)
	if err != nil {
		return domain.SolarOccurrence{}, err
	}
	return c.eventAtDate(ctx, request, date)
}

func (c *Calculator) nextEvent(ctx context.Context, request eventRequest, after time.Time, evaluated *int) (domain.SolarOccurrence, error) {
	if err := c.ready(ctx); err != nil {
		return domain.SolarOccurrence{}, err
	}
	if !instantValid(after) {
		return domain.SolarOccurrence{}, failure(InvalidDate)
	}
	after = after.UTC()
	localDate, err := c.settings.zone.LocalDate(after)
	if err != nil {
		return domain.SolarOccurrence{}, failure(InvalidDate)
	}
	for offset := -1; offset <= 366; offset++ {
		if err := contextError(ctx); err != nil {
			return domain.SolarOccurrence{}, err
		}
		date, err := localDate.AddDays(offset)
		if err != nil {
			return domain.SolarOccurrence{}, failure(InvalidDate)
		}
		if evaluated != nil {
			*evaluated += 1
		}
		candidate, err := c.eventAtDate(ctx, request, date)
		if err != nil {
			return domain.SolarOccurrence{}, err
		}
		instant, _, kind, err := candidate.Components()
		if err != nil {
			return domain.SolarOccurrence{}, failure(CalculationFailed)
		}
		switch kind {
		case domain.SolarOccurrencePresent:
			if instant.After(after) {
				return candidate, nil
			}
		case domain.SolarOccurrenceAbsent:
		case domain.SolarOccurrenceUnspecified:
			return domain.SolarOccurrence{}, failure(CalculationFailed)
		default:
			return domain.SolarOccurrence{}, failure(CalculationFailed)
		}
	}
	return domain.SolarOccurrenceMissing(domain.SolarNoEventWithinSearchHorizon)
}

// NextEvent skips daily absence, but never skips a calculation or context error.
// It returns the first strictly future occurrence in the HA bounded search.
func (c *Calculator) NextEvent(ctx context.Context, kind domain.SolarEventKind, after time.Time) (domain.SolarOccurrence, error) {
	request, err := requestFor(kind)
	if err != nil {
		return domain.SolarOccurrence{}, err
	}
	return c.nextEvent(ctx, request, after, nil)
}
