package registry

import (
	"context"
	"sync/atomic"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
)

// BindingRequest is the same explicit source mapping as ConfigureBinding.
// Preparation reserves identities without writing SQL or discovering devices.
type BindingRequest struct {
	instance domain.IntegrationInstanceID
	key      integrations.SourceKey
	alias    domain.HAAlias
}

func NewBindingRequest(instance domain.IntegrationInstanceID, key integrations.SourceKey, alias string) (BindingRequest, error) {
	if _, _, _, err := sourceParts(instance, key); err != nil {
		return BindingRequest{}, ErrInvalidInput
	}
	parsed, err := ParseHAAlias(alias)
	if err != nil {
		return BindingRequest{}, err
	}
	return BindingRequest{instance: instance, key: key, alias: parsed}, nil
}

type preparedSource struct {
	instance domain.IntegrationInstanceID
	key      integrations.SourceKey
}
type preparedDevice struct {
	instance domain.IntegrationInstanceID
	source   string
}
type preparedBinding struct {
	request  BindingRequest
	identity integrations.ResolvedIdentity
}

// PreparedBindings is a bounded prospective SQL image, not a second identity
// authority. ConfigWriter admits exactly these IDs or rolls back on conflict.
// Consumers can therefore compile real energy/derived targets before COMMIT.
type PreparedBindings struct {
	repo     *Repository
	bindings []preparedBinding
	aliases  map[domain.HAAlias]Alias
	used     atomic.Bool
}

func (r *Repository) PrepareBindings(ctx context.Context, requests []BindingRequest) (*PreparedBindings, error) {
	if r == nil || nilContext(ctx) || len(requests) > 4096 {
		return nil, ErrInvalidInput
	}
	records, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	return r.prepareBindings(records, requests, true)
}

// CommittedBindings borrows a snapshot proved by the already retained
// activation gate. It cannot invent identities missing from committed SQL.
type CommittedBindings struct {
	repo    *Repository
	records []integrations.RegistryRecord
}

func (g *ActivationGate) CommittedBindings(ctx context.Context, target storage.ConfigGenerationRevision, manifest [32]byte) (*CommittedBindings, error) {
	records, err := g.CommittedRecords(ctx, target, manifest)
	if err != nil {
		return nil, err
	}
	return &CommittedBindings{repo: g.repo, records: records}, nil
}

func (p *CommittedBindings) PrepareBindings(requests []BindingRequest) (*PreparedBindings, error) {
	if p == nil || p.repo == nil {
		return nil, ErrInvalidInput
	}
	return p.repo.prepareBindings(p.records, requests, false)
}

func (r *Repository) prepareBindings(records []integrations.RegistryRecord, requests []BindingRequest, prospective bool) (*PreparedBindings, error) {
	if r == nil || len(requests) > 4096 {
		return nil, ErrInvalidInput
	}
	if len(records)+len(requests) > 8192 {
		return nil, ErrInvalidInput
	}
	p := &PreparedBindings{repo: r, aliases: make(map[domain.HAAlias]Alias, len(records)+len(requests))}
	existing := make(map[preparedSource]integrations.RegistryRecord, len(records))
	devices := make(map[preparedDevice]domain.DeviceID, len(records)+len(requests))
	for _, record := range records {
		device, entity, err := record.IDs()
		if err != nil {
			return nil, ErrStorage
		}
		instance, key, err := record.Source()
		if err != nil {
			return nil, ErrStorage
		}
		sourceDevice, _, _ := key.Components()
		existing[preparedSource{instance, key}] = record
		devices[preparedDevice{instance, sourceDevice}] = device
		_, current, aliases, _ := record.Aliases()
		for _, alias := range aliases {
			p.aliases[alias] = Alias{Entity: entity, Current: current == alias, Retired: record.Retired()}
		}
	}
	seen := make(map[preparedSource]bool, len(requests))
	for _, request := range requests {
		if _, _, _, err := sourceParts(request.instance, request.key); err != nil || !r.manifest.permits(request.alias) {
			return nil, ErrOutsideManifest
		}
		key := preparedSource{request.instance, request.key}
		if seen[key] {
			return nil, ErrBindingConflict
		}
		seen[key] = true
		var identity integrations.ResolvedIdentity
		if record, present := existing[key]; present {
			original, _, _, _ := record.Aliases()
			if record.Retired() {
				return nil, ErrTombstoned
			}
			if original != request.alias {
				return nil, ErrBindingConflict
			}
			identity.Device, identity.Entity, _ = record.IDs()
		} else {
			if !prospective {
				return nil, ErrUnconfigured
			}
			if _, present := p.aliases[request.alias]; present {
				return nil, ErrAliasConflict
			}
			sourceDevice, _, _ := request.key.Components()
			deviceKey := preparedDevice{request.instance, sourceDevice}
			identity.Device = devices[deviceKey]
			if _, err := identity.Device.Value(); err != nil {
				text, err := randomID("dev_")
				if err != nil {
					return nil, err
				}
				identity.Device, err = domain.ParseDeviceID(text)
				if err != nil {
					return nil, err
				}
				devices[deviceKey] = identity.Device
			}
			text, err := randomID("ent_")
			if err != nil {
				return nil, err
			}
			identity.Entity, err = domain.ParseEntityID(text)
			if err != nil {
				return nil, err
			}
			p.aliases[request.alias] = Alias{Entity: identity.Entity, Current: true}
		}
		p.bindings = append(p.bindings, preparedBinding{request: request, identity: identity})
	}
	return p, nil
}

func (p *PreparedBindings) LookupAlias(label string) (Alias, error) {
	if p == nil || p.repo == nil {
		return Alias{}, ErrInvalidInput
	}
	alias, err := ParseHAAlias(label)
	if err != nil {
		return Alias{}, err
	}
	value, present := p.aliases[alias]
	if !present {
		return Alias{}, ErrUnconfigured
	}
	return value, nil
}

func (w *ConfigWriter) ApplyPreparedBindings(p *PreparedBindings) error {
	if err := w.valid(); err != nil {
		return err
	}
	if p == nil || p.repo != w.repo || !p.used.CompareAndSwap(false, true) {
		return w.remember(ErrInvalidInput)
	}
	for _, binding := range p.bindings {
		instance, device, entity, err := sourceParts(binding.request.instance, binding.request.key)
		if err != nil {
			return w.remember(err)
		}
		alias, err := binding.request.alias.Value()
		if err != nil {
			return w.remember(err)
		}
		var actual integrations.ResolvedIdentity
		if err := configureBindingReservedSQL(w.sql, instance, device, entity, alias, &actual, &binding.identity); err != nil {
			return w.remember(err)
		}
	}
	return nil
}
