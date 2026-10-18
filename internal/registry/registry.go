package registry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrInvalidInput          = errors.New("invalid registry input")
	ErrOutsideManifest       = errors.New("identity is outside the configured manifest")
	ErrUnconfigured          = errors.New("source identity has no explicit binding")
	ErrTombstoned            = errors.New("identity was retired")
	ErrAliasConflict         = errors.New("HA alias belongs to another identity")
	ErrBindingConflict       = errors.New("source binding conflicts with configuration")
	ErrStorage               = errors.New("registry storage unavailable")
	ErrProjectionUnavailable = errors.New("registry projection requires reconciliation")
)

// storageFailure preserves typed causes for diagnosis without rendering SQL,
// credentials, or callback text in user-facing diagnostics.
type storageFailure struct{ cause error }

func (e *storageFailure) Error() string        { return ErrStorage.Error() }
func (e *storageFailure) GoString() string     { return e.Error() }
func (e *storageFailure) Unwrap() error        { return e.cause }
func (e *storageFailure) Is(target error) bool { return target == ErrStorage }

// Repository implements integrations.IdentityAuthority against the same Store
// migrated at server cold start. It owns durable configuration, not live sessions.
type Repository struct {
	store           *storage.Store
	manifest        Manifest
	mu              sync.RWMutex
	projection      integrations.ProjectionSink
	projectionDirty bool
}

var _ integrations.IdentityAuthority = (*Repository)(nil)

func NewRepository(store *storage.Store, manifest Manifest) (*Repository, error) {
	if store == nil || len(manifest.aliases) == 0 {
		return nil, ErrInvalidInput
	}
	return &Repository{store: store, manifest: manifest}, nil
}

func sourceParts(instance domain.IntegrationInstanceID, key integrations.SourceKey) (string, string, string, error) {
	instanceText, err := instance.Value()
	if err != nil {
		return "", "", "", ErrInvalidInput
	}
	device, entity, err := key.Components()
	if err != nil {
		return "", "", "", ErrInvalidInput
	}
	return instanceText, device, entity, nil
}

func randomID(prefix string) (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", ErrStorage
	}
	return prefix + hex.EncodeToString(bytes[:]), nil
}

