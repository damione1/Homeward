package server

import (
	"context"

	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/helpers"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
)

// Active configuration is selected elsewhere. This composition only projects
// its admitted helper definitions into the existing registry and Host.
func addActiveHelpers(plan *RuntimePlan, candidate bundle.Candidate, store *storage.Store) error {
	if plan == nil || store == nil || !candidate.Valid() {
		return integrations.ErrInvalidInput
	}
	definitions, err := helpers.Definitions(candidate)
	if err != nil {
		return err
	}
	if len(definitions) == 0 {
		return nil
	}
	identity, ok := plan.Identity.(*registry.Repository)
	if !ok {
		return ErrIdentityNotConfigured
	}
	policy, err := scope.RetainedHelperButtonPolicy()
	if err != nil {
		return err
	}
	repository, err := helpers.NewRepository(store)
	if err != nil {
		return err
	}
	factory, err := helpers.NewPilotFactory(repository, definitions, policy)
	if err != nil {
		return err
	}
	instance, err := domain.ParseIntegrationInstanceID("homeward-helpers-v1")
	if err != nil {
		return err
	}
	spec, err := integrations.NewPilotSpec(instance, factory)
	if err != nil {
		return err
	}
	plan.Pilots = append(plan.Pilots, spec)
	previous := plan.Configure
	plan.Configure = func(ctx context.Context, host *integrations.Manager, store *storage.Store) error {
		for _, definition := range definitions {
			alias, err := definition.Alias()
			if err != nil {
				return err
			}
			text, _ := alias.Value()
			key, err := helpers.ConfiguredSourceKey(alias)
			if err != nil {
				return err
			}
			if _, err := identity.ConfigureBinding(ctx, instance, key, text); err != nil {
				return err
			}
		}
		if previous != nil {
			return previous(ctx, host, store)
		}
		return nil
	}
	return nil
}
