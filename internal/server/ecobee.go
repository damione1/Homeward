package server

import (
	"context"

	"github.com/damione1/Homeward/internal/ecobee"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
)

func addConfiguredEcobee(plan *RuntimePlan, settings ecobee.Settings, manifest registry.Manifest) error {
	if plan == nil || !settings.Valid() {
		return ecobee.ErrSettings
	}
	revision, ok := manifest.Revision()
	if !ok || revision != scope.EcobeeMVPV1() {
		return registry.ErrOutsideManifest
	}
	repository, ok := plan.Identity.(*registry.Repository)
	if !ok {
		return ErrIdentityNotConfigured
	}
	instance, err := settings.Instance()
	if err != nil {
		return err
	}
	factory, err := ecobee.NewPilotFactory(settings)
	if err != nil {
		return err
	}
	spec, err := integrations.NewPilotSpec(instance, factory)
	if err != nil {
		return err
	}
	previous := plan.Configure
	plan.Configure = func(ctx context.Context, host *integrations.Manager, store *storage.Store) error {
		if _, err := repository.ConfigureBinding(ctx, instance, settings.SourceKey(), ecobee.Alias); err != nil {
			return err
		}
		if previous != nil {
			return previous(ctx, host, store)
		}
		return nil
	}
	plan.Pilots = append(plan.Pilots, spec)
	return nil
}