func lock(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(481316, 15)`)
	return err
}

func classify(err error) error {
	if err == nil {
		return nil
	}
	for _, expected := range []error{ErrInvalidInput, ErrOutsideManifest, ErrUnconfigured, ErrTombstoned, ErrAliasConflict, ErrBindingConflict} {
		if errors.Is(err, expected) {
			return expected
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &storageFailure{cause: err}
}

func (r *Repository) transaction(ctx context.Context, fn func(pgx.Tx) error) error {
	_, err := r.transactionOutcome(ctx, fn)
	return err
}

func (r *Repository) transactionOutcome(ctx context.Context, fn func(pgx.Tx) error) (storage.TransactionOutcome, error) {
	if r == nil || r.store == nil || ctx == nil {
		return storage.TransactionNotCommitted, ErrInvalidInput
	}
	outcome, err := r.store.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		if err := lock(ctx, tx); err != nil {
			return err
		}
		return fn(tx)
	})
	return outcome, classify(err)
}

func (r *Repository) mutate(ctx context.Context, fn func(pgx.Tx) error) error {
	if r == nil {
		return ErrInvalidInput
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.projectionDirty {
		return ErrProjectionUnavailable
	}
	outcome, err := r.transactionOutcome(ctx, fn)
	if err != nil {
		if outcome == storage.TransactionIndeterminate && r.projection != nil {
			r.projectionDirty = true
			r.projection.InvalidateRegistry()
		}
		return err
	}
	if r.projection == nil {
		return nil
	}
	// Commit is now durable. The caller's cancellation cannot skip publication.
	background, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	records, err := r.listUnlocked(background)
	if err == nil {
		err = r.projection.PublishRegistry(background, records)
	}
	if err != nil {
		r.projectionDirty = true
		r.projection.InvalidateRegistry()
	}
	return err
}

// ConfigureBinding records an operator's explicit mapping from a stable source
// key to an original manifest identity. Discovery never calls this method.
// Repeating the same mapping is idempotent; changing it requires an explicit
// migration outside this ticket, so a source cannot silently take a new ID.
func (r *Repository) ConfigureBinding(ctx context.Context, instance domain.IntegrationInstanceID, key integrations.SourceKey, originalAlias string) (integrations.ResolvedIdentity, error) {
	var result integrations.ResolvedIdentity
	instanceText, sourceDevice, sourceEntity, err := sourceParts(instance, key)
	alias, aliasErr := ParseHAAlias(originalAlias)
	if err != nil || aliasErr != nil {
		return result, ErrInvalidInput
	}
	if r == nil || !r.manifest.permits(alias) {
		return result, ErrOutsideManifest
	}
	err = r.mutate(ctx, func(tx pgx.Tx) error {
		var deviceID, entityID, existingAlias string
		var retired bool
		err := tx.QueryRow(ctx, `SELECT d.id, e.id, e.manifest_alias, e.tombstoned
			FROM public.registry_source_bindings b
			JOIN public.registry_entities e ON e.id=b.entity_id
			JOIN public.registry_devices d ON d.id=e.device_id
			WHERE b.instance_id=$1 AND b.source_device=$2 AND b.source_entity=$3`, instanceText, sourceDevice, sourceEntity).Scan(&deviceID, &entityID, &existingAlias, &retired)
		if err == nil {
			if retired {
				return ErrTombstoned
			}
			if existingAlias != originalAlias {
				return ErrBindingConflict
			}
			return assign(&result, deviceID, entityID)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// A manifest alias has one identity even if its protocol source changes.
		err = tx.QueryRow(ctx, `SELECT id FROM public.registry_entities WHERE manifest_alias=$1`, originalAlias).Scan(&entityID)
		if err == nil {
			return ErrBindingConflict
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT device_id FROM public.registry_device_sources WHERE instance_id=$1 AND source_device=$2`, instanceText, sourceDevice).Scan(&deviceID)
		if errors.Is(err, pgx.ErrNoRows) {
			deviceID, err = randomID("dev_")
			if err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO public.registry_devices(id) VALUES ($1)`, deviceID); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO public.registry_device_sources(instance_id,source_device,device_id) VALUES ($1,$2,$3)`, instanceText, sourceDevice, deviceID); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		entityID, err = randomID("ent_")
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO public.registry_entities(id,device_id,manifest_alias) VALUES ($1,$2,$3)`, entityID, deviceID, originalAlias); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO public.registry_source_bindings(instance_id,source_device,source_entity,entity_id) VALUES ($1,$2,$3,$4)`, instanceText, sourceDevice, sourceEntity, entityID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO public.registry_aliases(alias,entity_id,current) VALUES ($1,$2,true)`, originalAlias, entityID); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return ErrAliasConflict
			}
			return err
		}
		return assign(&result, deviceID, entityID)
	})
	return result, err
}

func assign(result *integrations.ResolvedIdentity, deviceText, entityText string) error {
	device, err := domain.ParseDeviceID(deviceText)
	if err != nil {
		return ErrStorage
	}
	entity, err := domain.ParseEntityID(entityText)
	if err != nil {
		return ErrStorage
	}
	*result = integrations.ResolvedIdentity{Device: device, Entity: entity}
	return nil
}

// Resolve is one atomic batch read. It only admits configured, active sources
// whose original identity remains in the supplied manifest snapshot.
func (r *Repository) Resolve(ctx context.Context, instance domain.IntegrationInstanceID, keys []integrations.SourceKey) ([]integrations.ResolvedIdentity, error) {
	if r == nil {
		return nil, ErrInvalidInput
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.projectionDirty {
		return nil, ErrProjectionUnavailable
	}
	if len(keys) == 0 || len(keys) > 256 {
		return nil, ErrInvalidInput
	}
	instanceText, err := instance.Value()
	if err != nil {
		return nil, ErrInvalidInput
	}
	seen := make(map[integrations.SourceKey]bool, len(keys))
	for _, key := range keys {
		if _, _, err := key.Components(); err != nil || seen[key] {
			return nil, ErrInvalidInput
		}
		seen[key] = true
	}
	resolved := make([]integrations.ResolvedIdentity, len(keys))
	err = r.transaction(ctx, func(tx pgx.Tx) error {
		for i, key := range keys {
			device, entity, _ := key.Components()
			var deviceID, entityID, originalAlias string
			var retired bool
			err := tx.QueryRow(ctx, `SELECT e.device_id, e.id, e.manifest_alias, e.tombstoned
				FROM public.registry_source_bindings b JOIN public.registry_entities e ON e.id=b.entity_id
				WHERE b.instance_id=$1 AND b.source_device=$2 AND b.source_entity=$3`, instanceText, device, entity).Scan(&deviceID, &entityID, &originalAlias, &retired)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrUnconfigured
			}
			if err != nil {
				return err
			}
			if retired {
				return ErrTombstoned
			}
			alias, err := ParseHAAlias(originalAlias)
			if err != nil || !r.manifest.permits(alias) {
				return ErrOutsideManifest
			}
			if err := assign(&resolved[i], deviceID, entityID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resolved, nil
}

// Rename retains every former HA alias as a resolvable historical reference.
func (r *Repository) Rename(ctx context.Context, entity domain.EntityID, alias string) error {
	entityText, err := entity.Value()
	parsedAlias, aliasErr := ParseHAAlias(alias)
	if err != nil || aliasErr != nil {
		return ErrInvalidInput
	}
	return r.mutate(ctx, func(tx pgx.Tx) error {
		var retired bool
		var originalAlias string
		if err := tx.QueryRow(ctx, `SELECT tombstoned,manifest_alias FROM public.registry_entities WHERE id=$1`, entityText).Scan(&retired, &originalAlias); errors.Is(err, pgx.ErrNoRows) {
			return ErrUnconfigured
		} else if err != nil {
			return err
		}
		if retired {
			return ErrTombstoned
		}
		if r.manifest.permits(parsedAlias) && alias != originalAlias {
			return ErrAliasConflict
		}
		var owner string
		var current bool
		err := tx.QueryRow(ctx, `SELECT entity_id,current FROM public.registry_aliases WHERE alias=$1`, alias).Scan(&owner, &current)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil && owner != entityText {
			return ErrAliasConflict
		}
		if current {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE public.registry_aliases SET current=false WHERE entity_id=$1 AND current`, entityText); err != nil {
			return err
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE public.registry_aliases SET current=true WHERE alias=$1`, alias)
		} else {
			_, err = tx.Exec(ctx, `INSERT INTO public.registry_aliases(alias,entity_id,current) VALUES ($1,$2,true)`, alias, entityText)
		}
		return err
	})
}

// Tombstone keeps the source key and aliases reserved. Rediscovery cannot revive it.
func (r *Repository) Tombstone(ctx context.Context, entity domain.EntityID) error {
	entityText, err := entity.Value()
	if err != nil {
		return ErrInvalidInput
	}
	return r.mutate(ctx, func(tx pgx.Tx) error {
		result, err := tx.Exec(ctx, `UPDATE public.registry_entities SET tombstoned=true WHERE id=$1`, entityText)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return ErrUnconfigured
		}
		return nil
	})
}

// Alias identifies a current or historical HA label independently of the
// immutable Host entity ID. Retired aliases remain visible for references.
type Alias struct {
	Entity  domain.EntityID
	Current bool
	Retired bool
}

func (r *Repository) LookupAlias(ctx context.Context, label string) (Alias, error) {
	var result Alias
	if _, err := ParseHAAlias(label); err != nil {
		return result, ErrInvalidInput
	}
	err := r.transaction(ctx, func(tx pgx.Tx) error {
		var entityText string
		err := tx.QueryRow(ctx, `SELECT a.entity_id,a.current,e.tombstoned FROM public.registry_aliases a JOIN public.registry_entities e ON e.id=a.entity_id WHERE a.alias=$1`, label).Scan(&entityText, &result.Current, &result.Retired)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnconfigured
		}
		if err != nil {
			return err
		}
		result.Entity, err = domain.ParseEntityID(entityText)
		if err != nil {
			return ErrStorage
		}
		return nil
	})
	return result, err
}
