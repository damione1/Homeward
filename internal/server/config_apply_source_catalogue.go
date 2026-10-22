package server

import (
	"context"
	"fmt"
	"sort"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/contracts"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"google.golang.org/protobuf/proto"
)

// newSourceSemanticCatalogue takes a compilation snapshot from the existing
// durable identity and admitted capability authorities. It never reads a client
// source to manufacture a capability. Dispatch still resolves identity, live
// binding, generation, technical limits and the internal execution boundary.
func newSourceSemanticCatalogue(ctx context.Context, repository *registry.Repository, host *integrations.Manager, catalog *contracts.CapabilityCatalog, manifest registry.Manifest) (ir.Catalogue, error) {
	if ctx == nil || repository == nil || host == nil || catalog == nil || !catalog.Ready() {
		return ir.Catalogue{}, ir.ErrInvalidCatalogue
	}
	actual, err := repository.ScopeManifest()
	if err != nil {
		return ir.Catalogue{}, err
	}
	digest, valid := manifest.Digest()
	actualDigest, actualValid := actual.Digest()
	revision, revisionValid := manifest.Revision()
	actualRevision, actualRevisionValid := actual.Revision()
	if !valid || !actualValid || !revisionValid || !actualRevisionValid || digest != actualDigest || revision != actualRevision {
		return ir.Catalogue{}, ir.ErrInvalidCatalogue
	}
	records, err := repository.List(ctx)
	if err != nil {
		return ir.Catalogue{}, err
	}
	return sourceSemanticCatalogueRecords(ctx, records, host.Capabilities, catalog, manifest)
}

type sourceSemanticCapabilities func(domain.EntityID) ([]*wireregistry.CapabilityDescription, error)

// The record input is the Repository.List projection, not a second registry.
// The function port lets tests supply declarations without starting a pilot.
// HA device IDs, events and scenes have no correspondence proved by these
// records, so their namespaces remain absent instead of being guessed from a
// SourceKey, an alias domain, or a client YAML definition.
func sourceSemanticCatalogueRecords(ctx context.Context, records []integrations.RegistryRecord, read sourceSemanticCapabilities, catalog *contracts.CapabilityCatalog, manifest registry.Manifest) (ir.Catalogue, error) {
	if ctx == nil || read == nil || catalog == nil || !catalog.Ready() {
		return ir.Catalogue{}, ir.ErrInvalidCatalogue
	}
	if _, valid := manifest.Revision(); !valid {
		return ir.Catalogue{}, ir.ErrInvalidCatalogue
	}
	entities := make([]ir.Entity, 0, len(records))
	seenEntities := make(map[domain.EntityID]struct{}, len(records))
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return ir.Catalogue{}, err
		}
		_, entity, err := record.IDs()
		if err != nil {
			return ir.Catalogue{}, ir.ErrInvalidCatalogue
		}
		if _, duplicate := seenEntities[entity]; duplicate {
			return ir.Catalogue{}, ir.ErrInvalidCatalogue
		}
		seenEntities[entity] = struct{}{}
		if _, _, err := record.Source(); err != nil {
			return ir.Catalogue{}, ir.ErrInvalidCatalogue
		}
		original, _, aliases, err := record.Aliases()
		if err != nil {
			return ir.Catalogue{}, ir.ErrInvalidCatalogue
		}
		// Provenance follows the immutable original, including after a rename.
		source, included := manifest.SourceFor(original)
		if _, valid := source.Value(); !included || !valid {
			return ir.Catalogue{}, ir.ErrInvalidCatalogue
		}
		status := ir.ReferencePresent
		var supports []ir.DeclaredSupport
		if record.Retired() {
			status = ir.ReferenceExcluded
		} else {
			descriptions, err := read(entity)
			if err != nil {
				return ir.Catalogue{}, err
			}
			admitted, err := catalog.Descriptions(entity)
			if err != nil {
				return ir.Catalogue{}, err
			}
			if !sameSourceSemanticCapabilities(descriptions, admitted) {
				// A changed discovery, or an unrelated injected catalogue, cannot
				// manufacture a larger compilation capability set.
				return ir.Catalogue{}, ir.ErrInvalidCatalogue
			}
			supports, err = sourceSemanticSupports(catalog, admitted)
			if err != nil {
				return ir.Catalogue{}, err
			}
		}
		for _, alias := range aliases {
			label, err := alias.Value()
			if err != nil {
				return ir.Catalogue{}, ir.ErrInvalidCatalogue
			}
			item, err := ir.NewEntity(label, status, supports...)
			if err != nil {
				return ir.Catalogue{}, err
			}
			entities = append(entities, item)
		}
	}
	if err := ctx.Err(); err != nil {
		return ir.Catalogue{}, err
	}
	sort.Slice(entities, func(i, j int) bool {
		left, _ := entities[i].Alias().Value()
		right, _ := entities[j].Alias().Value()
		return left < right
	})
	return ir.NewCatalogue(entities, nil, nil)
}

