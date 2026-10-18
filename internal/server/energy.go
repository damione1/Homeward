package server

import (
	"context"
	"fmt"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/energy"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
)

// configuredEnergySpecs is called after the runtime's source bindings are
// configured and before Supervisor.Start. It resolves every alias through the
// registry. The three MVP rows are data supplied by config, not engine cases.
func configuredEnergySpecs(ctx context.Context, c config.Config, store *storage.Store, sequence *sequencer.Sequencer) ([]integrations.PilotSpec, error) {
	rows := c.EnergyBindings()
	if len(rows) == 0 {
		return nil, nil
	}
	manifest, err := registry.ParseManifest(scope.ManifestV1())
	if err != nil {
		return nil, err
	}
	identities, err := registry.NewRepository(store, manifest)
	if err != nil {
		return nil, err
	}
	instance, err := domain.ParseIntegrationInstanceID("homeward-energy-v1")
	if err != nil {
		return nil, err
	}
	unit, err := domain.NewUnitCatalog().Parse("kWh")
	if err != nil {
		return nil, err
	}
	bindings := make([]energy.OutputBinding, 0, len(rows))
	for _, row := range rows {
		source, err := identities.LookupAlias(ctx, row.SourceAlias())
		if err != nil {
			return nil, fmt.Errorf("energy: source alias %s: %w", row.SourceAlias(), err)
		}
		if !source.Current || source.Retired {
			return nil, fmt.Errorf("energy: source alias is not current: %s", row.SourceAlias())
		}
		key, err := integrations.NewSourceKey("computed-energy", row.OutputAlias())
		if err != nil {
			return nil, err
		}
		if _, err = identities.ConfigureBinding(ctx, instance, key, row.OutputAlias()); err != nil {
			return nil, err
		}
		output, err := identities.LookupAlias(ctx, row.OutputAlias())
		if err != nil {
			return nil, fmt.Errorf("energy: output alias %s: %w", row.OutputAlias(), err)
		}
		if !output.Current || output.Retired {
			return nil, fmt.Errorf("energy: output alias is not current: %s", row.OutputAlias())
		}
		profile, err := energy.NewProfile("mvp-trapezoidal-v1", 1, source.Entity, output.Entity, energy.MethodTrapezoidal, row.TimeBasis(), unit, row.Precision(), domain.SemanticTotal, row.MaxGap(), 0)
		if err != nil {
			return nil, err
		}
		binding, err := energy.NewOutputBinding(profile, key)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	if err := identities.AttachProjection(ctx, sequence); err != nil {
		return nil, err
	}
	counters, err := energy.NewRepository(store)
	if err != nil {
		return nil, err
	}
	factory, err := energy.NewPilotFactory(sequence, counters, bindings, nil, energy.RealClock{Sequence: sequence})
	if err != nil {
		return nil, err
	}
	spec, err := integrations.NewPilotSpec(instance, factory)
	if err != nil {
		return nil, err
	}
	return []integrations.PilotSpec{spec}, nil
}
