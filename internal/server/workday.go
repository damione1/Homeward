package server

import (
	"context"

	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/workday"
	"github.com/damione1/Homeward/scope"
)

func addConfiguredWorkday(plan *RuntimePlan, settings workday.Settings, clock automationtriggers.Clock) error {
	manifest, err := registry.LoadManifest(scope.BaselineV1())
	if err != nil {
		return err
	}
	return addConfiguredWorkdayWithManifest(plan, settings, clock, manifest)
}

func addConfiguredWorkdayWithManifest(plan *RuntimePlan, settings workday.Settings, clock automationtriggers.Clock, manifest registry.Manifest) error {
	if plan == nil || !settings.Valid() {
		return integrations.ErrInvalidInput
	}
	identity, ok := plan.Identity.(*registry.Repository)
	if !ok {
		return ErrIdentityNotConfigured
	}
	instance, err := workday.InstanceID()
	if err != nil {
		return err
	}
	factory, err := workday.NewPilotFactory(settings, clock)
	if err != nil {
		return err
	}
	spec, err := integrations.NewPilotSpec(instance, factory)
	if err != nil {
		return err
	}
	definitions, err := workday.ConfiguredBindings()
	if err != nil {
		return err
	}
	previous := plan.Configure
	plan.Configure = func(ctx context.Context, host *integrations.Manager, store *storage.Store) error {
		var calendarEntity = integrations.ResolvedIdentity{}
		for index, definition := range definitions {
			alias, err := definition.Alias().Value()
			if err != nil {
				return err
			}
			resolved, err := identity.ConfigureBinding(ctx, instance, definition.SourceKey(), alias)
			if err != nil {
				return err
			}
			if index == 1 {
				calendarEntity = resolved
			}
		}
		if previous != nil {
			if err := previous(ctx, host, store); err != nil {
				return err
			}
		}
		calendarRepository, err := calendars.NewRepository(store, manifest)
		if err != nil {
			return err
		}
		id, err := calendarRepository.Bind(ctx, calendarEntity.Entity)
		if err != nil {
			return err
		}
		reader, err := workday.NewCalendarReader(id, settings)
		if err != nil {
			return err
		}
		plan.WorkdayCalendar = reader
		return nil
	}
	plan.Pilots = append(plan.Pilots, spec)
	return nil
}
