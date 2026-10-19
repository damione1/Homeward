package solar

import (
	"context"
	"time"

	"github.com/damione1/Homeward/internal/domain"
)

type Phase uint8

const (
	PhaseUnspecified Phase = iota
	PhaseNight
	PhaseAstronomicalTwilight
	PhaseNauticalTwilight
	PhaseTwilight
	PhaseSmallDay
	PhaseDay
)

func (p Phase) Name() (string, error) {
	switch p {
	case PhaseNight:
		return "night", nil
	case PhaseAstronomicalTwilight:
		return "astronomical_twilight", nil
	case PhaseNauticalTwilight:
		return "nautical_twilight", nil
	case PhaseTwilight:
		return "twilight", nil
	case PhaseSmallDay:
		return "small_day", nil
	case PhaseDay:
		return "day", nil
	case PhaseUnspecified:
		return "", failure(CalculationFailed)
	default:
		return "", failure(CalculationFailed)
	}
}

func (p Phase) PositionInterval() (time.Duration, error) {
	switch p {
	case PhaseNight:
		return 20 * time.Minute, nil
	case PhaseAstronomicalTwilight:
		return 8 * time.Minute, nil
	case PhaseNauticalTwilight, PhaseDay:
		return 4 * time.Minute, nil
	case PhaseTwilight, PhaseSmallDay:
		return 2 * time.Minute, nil
	case PhaseUnspecified:
		return 0, failure(CalculationFailed)
	default:
		return 0, failure(CalculationFailed)
	}
}

// Computation is pure calculation output, not a running or published service.
// It contains no observer, timezone object, timer, goroutine, or shared cache.
type Computation struct {
	valid        bool
	value        domain.SolarValue
	computedAt   time.Time
	nextChange   time.Time
	nextPosition time.Time
	phase        Phase
	evaluations  int
}

func (c Computation) Components() (domain.SolarValue, time.Time, Phase, time.Time, error) {
	if !c.valid || c.computedAt.IsZero() || !c.nextChange.After(c.computedAt) || c.evaluations < 1 || c.evaluations > 12*MaxSearchDates {
		return domain.SolarValue{}, time.Time{}, PhaseUnspecified, time.Time{}, failure(CalculationFailed)
	}
	if _, err := c.value.Components(); err != nil {
		return domain.SolarValue{}, time.Time{}, PhaseUnspecified, time.Time{}, err
	}
	if _, err := c.phase.Name(); err != nil {
		return domain.SolarValue{}, time.Time{}, PhaseUnspecified, time.Time{}, err
	}
	return c.value, c.computedAt, c.phase, c.nextChange, nil
}

func (c Computation) NextPosition() (time.Time, bool, error) {
	if _, _, _, _, err := c.Components(); err != nil {
		return time.Time{}, false, err
	}
	if c.nextPosition.IsZero() {
		return time.Time{}, false, nil
	}
	if !c.nextPosition.After(c.computedAt) || !c.nextPosition.Before(c.nextChange) {
		return time.Time{}, false, failure(CalculationFailed)
	}
	return c.nextPosition, true, nil
}

func (c Computation) DateEvaluations() (int, error) {
	if _, _, _, _, err := c.Components(); err != nil {
		return 0, err
	}
	return c.evaluations, nil
}

type phaseRequest struct {
	request eventRequest
	before  Phase
	store   domain.SolarEventKind
}

// The order is HA Sun.update_events order, including its first-event tie rule.
func phaseRequests() [12]phaseRequest {
	dawn, _ := requestFor(domain.SolarDawn)
	dusk, _ := requestFor(domain.SolarDusk)
	rising, _ := requestFor(domain.SolarRising)
	setting, _ := requestFor(domain.SolarSetting)
	noon, _ := requestFor(domain.SolarNoon)
	midnight, _ := requestFor(domain.SolarMidnight)
	return [12]phaseRequest{
		{eventRequest{domain.SolarDawn, 108, traversalRising}, PhaseNight, domain.SolarEventUnspecified},
		{eventRequest{domain.SolarDawn, 102, traversalRising}, PhaseAstronomicalTwilight, domain.SolarEventUnspecified},
		{dawn, PhaseNauticalTwilight, domain.SolarDawn},
		{rising, PhaseTwilight, domain.SolarRising},
		{eventRequest{domain.SolarDawn, 80, traversalRising}, PhaseSmallDay, domain.SolarEventUnspecified},
		{noon, PhaseUnspecified, domain.SolarNoon},
		{eventRequest{domain.SolarDusk, 80, traversalSetting}, PhaseDay, domain.SolarEventUnspecified},
		{setting, PhaseSmallDay, domain.SolarSetting},
		{dusk, PhaseTwilight, domain.SolarDusk},
		{eventRequest{domain.SolarDusk, 102, traversalSetting}, PhaseNauticalTwilight, domain.SolarEventUnspecified},
		{eventRequest{domain.SolarDusk, 108, traversalSetting}, PhaseAstronomicalTwilight, domain.SolarEventUnspecified},
		{midnight, PhaseUnspecified, domain.SolarMidnight},
	}
}