func sameSourceSemanticCapabilities(left, right []*wireregistry.CapabilityDescription) bool {
	if len(left) != len(right) {
		return false
	}
	type key struct{ schema, endpoint string }
	indexed := make(map[key]*wireregistry.CapabilityDescription, len(left))
	for _, description := range left {
		if description == nil {
			return false
		}
		id := key{description.SchemaId, description.Endpoint}
		if _, duplicate := indexed[id]; duplicate {
			return false
		}
		indexed[id] = description
	}
	for _, description := range right {
		if description == nil {
			return false
		}
		id := key{description.SchemaId, description.Endpoint}
		if candidate, found := indexed[id]; !found || !proto.Equal(candidate, description) {
			return false
		}
		delete(indexed, id)
	}
	return len(indexed) == 0
}

func sourceSemanticSupports(catalog *contracts.CapabilityCatalog, descriptions []*wireregistry.CapabilityDescription) ([]ir.DeclaredSupport, error) {
	if catalog == nil || !catalog.Ready() {
		return nil, ir.ErrInvalidCatalogue
	}
	schemaCount := make(map[string]int, len(descriptions))
	for _, description := range descriptions {
		if err := catalog.AdmitDriverCapability(description); err != nil {
			return nil, fmt.Errorf("%w: %w", ir.ErrInvalidCatalogue, err)
		}
		schemaCount[description.SchemaId]++
	}
	set := make(map[ir.DeclaredSupport]struct{})
	for _, description := range descriptions {
		// These are explicit conversions of admitted core contracts. Unknown
		// extensions supply neither observation nor action support to HA IR.
		switch constraints := description.Constraints.(type) {
		case *wireregistry.CapabilityDescription_Switch:
			set[ir.SupportObserveState] = struct{}{}
			if schemaCount[description.SchemaId] == 1 && constraints.Switch.SetOn {
				set[ir.SupportSwitchTurnOn] = struct{}{}
			}
		case *wireregistry.CapabilityDescription_Light:
			set[ir.SupportObserveState] = struct{}{}
			if schemaCount[description.SchemaId] != 1 {
				continue
			}
			if constraints.Light.SetOn {
				set[ir.SupportLightTurnOn] = struct{}{}
				set[ir.SupportLightTurnOff] = struct{}{}
			}
			if constraints.Light.BrightnessPercent != nil {
				set[ir.SupportLightBrightnessAbsolute] = struct{}{}
			}
			if constraints.Light.TransitionSeconds != nil {
				set[ir.SupportLightTransition] = struct{}{}
			}
		case *wireregistry.CapabilityDescription_Climate:
			climate := constraints.Climate
			// Climate command limits and observation declarations are separate
			// in the authoritative contract (AdmitDomainClimate).
			if climate.ObservedTemperatureCelsius != nil || climate.ObservedSetpointCelsius != nil || climate.ObservedSetpointRangeCelsius != nil || len(climate.ObservedModeOptions) != 0 || len(climate.ActivityOptions) != 0 || climate.ObservesFanActivity || climate.ObservedHumidityPercent != nil || len(climate.ObservedFanTargetOptions) != 0 {
				set[ir.SupportObserveState] = struct{}{}
			}
			if schemaCount[description.SchemaId] == 1 && constraints.Climate.SetpointCelsius != nil {
				set[ir.SupportClimateSetpoint] = struct{}{}
			}
		case *wireregistry.CapabilityDescription_Fan,
			*wireregistry.CapabilityDescription_Measurement,
			*wireregistry.CapabilityDescription_Binary,
			*wireregistry.CapabilityDescription_Helper,
			*wireregistry.CapabilityDescription_Solar,
			*wireregistry.CapabilityDescription_SolarEvent:
			set[ir.SupportObserveState] = struct{}{}
		case *wireregistry.CapabilityDescription_Extension:
			// No extension-to-HA semantic conversion is declared.
		default:
			return nil, ir.ErrInvalidCatalogue
		}
	}
	// Relative brightness needs a qualified Host conversion profile, which the
	// public declaration does not expose. It is deliberately absent here.
	order := []ir.DeclaredSupport{ir.SupportObserveState, ir.SupportClimateSetpoint, ir.SupportSwitchTurnOn, ir.SupportLightTurnOn, ir.SupportLightTurnOff, ir.SupportLightBrightnessAbsolute, ir.SupportLightTransition}
	result := make([]ir.DeclaredSupport, 0, len(set))
	for _, support := range order {
		if _, included := set[support]; included {
			result = append(result, support)
		}
	}
	return result, nil
}
