package server

import (
	"context"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/solar"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/workday"
)

// Internal dependency injection preserves the configured production Run path.
// It injects clocks and a consumer without replacing configured pilots.
type runDependencies struct {
	sequenceClock            sequencer.Clock
	solarClock               automationtriggers.Clock
	workdayClock             automationtriggers.Clock
	configureWorkdayCalendar func(context.Context, workday.CalendarReader) error
	observe                  func(context.Context, *sequencer.Sequencer) error
}

func addConfiguredSolar(plan *RuntimePlan, settings solar.Settings, clock automationtriggers.Clock) error {
	if plan == nil {
		return integrations.ErrInvalidInput
	}
	identity, ok := plan.Identity.(*registry.Repository)
	if !ok {
		return ErrIdentityNotConfigured
	}
	instance, err := solar.InstanceID()
	if err != nil {
		return err
	}
	factory, err := solar.NewPilotFactory(settings, clock)
	if err != nil {
		return err
	}
	spec, err := integrations.NewPilotSpec(instance, factory)
	if err != nil {
		return err
	}
	definitions, err := solar.ConfiguredBindings()
	if err != nil {
		return err
	}
	previous := plan.Configure
	plan.Configure = func(ctx context.Context, host *integrations.Manager, store *storage.Store) error {
		for _, definition := range definitions {
			alias, err := definition.Alias().Value()
			if err != nil {
				return err
			}
			if _, err := identity.ConfigureBinding(ctx, instance, definition.SourceKey(), alias); err != nil {
				return err
			}
		}
		if previous != nil {
			return previous(ctx, host, store)
		}
		return nil
	}
	plan.Pilots = append(plan.Pilots, spec)
	return nil
}
