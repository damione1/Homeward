package server

import (
	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/heatingownership"
	"github.com/damione1/Homeward/internal/hilo"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
	"reflect"
)

func initialSourceOwnership(admitted configactivation.AdmittedGeneration, manifest registry.Manifest) (heatingownership.Decision, error) {
	decision, present, err := admitted.OwnershipDecision(manifest)
	if err != nil {
		return heatingownership.Decision{}, err
	}
	if present {
		return decision, nil
	}
	return heatingownership.ParseForManifest(scope.HeatingOwnershipV1(), manifest)
}

func sameSourceOwnership(left, right heatingownership.Decision) bool {
	return reflect.DeepEqual(left, right)
}

// Both initial and replacement Hilo factories use this canonical constructor.
// The source decision remains immutable; no live Host capability is invented.
func newHiloCompositionWithDecision(manifest registry.Manifest, decision heatingownership.Decision, settings hilo.Settings, store *storage.Store, sequence *sequencer.Sequencer, catalog *contracts.CapabilityCatalog, repository *registry.Repository) (*hiloComposition, error) {
	if !settings.Valid() || store == nil || sequence == nil || catalog == nil || repository == nil || !sourceOwnershipWithinManifest(decision, manifest) {
		return nil, hilo.ErrSettings
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

func sourceOwnershipWithinManifest(decision heatingownership.Decision, manifest registry.Manifest) bool {
	if decision.HiloActionPolicy() != heatingownership.ActionDenied || decision.HeatingOwner() != heatingownership.ProviderNeviweb || decision.EnergyRelation() != heatingownership.EnergyIndependent || len(decision.Pairs()) != 9 {
		return false
	}
	if _, valid := manifest.Revision(); !valid {
		return false
	}
	for _, pair := range decision.Pairs() {
		hiloAlias, neviwebAlias := pair.Aliases()
		hSource, hPresent := manifest.SourceFor(hiloAlias)
		nSource, nPresent := manifest.SourceFor(neviwebAlias)
		hName, hValid := hSource.Value()
		nName, nValid := nSource.Value()
		if !hPresent || !nPresent || !hValid || !nValid || hName != "hilo" || nName != "neviweb130" {
			return false
		}
	}
	return true
}