func phaseAt(elevation float64) Phase {
	switch {
	case elevation >= 10:
		return PhaseDay
	case elevation >= 0:
		return PhaseSmallDay
	case elevation >= -6:
		return PhaseTwilight
	case elevation >= -12:
		return PhaseNauticalTwilight
	case elevation >= -18:
		return PhaseAstronomicalTwilight
	default:
		return PhaseNight
	}
}

// Compute follows HA Sun's phase and position rules with caller-supplied time.
// Cancellation is checked between events and dates. A wall-time budget is not
// imposed here; the proposed service budget still requires P0 measurements.
func (c *Calculator) Compute(ctx context.Context, now time.Time) (Computation, error) {
	if err := c.ready(ctx); err != nil {
		return Computation{}, err
	}
	if !instantValid(now) {
		return Computation{}, failure(InvalidDate)
	}
	now = now.UTC()
	result := Computation{computedAt: now, nextChange: now.AddDate(0, 0, 400)}
	var input domain.SolarValueInput
	for _, phaseEvent := range phaseRequests() {
		occurrence, err := c.nextEvent(ctx, phaseEvent.request, now, &result.evaluations)
		if err != nil {
			return Computation{}, err
		}
		if phaseEvent.store != domain.SolarEventUnspecified {
			index, err := phaseEvent.store.Index()
			if err != nil {
				return Computation{}, failure(CalculationFailed)
			}
			input.Next[index] = occurrence
		}
		instant, _, kind, err := occurrence.Components()
		if err != nil {
			return Computation{}, failure(CalculationFailed)
		}
		switch kind {
		case domain.SolarOccurrencePresent:
			if instant.Before(result.nextChange) {
				result.nextChange, result.phase = instant, phaseEvent.before
			}
		case domain.SolarOccurrenceAbsent:
		case domain.SolarOccurrenceUnspecified:
			return Computation{}, failure(CalculationFailed)
		default:
			return Computation{}, failure(CalculationFailed)
		}
	}
	if result.phase == PhaseUnspecified {
		elevation, _, err := position(c.settings.observer, result.nextChange)
		if err != nil {
			return Computation{}, err
		}
		result.phase = phaseAt(elevation)
	}
	elevation, azimuth, err := position(c.settings.observer, now)
	if err != nil {
		return Computation{}, err
	}
	input.ElevationDegrees, err = roundHA2(elevation)
	if err != nil {
		return Computation{}, err
	}
	input.AzimuthDegrees, err = roundHA2(azimuth)
	if err != nil {
		return Computation{}, err
	}
	input.Horizon = domain.SolarBelowHorizon
	if input.ElevationDegrees > -0.833 {
		input.Horizon = domain.SolarAboveHorizon
	}
	noonIndex, _ := domain.SolarNoon.Index()
	midnightIndex, _ := domain.SolarMidnight.Index()
	noon, _, noonKind, _ := input.Next[noonIndex].Components()
	midnight, _, midnightKind, _ := input.Next[midnightIndex].Components()
	if noonKind != domain.SolarOccurrencePresent || midnightKind != domain.SolarOccurrencePresent {
		return Computation{}, failure(CalculationFailed)
	}
	input.Rising = noon.Before(midnight)
	result.value, err = domain.NewSolarValue(input)
	if err != nil {
		return Computation{}, err
	}
	interval, err := result.phase.PositionInterval()
	if err != nil {
		return Computation{}, err
	}
	if !now.Add(interval + interval/4).After(result.nextChange) {
		result.nextPosition = now.Add(interval)
	}
	if err := contextError(ctx); err != nil {
		return Computation{}, err
	}
	result.valid = true
	if _, _, _, _, err := result.Components(); err != nil {
		return Computation{}, err
	}
	return result, nil
}

func (c *Calculator) Snapshot(ctx context.Context, now time.Time) (domain.SolarValue, error) {
	computed, err := c.Compute(ctx, now)
	if err != nil {
		return domain.SolarValue{}, err
	}
	value, _, _, _, err := computed.Components()
	return value, err
}
