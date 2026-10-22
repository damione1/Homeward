package server

import (
	"context"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/heatingownership"
	"github.com/damione1/Homeward/internal/reactivetemplate"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/scope"
)

func (c *configuredOperationalCompiler) PrepareOperationalSources(ctx context.Context, old, next config.Config, oldDefinitions, nextDefinitions bundle.Candidate, templates reactivetemplate.Plan, triggers automationtriggers.Plan, forced []domain.IntegrationInstanceID, committed *registry.CommittedBindings, sources configactivation.SourceCompilation, gate *registry.ActivationGate) (configactivation.OperationalImage, error) {
	if c == nil || !sources.Valid() {
		return configactivation.OperationalImage{}, configactivation.ErrOperationalComposition
	}
	candidate := *c
	candidate.sourceGate, candidate.sourceParent = gate, c
	candidate.sourceOwnership = nil
	decision, present := sources.Ownership()
	if !present {
		return candidate.PrepareOperational(ctx, old, next, oldDefinitions, nextDefinitions, templates, triggers, forced, committed)
	}
	candidate.sourceOwnership, candidate.sourceParent = &decision, c
	// Ownership changes both cloud factories even when operational YAML is equal.
	// The existing supervisor performs replacement after the one SQL COMMIT.
	forced = append([]domain.IntegrationInstanceID(nil), forced...)
	if settings, enabled := next.NeviwebSettings(); enabled {
		instance, err := settings.Instance()
		if err != nil {
			return configactivation.OperationalImage{}, err
		}
		forced = append(forced, instance)
	}
	if settings, enabled := next.HiloSettings(); enabled {
		instance, err := settings.Instance()
		if err != nil {
			return configactivation.OperationalImage{}, err
		}
		forced = append(forced, instance)
	}
	return candidate.PrepareOperational(ctx, old, next, oldDefinitions, nextDefinitions, templates, triggers, forced, committed)
}

var _ configactivation.SourceOperationalCompiler = (*configuredOperationalCompiler)(nil)

func (c *configuredOperationalCompiler) ownershipDecision() (heatingownership.Decision, error) {
	if c == nil {
		return heatingownership.Decision{}, configactivation.ErrOperationalComposition
	}
	if c.sourceOwnership != nil {
		if !sourceOwnershipWithinManifest(*c.sourceOwnership, c.manifest) {
			return heatingownership.Decision{}, heatingownership.ErrInvalidDecision
		}
		return *c.sourceOwnership, nil
	}
	return heatingownership.ParseForManifest(scope.HeatingOwnershipV1(), c.manifest)
}
