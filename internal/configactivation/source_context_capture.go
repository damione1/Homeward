package configactivation

import (
	"context"
	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
)

// SourceContextFromSnapshot projects only the already-admitted registry and
// capability declarations from one existing bounded Sequencer capture. State
// values, health, IR and command authority never enter durable compilation.
func SourceContextFromSnapshot(snapshot sequencer.Snapshot, manifest registry.Manifest) (configcompilation.Context, error) {
	if !snapshot.RegistryReady {
		return configcompilation.Context{}, ErrActivationUnavailable
	}
	revision, valid := manifest.Revision()
	if !valid {
		return configcompilation.Context{}, ErrInvalidStartup
	}
	name, err := revision.Value()
	if err != nil {
		return configcompilation.Context{}, err
	}
	digest, valid := manifest.Digest()
	if !valid {
		return configcompilation.Context{}, ErrInvalidStartup
	}
	identities := make([]configcompilation.Identity, 0, len(snapshot.Registry))
	for entity, record := range snapshot.Registry {
		device, registeredEntity, err := record.IDs()
		if err != nil || entity != registeredEntity {
			return configcompilation.Context{}, ErrStartupIntegrity
		}
		instance, key, err := record.Source()
		if err != nil {
			return configcompilation.Context{}, err
		}
		sourceDevice, sourceEntity, err := key.Components()
		if err != nil {
			return configcompilation.Context{}, err
		}
		original, current, aliases, err := record.Aliases()
		if err != nil {
			return configcompilation.Context{}, err
		}
		if _, present := manifest.SourceFor(original); !present {
			return configcompilation.Context{}, registry.ErrOutsideManifest
		}
		identities = append(identities, configcompilation.Identity{Device: device, Entity: entity, Instance: instance, SourceDevice: sourceDevice, SourceEntity: sourceEntity, Original: original, Current: current, Aliases: aliases, Retired: record.Retired()})
	}
	return configcompilation.NewContext(name, digest, identities, snapshot.Capabilities)
}

func (o *Owner) captureSourceContext(ctx context.Context) (configcompilation.Context, sequencer.Snapshot, error) {
	if o == nil || !usableContext(ctx) {
		return configcompilation.Context{}, sequencer.Snapshot{}, ErrInvalidStartup
	}
	manifest, err := o.input.Registry.ScopeManifest()
	if err != nil {
		return configcompilation.Context{}, sequencer.Snapshot{}, err
	}
	snapshot, err := o.input.Projection.CaptureBounded(sequencer.MaxJournalBytes)
	if err != nil {
		return configcompilation.Context{}, sequencer.Snapshot{}, err
	}
	compiled, err := SourceContextFromSnapshot(snapshot, manifest)
	return compiled, snapshot, err
}
