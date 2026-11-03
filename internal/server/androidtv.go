package server

import (
	"context"
	"github.com/damione1/Homeward/internal/androidtv"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
)

func addConfiguredAndroidTV(plan *RuntimePlan, settings androidtv.Settings) error {
	if plan == nil || plan.androidTVRuntime == nil {
		return androidtv.ErrSettings
	}
	repository, ok := plan.Identity.(*registry.Repository)
	if !ok {
		return ErrIdentityNotConfigured
	}
	instance, e := settings.Instance()
	if e != nil {
		return e
	}
	factory, e := plan.androidTVRuntime.Factory(settings)
	if e != nil {
		return e
	}
	spec, e := integrations.NewPilotSpec(instance, factory)
	if e != nil {
		return e
	}
	previous := plan.Configure
	plan.Configure = func(ctx context.Context, host *integrations.Manager, store *storage.Store) error {
		for _, p := range settings.Players() {
			if _, e := repository.ConfigureBinding(ctx, instance, p.SourceKey(), p.Alias); e != nil {
				return e
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
