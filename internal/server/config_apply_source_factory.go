package server

import (
	"context"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/mqtt"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/solar"
)

// The same fixed factory consumes a live bounded capture or its historical
// generation-bound encoding. It never registers historical capabilities in
// the live Host/catalogue and never constructs a command authority.
func defaultSourceRuntimeFactory(manifest registry.Manifest, repository *registry.Repository, remote *remoteCalendarRuntime) configactivation.SourceRuntimeFactory {
	return func(ctx context.Context, input configcompilation.Context, operational config.Config) (*configactivation.SourceRuntimeCompiler, error) {
		catalogue, records, err := historicalSourceCompilationInputs(ctx, input, manifest)
		if err != nil {
			return nil, err
		}
		calendars := map[domain.HAAlias]automationtriggers.CalendarSourceID{}
		if _, enabled := operational.RemoteCalendarSettings(); enabled {
			if remote == nil || remote.bridge == nil || !remote.bridge.source.Valid() {
				return nil, ErrInvalidTriggerComposition
			}
			alias, err := domain.ParseHAAlias(remoteCalendarAlias)
			if err != nil {
				return nil, err
			}
			calendars[alias] = remote.bridge.source
		}
		resolver, err := sourceRuntimeResolverFromRecords(records, manifest, calendars)
		if err != nil {
			return nil, err
		}
		// UTC is the existing default Run template/script environment. Explicit
		// solar settings retain their actual admitted trigger environment.
		environment := automationtriggers.PlanInput{Zone: "UTC", Resolver: resolver}
		if settings, enabled := operational.MQTTSettings(); enabled {
			subscriptions, err := settings.Subscriptions()
			if err != nil {
				return nil, err
			}
			proof, err := mqtt.NewTriggerEdgePilot(settings, repository, subscriptions)
			if err != nil {
				return nil, err
			}
			environment.MQTTSettings, environment.MQTTIngress = settings, proof
		}
		if settings, enabled := operational.SolarSettings(); enabled {
			environment, err = solar.ConfigureTriggerInput(settings, environment)
			if err != nil {
				return nil, err
			}
		}
		return configactivation.NewSourceRuntimeCompiler(manifest, catalogue, environment)
	}
}

// Replace the one configured MQTT factory with the proof-backed version. It
// owns the same instance and existing pilot slot, never a second MQTT source.
func installSourceMQTTPilot(plan *RuntimePlan) error {
	proof, present := plan.AutomationTriggers.MQTTIngress()
	if !present {
		return nil
	}
	instance, err := proof.Instance()
	if err != nil || !proof.Covers(instance, plan.AutomationTriggers.MQTTFilters()) {
		return ErrInvalidTriggerComposition
	}
	spec, err := proof.PilotSpec()
	if err != nil {
		return err
	}
	found := false
	for index, prior := range plan.Pilots {
		id, err := prior.Instance()
		if err != nil {
			return err
		}
		if id == instance {
			if found {
				return ErrInvalidTriggerComposition
			}
			plan.Pilots[index], found = spec, true
		}
	}
	if !found {
		plan.Pilots = append(plan.Pilots, spec)
	}
	return nil
}
