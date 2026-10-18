package registry

import (
	"context"
	"database/sql"
	"reflect"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/jackc/pgx/v5"
)

// List reads the registry's durable identity and alias authority in one SQL
// transaction. It is for loading a projection, never for serving live pages.
func (r *Repository) List(ctx context.Context) ([]integrations.RegistryRecord, error) {
	if r == nil {
		return nil, ErrInvalidInput
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.listUnlocked(ctx)
}

func (r *Repository) listUnlocked(ctx context.Context) ([]integrations.RegistryRecord, error) {
	var records []integrations.RegistryRecord
	err := r.transaction(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT e.id,e.device_id,e.manifest_alias,e.tombstoned,b.instance_id,b.source_device,b.source_entity,a.alias,a.current
			FROM public.registry_entities e
			LEFT JOIN public.registry_source_bindings b ON b.entity_id=e.id
			LEFT JOIN public.registry_aliases a ON a.entity_id=e.id
			ORDER BY e.id,a.alias`)
		if err != nil {
			return err
		}
		defer rows.Close()
		type pendingRecord struct {
			device   domain.DeviceID
			entity   domain.EntityID
			instance domain.IntegrationInstanceID
			key      integrations.SourceKey
			original domain.HAAlias
			current  domain.HAAlias
			aliases  []domain.HAAlias
			retired  bool
		}
		var pending *pendingRecord
		finish := func() error {
			if pending == nil {
				return nil
			}
			record, err := integrations.NewRegistryRecord(pending.device, pending.entity, pending.instance, pending.key, pending.original, pending.current, pending.aliases, pending.retired)
			if err != nil {
				return ErrStorage
			}
			records = append(records, record)
			return nil
		}
		for rows.Next() {
			var entityText, deviceText, originalText string
			var instanceText, sourceDevice, sourceEntity, aliasText sql.NullString
			var retired bool
			var current sql.NullBool
			if err := rows.Scan(&entityText, &deviceText, &originalText, &retired, &instanceText, &sourceDevice, &sourceEntity, &aliasText, &current); err != nil {
				return err
			}
			if !instanceText.Valid || !sourceDevice.Valid || !sourceEntity.Valid || !aliasText.Valid || !current.Valid {
				return ErrStorage
			}
			entity, err := domain.ParseEntityID(entityText)
			if err != nil {
				return ErrStorage
			}
			if pending == nil || pending.entity != entity {
				if err := finish(); err != nil {
					return err
				}
				device, err := domain.ParseDeviceID(deviceText)
				if err != nil {
					return ErrStorage
				}
				instance, err := domain.ParseIntegrationInstanceID(instanceText.String)
				if err != nil {
					return ErrStorage
				}
				key, err := integrations.NewSourceKey(sourceDevice.String, sourceEntity.String)
				if err != nil {
					return ErrStorage
				}
				original, err := ParseHAAlias(originalText)
				if err != nil || !r.manifest.permits(original) {
					return ErrOutsideManifest
				}
				pending = &pendingRecord{device: device, entity: entity, instance: instance, key: key, original: original, retired: retired}
			}
			alias, err := ParseHAAlias(aliasText.String)
			if err != nil {
				return ErrStorage
			}
			pending.aliases = append(pending.aliases, alias)
			if current.Bool {
				if pending.current != (domain.HAAlias{}) {
					return ErrStorage
				}
				pending.current = alias
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return finish()
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

// AttachProjection loads SQL before publishing to the sequencer. All subsequent
// repository writes publish their committed image before another Resolve returns.
func (r *Repository) AttachProjection(ctx context.Context, sink integrations.ProjectionSink) error {
	if r == nil || ctx == nil || sink == nil || (reflect.ValueOf(sink).Kind() == reflect.Pointer && reflect.ValueOf(sink).IsNil()) {
		return ErrInvalidInput
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.projection != nil {
		return ErrInvalidInput
	}
	records, err := r.listUnlocked(ctx)
	if err != nil {
		return err
	}
	if err := sink.PublishRegistry(ctx, records); err != nil {
		return err
	}
	r.projection = sink
	r.projectionDirty = false
	return nil
}

// ReloadProjection reconciles an indeterminate commit or a failed post-commit
// load. It requires the authoritative SQL store to be available again.
func (r *Repository) ReloadProjection(ctx context.Context) error {
	if r == nil || ctx == nil {
		return ErrInvalidInput
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.projection == nil {
		return ErrInvalidInput
	}
	records, err := r.listUnlocked(ctx)
	if err != nil {
		return err
	}
	if err := r.projection.PublishRegistry(ctx, records); err != nil {
		return err
	}
	r.projectionDirty = false
	return nil
}
