package server

import (
	"context"
	"errors"
	"time"

	"github.com/damione1/Homeward/internal/derivedpower"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/reactivetemplate"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
)

var ErrDerivedPowerComposition = errors.New("server: invalid derived power composition")

type derivedPowerConfiguration struct {
	bindings []derivedpower.Binding
}

// Reserve all three manifest aliases before the registry snapshot and the
// energy composition resolve their identities. The future Ecobee input is not
// reserved or mapped here; its missing read remains explicit.
func addConfiguredDerivedPower(plan *RuntimePlan) (*derivedPowerConfiguration, error) {
	if plan == nil || plan.Configure == nil {
		return nil, ErrDerivedPowerComposition
	}
	repository, ok := plan.Identity.(*registry.Repository)
	if !ok {
		return nil, ErrDerivedPowerComposition
	}
	configuration := &derivedPowerConfiguration{}
	previous := plan.Configure
	plan.Configure = func(ctx context.Context, host *integrations.Manager, store *storage.Store) error {
		instance, err := derivedpower.Instance()
		if err != nil {
			return err
		}
		bindings := make([]derivedpower.Binding, 0, 3)
		for _, kind := range derivedpower.Kinds() {
			alias, err := kind.OutputAlias()
			if err != nil {
				return err
			}
			key, err := kind.SourceKey()
			if err != nil {
				return err
			}
			identity, err := repository.ConfigureBinding(ctx, instance, key, alias)
			if err != nil {
				return err
			}
			binding, err := derivedpower.NewBinding(kind, identity.Entity)
			if err != nil {
				return err
			}
			bindings = append(bindings, binding)
		}
		if err := previous(ctx, host, store); err != nil {
			return err
		}
		configuration.bindings = bindings
		return nil
	}
	return configuration, nil
}

type derivedPowerOutput struct{ publisher *derivedpower.Publisher }

func (o derivedPowerOutput) PublishReactiveTemplate(ctx context.Context, _ *integrations.Manager, update reactivetemplate.Update) error {
	return o.publisher.Publish(ctx, update)
}

func (c *derivedPowerConfiguration) prepare(ctx context.Context, host *integrations.Manager, plan *RuntimePlan) error {
	if c == nil || len(c.bindings) != 3 || plan == nil {
		return ErrDerivedPowerComposition
	}
	definitions := make([]reactivetemplate.Definition, 0, len(c.bindings))
	for _, binding := range c.bindings {
		definition, err := derivedpower.Definition(binding.Kind(), binding.Target())
		if err != nil {
			return err
		}
		definitions = append(definitions, definition)
	}
	enginePlan, err := reactivetemplate.NewPlan(definitions, nil, time.UTC)
	if err != nil {
		return err
	}
	publisher, err := derivedpower.NewPublisher(c.bindings)
	if err != nil {
		return err
	}
	if err := publisher.Prepare(ctx, host); err != nil {
		return err
	}
	composition, err := composeDerivedReactiveTemplates(plan.ReactiveTemplates, enginePlan, derivedPowerOutput{publisher})
	if err != nil {
		return err
	}
	plan.ReactiveTemplates = composition
	return nil
}
