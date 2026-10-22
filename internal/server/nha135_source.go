package server

import (
	"context"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/derivedpower"
	"github.com/damione1/Homeward/internal/ecobee"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/scope"
)

// NHA-135 applies the separately pinned NHA-137 amendment, whose technical
// consumers explicitly admit climate.home -> HVAC estimated W -> kWh. It does
// not change the baseline or amendment, create an alias, or qualify a provider
// by its alias. The existing Ecobee settings select the NHA-009/136 profile.
func configuredHVACSource(ctx context.Context, selected config.Config, repository *registry.Repository) (derivedpower.HVACSource, error) {
	settings, enabled := selected.EcobeeSettings()
	if !enabled {
		return derivedpower.HVACSource{}, nil
	}
	manifest, err := repository.ScopeManifest()
	if err != nil {
		return derivedpower.HVACSource{}, err
	}
	if revision, valid := manifest.Revision(); !valid || revision != scope.EcobeeMVPV1() {
		return derivedpower.HVACSource{}, registry.ErrOutsideManifest
	}
	instance, err := settings.Instance()
	if err != nil {
		return derivedpower.HVACSource{}, err
	}
	identities, err := repository.Resolve(ctx, instance, []integrations.SourceKey{settings.SourceKey()})
	if err != nil {
		return derivedpower.HVACSource{}, err
	}
	alias, err := repository.LookupAlias(ctx, ecobee.Alias)
	if err != nil {
		return derivedpower.HVACSource{}, err
	}
	if len(identities) != 1 || !alias.Current || alias.Retired || alias.Entity != identities[0].Entity {
		return derivedpower.HVACSource{}, ErrDerivedPowerComposition
	}
	return derivedpower.NewHVACSource(alias.Entity)
}

func preparedHVACSource(selected config.Config, manifest registry.Manifest, bindings *registry.PreparedBindings) (derivedpower.HVACSource, error) {
	if _, enabled := selected.EcobeeSettings(); !enabled {
		return derivedpower.HVACSource{}, nil
	}
	if revision, valid := manifest.Revision(); !valid || revision != scope.EcobeeMVPV1() {
		return derivedpower.HVACSource{}, registry.ErrOutsideManifest
	}
	// PrepareOperational already requested this alias with exactly the selected
	// Ecobee instance/key. PreparedBindings refuses any competing binding and
	// ConfigWriter commits exactly that prospective identity under its gate.
	alias, err := bindings.LookupAlias(ecobee.Alias)
	if err != nil {
		return derivedpower.HVACSource{}, err
	}
	if !alias.Current || alias.Retired {
		return derivedpower.HVACSource{}, ErrDerivedPowerComposition
	}
	return derivedpower.NewHVACSource(alias.Entity)
}
