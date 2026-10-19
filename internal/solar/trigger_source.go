package solar

import (
	"context"
	"fmt"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/domain"
	"time"
)

type triggerSource struct {
	settings   *Settings
	calculator *Calculator
}

func (*triggerSource) Format(out fmt.State, _ rune) {
	_, _ = out.Write([]byte("solar.TriggerSource(<private>)"))
}

func NewTriggerSource(settings Settings) (automationtriggers.SunSource, error) {
	calculator, err := NewCalculator(settings)
	if err != nil {
		return nil, err
	}
	return &triggerSource{settings: &settings, calculator: calculator}, nil
}

func ConfigureTriggerInput(settings Settings, input automationtriggers.PlanInput) (automationtriggers.PlanInput, error) {
	source, err := NewTriggerSource(settings)
	if err != nil {
		return automationtriggers.PlanInput{}, err
	}
	observer, err := settings.Observer()
	if err != nil {
		return automationtriggers.PlanInput{}, err
	}
	latitude, longitude, _, err := observer.Components()
	if err != nil {
		return automationtriggers.PlanInput{}, err
	}
	position, err := automationtriggers.NewSolarPosition(latitude, longitude)
	if err != nil {
		return automationtriggers.PlanInput{}, failure(InvalidSettings)
	}
	zone, err := settings.Zone()
	if err != nil {
		return automationtriggers.PlanInput{}, err
	}
	input.Zone, input.SolarPosition, input.SunSource = zone.Name(), position, source
	return input, nil
}

func (s *triggerSource) NextSunset(ctx context.Context, after time.Time, zone string, position automationtriggers.SolarPosition) (automationtriggers.SunResult, error) {
	if s == nil || s.calculator == nil || s.settings == nil || !s.settings.Valid() {
		return automationtriggers.SunResult{}, failure(InvalidSettings)
	}
	observer, _ := s.settings.Observer()
	latitude, longitude, _, err := observer.Components()
	if err != nil {
		return automationtriggers.SunResult{}, err
	}
	requestedLatitude, requestedLongitude, err := position.Components()
	configuredZone, _ := s.settings.Zone()
	if err != nil || requestedLatitude != latitude || requestedLongitude != longitude || zone != configuredZone.Name() {
		return automationtriggers.SunResult{}, failure(InvalidSettings)
	}
	occurrence, err := s.calculator.NextEvent(ctx, domain.SolarSetting, after)
	if err != nil {
		return automationtriggers.SunResult{}, err
	}
	at, reason, kind, err := occurrence.Components()
	if err != nil {
		return automationtriggers.SunResult{}, err
	}
	switch kind {
	case domain.SolarOccurrencePresent:
		if !at.After(after) {
			return automationtriggers.SunResult{}, failure(CalculationFailed)
		}
		return automationtriggers.SunEventAt(at)
	case domain.SolarOccurrenceAbsent:
		if reason != domain.SolarNoEventWithinSearchHorizon {
			return automationtriggers.SunResult{}, failure(CalculationFailed)
		}
		return automationtriggers.NoSunEvent(), nil
	case domain.SolarOccurrenceUnspecified:
		return automationtriggers.SunResult{}, failure(CalculationFailed)
	default:
		return automationtriggers.SunResult{}, failure(CalculationFailed)
	}
}
