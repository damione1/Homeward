package server

import (
	"context"
	"sort"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
)

// historicalSourceCompilationInputs reconstructs only detached compilation
// inputs. Historical declarations never enter a live Host or capability
// catalogue, establish a session, or grant command ownership. Dispatch retains
// its live identity, capability, generation and internal execution checks.
func historicalSourceCompilationInputs(ctx context.Context, input configcompilation.Context, manifest registry.Manifest) (ir.Catalogue, []integrations.RegistryRecord, error) {
	if ctx == nil || !input.Valid() {
		return ir.Catalogue{}, nil, ir.ErrInvalidCatalogue
	}
	if err := ctx.Err(); err != nil {
		return ir.Catalogue{}, nil, err
	}
	revision, revisionValid := manifest.Revision()
	digest, digestValid := manifest.Digest()
	revisionText, revisionErr := revision.Value()
	if !revisionValid || revisionErr != nil || !digestValid || revisionText != input.ManifestRevision() || digest != input.ManifestSHA256() {
		return ir.Catalogue{}, nil, ir.ErrInvalidCatalogue
	}
	units, err := newRunUnitCatalog(true)
	if err != nil {
		return ir.Catalogue{}, nil, err
	}
	// This fresh catalogue supplies existing semantic validators only. No
	// driver declaration is registered, even into this detached validator.
	validation, err := contracts.NewCapabilityCatalog(units)
	if err != nil {
		return ir.Catalogue{}, nil, err
	}
	identities := input.Identities()
	capabilities := input.Capabilities()
	records := make([]integrations.RegistryRecord, 0, len(identities))
	entities := make([]ir.Entity, 0, len(identities))
	for _, identity := range identities {
		if err := ctx.Err(); err != nil {
			return ir.Catalogue{}, nil, err
		}
		// A historical or renamed alias retains its immutable source origin.
		source, included := manifest.SourceFor(identity.Original)
		if _, valid := source.Value(); !included || !valid {
			return ir.Catalogue{}, nil, ir.ErrInvalidCatalogue
		}
		key, err := integrations.NewSourceKey(identity.SourceDevice, identity.SourceEntity)
		if err != nil {
			return ir.Catalogue{}, nil, ir.ErrInvalidCatalogue
		}
		record, err := integrations.NewRegistryRecord(identity.Device, identity.Entity, identity.Instance, key, identity.Original, identity.Current, identity.Aliases, identity.Retired)
		if err != nil {
			return ir.Catalogue{}, nil, ir.ErrInvalidCatalogue
		}
		status := ir.ReferencePresent
		var supports []ir.DeclaredSupport
		if identity.Retired {
			status = ir.ReferenceExcluded
		} else {
			// An absent discovery remains absent: nil declarations add no
			// support. Re-admit every supplied declaration through the same
			// contracts validator and explicit conversion as live compilation.
			supports, err = sourceSemanticSupports(validation, capabilities[identity.Entity])
			if err != nil {
				return ir.Catalogue{}, nil, err
			}
		}
		for _, alias := range identity.Aliases {
			// Registry aliases cannot borrow another selected identity's
			// immutable manifest name, including in detached compilation.
			if alias != identity.Original {
				if _, reserved := manifest.SourceFor(alias); reserved {
					return ir.Catalogue{}, nil, ir.ErrInvalidCatalogue
				}
			}
			label, err := alias.Value()
			if err != nil {
				return ir.Catalogue{}, nil, ir.ErrInvalidCatalogue
			}
			entity, err := ir.NewEntity(label, status, supports...)
			if err != nil {
				return ir.Catalogue{}, nil, err
			}
			entities = append(entities, entity)
		}
		records = append(records, record)
	}
	if err := ctx.Err(); err != nil {
		return ir.Catalogue{}, nil, err
	}
	sort.Slice(entities, func(i, j int) bool {
		left, _ := entities[i].Alias().Value()
		right, _ := entities[j].Alias().Value()
		return left < right
	})
	catalogue, err := ir.NewCatalogue(entities, nil, nil)
	if err != nil {
		return ir.Catalogue{}, nil, err
	}
	return catalogue, records, nil
}
