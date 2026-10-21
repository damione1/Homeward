package server

import (
	"context"
	"fmt"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/energy"
	"github.com/damione1/Homeward/internal/heatingownership"
	"github.com/damione1/Homeward/internal/hilo"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
)

// hiloComposition is Run-owned. The provider cannot read SQL credentials or
// invoke authorization; the NHA-177 coordinator receives an explicit input.
type hiloComposition struct {
	settings   hilo.Settings
	repository *registry.Repository
	decision   heatingownership.Decision
	catalog    *contracts.CapabilityCatalog
	sequence   *sequencer.Sequencer
	store      *storage.Store
	policy     *integrations.CloudPolicy
	quota      *storage.CloudAttemptQuota
	leases     *hilo.LeaseRegistry
}

func newRunUnitCatalog(hiloEnabled bool) (*domain.UnitCatalog, error) {
	units := domain.NewUnitCatalog()
	if hiloEnabled {
		if err := units.Register("{notification}", domain.DimensionCount, 1, 0); err != nil {
			return nil, err
		}
	}
	return units, nil
}

func newHiloComposition(settings hilo.Settings, store *storage.Store, sequence *sequencer.Sequencer, catalog *contracts.CapabilityCatalog, repository *registry.Repository) (*hiloComposition, error) {
	if !settings.Valid() || store == nil || sequence == nil || catalog == nil || repository == nil {
		return nil, hilo.ErrSettings
	}
	manifest, err := registry.ParseManifest(scope.ManifestV1())
	if err != nil {
		return nil, err
	}
	decision, err := heatingownership.Parse(scope.HeatingOwnershipV1(), manifest)
	if err != nil {
		return nil, err
	}
	instance, err := settings.Instance()
	if err != nil {
		return nil, err
	}
	limit, zone := settings.Budget()
	quota, err := storage.NewCloudAttemptQuota(store, settings.AccountText(), limit, zone)
	if err != nil {
		return nil, err
	}
	account, err := integrations.NewCloudAccountBinding(instance, settings.Account())
	if err != nil {
		return nil, err
	}
	policy, err := integrations.NewCloudPolicyWithStreams(integrations.DefaultCloudLimits(), integrations.DefaultCloudStreamLimits(), []integrations.CloudAccountBinding{account}, integrations.RealCloudClock{}, integrations.RealCloudTimer{}, integrations.RandomJitter{})
	if err != nil {
		return nil, err
	}
	if cells, err := sequence.ObservationCells(); err == nil {
		_ = policy.AttachObservation(cells)
	}
	leases, err := hilo.NewLeaseRegistry(sequence, instance, settings.LeaseWindow())
	if err != nil {
		return nil, err
	}
	return &hiloComposition{settings, repository, decision, catalog, sequence, store, policy, quota, leases}, nil
}

// augment only composes the Hilo-owned pilot. Run calls it before Host and
// Supervisor start, so the NHA-177 binder exists before the pilot may Bind.
func (h *hiloComposition) augment(plan *RuntimePlan) error {
	if h == nil || plan == nil {
		return hilo.ErrSettings
	}
	previousConfigure := plan.Configure
	plan.Configure = func(ctx context.Context, manager *integrations.Manager, store *storage.Store) error {
		instance, err := h.settings.Instance()
		if err != nil {
			return err
		}
		for _, binding := range h.settings.Bindings() {
			if _, err := h.repository.ConfigureBinding(ctx, instance, binding.SourceKey(), binding.Alias()); err != nil {
				return err
			}
		}
		energyInstance, err := domain.ParseIntegrationInstanceID("homeward-hilo-energy-v1")
		if err != nil {
			return err
		}
		for _, pair := range h.settings.EnergyPairs() {
			key, err := integrations.NewSourceKey("computed-hilo-energy", pair.OutputAlias)
			if err != nil {
				return err
			}
			if _, err := h.repository.ConfigureBinding(ctx, energyInstance, key, pair.OutputAlias); err != nil {
				return err
			}
		}
		if previousConfigure != nil {
			return previousConfigure(ctx, manager, store)
		}
		return h.repository.AttachProjection(ctx, h.sequence)
	}
	previousAuthorization := plan.ConfigureCloudAuthorization
	plan.ConfigureCloudAuthorization = func(ctx context.Context, composition CloudAuthorizationComposition) error {
		if previousAuthorization != nil {
			if err := previousAuthorization(ctx, composition); err != nil {
				return err
			}
		}
		instance, err := h.settings.Instance()
		if err != nil {
			return err
		}
		profile, err := integrations.ParseCloudAuthorizationProfileID("hilo-android-b2c-v1")
		if err != nil {
			return err
		}
		provider, err := hilo.NewAuthorizationProvider(profile)
		if err != nil {
			return err
		}
		registration, err := NewCloudAuthorizationRegistration(instance, h.settings.CredentialHandle(), profile, h.policy, provider, h.quota)
		if err != nil {
			return err
		}
		binder, err := composition.Register(registration)
		if err != nil {
			return err
		}
		factory, err := hilo.NewPilotFactory(h.settings, h.repository, h.decision, h.catalog, h.policy, h.quota, binder, h.leases)
		if err != nil {
			return err
		}
		spec, err := integrations.NewPilotSpec(instance, factory)
		if err != nil {
			return err
		}
		plan.Pilots = append(plan.Pilots, spec)
		return nil
	}
	return nil
}

// energySpec is called after configured Hilo bindings exist. It uses the
// delivered NHA-108 engine, with a distinct instance from the three generic
// trapezoidal counters. E10 never becomes a direct cloud measurement.
func (h *hiloComposition) energySpec(ctx context.Context) (integrations.PilotSpec, error) {
	if h == nil {
		return integrations.PilotSpec{}, hilo.ErrSettings
	}
	instance, err := domain.ParseIntegrationInstanceID("homeward-hilo-energy-v1")
	if err != nil {
		return integrations.PilotSpec{}, err
	}
	bindings := make([]energy.OutputBinding, 0, 10)
	for _, pair := range h.settings.EnergyPairs() {
		source, err := h.repository.LookupAlias(ctx, pair.SourceAlias)
		if err != nil || !source.Current || source.Retired {
			return integrations.PilotSpec{}, fmt.Errorf("hilo energy source unavailable: %s", pair.SourceAlias)
		}
		key, err := integrations.NewSourceKey("computed-hilo-energy", pair.OutputAlias)
		if err != nil {
			return integrations.PilotSpec{}, err
		}
		output, err := h.repository.LookupAlias(ctx, pair.OutputAlias)
		if err != nil || !output.Current || output.Retired {
			return integrations.PilotSpec{}, fmt.Errorf("hilo energy output unavailable: %s", pair.OutputAlias)
		}
		profile, err := energy.HiloLeftV1(source.Entity, output.Entity, h.settings.EnergyMaxGap())
		if err != nil {
			return integrations.PilotSpec{}, err
		}
		binding, err := energy.NewOutputBinding(profile, key)
		if err != nil {
			return integrations.PilotSpec{}, err
		}
		bindings = append(bindings, binding)
	}
	counters, err := energy.NewRepository(h.store)
	if err != nil {
		return integrations.PilotSpec{}, err
	}
	factory, err := energy.NewPilotFactory(h.sequence, counters, bindings, h.leases, energy.RealClock{Sequence: h.sequence})
	if err != nil {
		return integrations.PilotSpec{}, err
	}
	return integrations.NewPilotSpec(instance, factory)
}
