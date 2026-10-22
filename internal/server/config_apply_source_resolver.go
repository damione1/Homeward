package server

import (
	"context"
	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
)

// sourceRuntimeResolver is a detached registry snapshot, not a source-derived
// identity registry. Runtime admission still checks the current Host/DCS cut.
type sourceRuntimeResolver struct {
	entities  map[domain.HAAlias]domain.EntityID
	calendars map[domain.HAAlias]automationtriggers.CalendarSourceID
}

func newSourceRuntimeResolver(ctx context.Context, repository *registry.Repository, manifest registry.Manifest, remote *remoteCalendarRuntime, remoteEnabled bool) (sourceRuntimeResolver, error) {
	if repository == nil || !validRunContext(ctx) {
		return sourceRuntimeResolver{}, registry.ErrInvalidInput
	}
	records, err := repository.List(ctx)
	if err != nil {
		return sourceRuntimeResolver{}, err
	}
	calendars := map[domain.HAAlias]automationtriggers.CalendarSourceID{}
	if remoteEnabled {
		if remote == nil || remote.bridge == nil || !remote.bridge.source.Valid() {
			return sourceRuntimeResolver{}, automationtriggers.ErrInvalidPlan
		}
		alias, err := domain.ParseHAAlias(remoteCalendarAlias)
		if err != nil {
			return sourceRuntimeResolver{}, err
		}
		calendars[alias] = remote.bridge.source
	}
	return sourceRuntimeResolverFromRecords(records, manifest, calendars)
}
func sourceRuntimeResolverFromRecords(records []integrations.RegistryRecord, manifest registry.Manifest, calendars map[domain.HAAlias]automationtriggers.CalendarSourceID) (sourceRuntimeResolver, error) {
	if _, valid := manifest.Revision(); !valid {
		return sourceRuntimeResolver{}, registry.ErrOutsideManifest
	}
	result := sourceRuntimeResolver{entities: map[domain.HAAlias]domain.EntityID{}, calendars: map[domain.HAAlias]automationtriggers.CalendarSourceID{}}
	for _, record := range records {
		_, entity, err := record.IDs()
		if err != nil {
			return sourceRuntimeResolver{}, err
		}
		original, _, aliases, err := record.Aliases()
		if err != nil {
			return sourceRuntimeResolver{}, err
		}
		if _, included := manifest.SourceFor(original); !included {
			return sourceRuntimeResolver{}, registry.ErrOutsideManifest
		}
		if record.Retired() {
			continue
		}
		for _, alias := range aliases {
			if previous, present := result.entities[alias]; present && previous != entity {
				return sourceRuntimeResolver{}, registry.ErrAliasConflict
			}
			result.entities[alias] = entity
		}
	}
	for alias, source := range calendars {
		if _, included := manifest.SourceFor(alias); !included || !source.Valid() {
			return sourceRuntimeResolver{}, automationtriggers.ErrInvalidPlan
		}
		result.calendars[alias] = source
	}
	return result, nil
}
func (r sourceRuntimeResolver) ResolveEntity(alias domain.HAAlias) (domain.EntityID, error) {
	entity, present := r.entities[alias]
	if !present {
		return domain.EntityID{}, registry.ErrUnconfigured
	}
	return entity, nil
}
func (sourceRuntimeResolver) ResolveDevice(ir.HADeviceRef) (domain.DeviceID, error) {
	// There is no persisted HA-device-reference binding in the current registry.
	// A Homeward device UUID is never synthesized from an HA source string.
	return domain.DeviceID{}, registry.ErrUnconfigured
}
func (r sourceRuntimeResolver) ResolveCalendar(alias domain.HAAlias) (automationtriggers.CalendarSourceID, error) {
	source, present := r.calendars[alias]
	if !present {
		return automationtriggers.CalendarSourceID{}, registry.ErrUnconfigured
	}
	return source, nil
}

var _ automationtriggers.Resolver = sourceRuntimeResolver{}
