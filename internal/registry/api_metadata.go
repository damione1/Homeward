package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/damione1/Homeward/internal/apimutations"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
)

var ErrMetadataConflict = errors.New("entity metadata revision changed")
var ErrMetadataCapacity = errors.New("entity metadata capacity exhausted")
var ErrMetadataCommitUnknown = errors.New("entity metadata commit outcome unknown")

func (r *Repository) HomeMetadata(ctx context.Context) (apitypes.HomeMetadata, error) {
	name, err := r.HomeName(ctx)
	if err != nil {
		return apitypes.HomeMetadata{}, err
	}
	var created time.Time
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err = r.store.InTransaction(bounded, func(tx pgx.Tx) error {
		return tx.QueryRow(bounded, `SELECT create_time FROM public.homeward_registry_home WHERE singleton=true`).Scan(&created)
	})
	if err != nil {
		return apitypes.HomeMetadata{}, classify(err)
	}
	metadata, err := apitypes.NewHomeMetadata(name, created)
	if err != nil {
		return apitypes.HomeMetadata{}, ErrStorage
	}
	return metadata, nil
}

// HomeName is the single persisted installation resource identity. It is not a
// house label, alias, hostname or credential. No default name is fabricated.
func (r *Repository) HomeName(ctx context.Context) (apitypes.HomeName, error) {
	if r == nil || r.store == nil || ctx == nil {
		return apitypes.HomeName{}, ErrInvalidInput
	}
	r.mu.RLock()
	name := r.home
	r.mu.RUnlock()
	if name.Valid() {
		return name, nil
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var id string
	err := r.store.InTransaction(bounded, func(tx pgx.Tx) error {
		return tx.QueryRow(bounded, `SELECT resource_id FROM public.homeward_registry_home WHERE singleton=true`).Scan(&id)
	})
	if err != nil {
		return apitypes.HomeName{}, classify(err)
	}
	name, err = apitypes.ParseHomeName("homes/" + id)
	if err != nil {
		return apitypes.HomeName{}, ErrStorage
	}
	r.mu.Lock()
	if r.home.Valid() && r.home != name {
		r.mu.Unlock()
		return apitypes.HomeName{}, ErrStorage
	}
	r.home = name
	r.mu.Unlock()
	return name, nil
}

// ResolveEntityName reads only the registry index. Callers must authorize the
// returned native identity before diagnostics or data exposure; a public name
// is never authority. Existing native IDs and aliases remain unchanged.
func (r *Repository) ResolveEntityName(ctx context.Context, name apitypes.EntityName) (domain.EntityID, error) {
	if !name.Valid() {
		return domain.EntityID{}, ErrInvalidInput
	}
	home, err := r.HomeName(ctx)
	if err != nil {
		return domain.EntityID{}, err
	}
	if name.Home() != home {
		return domain.EntityID{}, ErrUnconfigured
	}
	publicID := strings.TrimPrefix(name.String(), home.String()+"/entities/")
	var id, alias string
	var retired bool
	err = r.transaction(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id,manifest_alias,tombstoned FROM public.registry_entities WHERE api_resource_id=$1`, publicID).Scan(&id, &alias, &retired)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.EntityID{}, ErrUnconfigured
	}
	if err != nil {
		return domain.EntityID{}, err
	}
	parsedAlias, err := ParseHAAlias(alias)
	if err != nil || !r.manifest.permits(parsedAlias) || retired {
		return domain.EntityID{}, ErrUnconfigured
	}
	entity, err := domain.ParseEntityID(id)
	if err != nil {
		return domain.EntityID{}, ErrStorage
	}
	expected, err := apitypes.EntityNameForIdentity(home, entity)
	if err != nil || expected != name {
		return domain.EntityID{}, ErrStorage
	}
	return entity, nil
}

func scanMetadata(row pgx.Row, id domain.EntityID) (apitypes.EntityMetadata, error) {
	var label string
	var revision int64
	var created, updated sql.NullTime
	if err := row.Scan(&label, &revision, &created, &updated); err != nil {
		return apitypes.EntityMetadata{}, err
	}
	name, err := apitypes.NewDisplayName(label)
	if err != nil {
		return apitypes.EntityMetadata{}, ErrStorage
	}
	var createTime, updateTime *time.Time
	if created.Valid {
		createTime = &created.Time
	}
	if updated.Valid {
		updateTime = &updated.Time
	}
	metadata, err := apitypes.NewEntityMetadata(id, name, revision, createTime, updateTime)
	if err != nil {
		return apitypes.EntityMetadata{}, ErrStorage
	}
	return metadata, nil
}

// UpdateEntityMetadata is an ordinary administrative registry transaction.
// The opaque evidence is SQL-fenced before deduplication or registry data; the
// original result is encoded purely under the same transaction, never after
// commit. No observed-state or hardware operation is expressible here.
func (r *Repository) UpdateEntityMetadata(ctx context.Context, evidence authorization.MetadataEvidence, id domain.EntityID, update apitypes.EntityUpdate,
	encode func(apitypes.EntityMetadata) ([]byte, error)) (apitypes.EntityMetadata, error) {
	var zero apitypes.EntityMetadata
	if r == nil || r.store == nil || ctx == nil || !update.Valid() {
		return zero, ErrInvalidInput
	}
	text, err := id.Value()
	if err != nil {
		return zero, ErrInvalidInput
	}
	rights, err := evidence.Rights()
	if err != nil {
		return zero, authorization.ErrDenied
	}
	account, err := rights.Account()
	principal, principalErr := authorization.PrincipalFromContext(ctx)
	if err != nil || principalErr != nil {
		return zero, authorization.ErrDenied
	}
	caller, err := principal.Account()
	if err != nil || caller != account {
		return zero, authorization.ErrDenied
	}
	home, err := r.HomeName(ctx)
	if err != nil {
		return zero, err
	}
	expectedName, err := apitypes.EntityNameForIdentity(home, id)
	if err != nil || update.Target() != expectedName {
		return zero, ErrInvalidInput
	}
	label, selected := update.Patch().DisplayName()
	if requestID, present := update.RequestID(); present {
		normalized, err := json.Marshal(struct {
			Name, ETag, Label string
			Selected          bool
		}{update.Target().String(), update.ETag().String(), label.String(), selected})
		if err != nil {
			return zero, ErrInvalidInput
		}
		request, err := apimutations.New(ctx, "/home.api.v2.HomeService/UpdateEntity", requestID.String(), normalized)
		if err != nil {
			return zero, err
		}
		ctx, err = apimutations.WithResultEncoder(ctx, request, encode)
		if err != nil {
			return zero, err
		}
	}
	if err := r.acquire(ctx); err != nil {
		return zero, err
	}
	defer r.release()
	dirty, _ := r.projectionState()
	if dirty {
		return zero, ErrProjectionUnavailable
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var result apitypes.EntityMetadata
	changed := false
	outcome, err := r.store.InTransactionOutcome(bounded, func(tx pgx.Tx) error {
		if err := authorization.FenceSQL(bounded, tx, rights); err != nil {
			return err
		}
		var err error
		result, err = apimutations.Execute(bounded, tx, func() (apitypes.EntityMetadata, error) {
			if err := lock(bounded, tx); err != nil {
				return zero, err
			}
			var alias string
			var retired bool
			if err := tx.QueryRow(bounded, `SELECT manifest_alias,tombstoned FROM public.registry_entities WHERE id=$1 FOR UPDATE`, text).Scan(&alias, &retired); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return zero, ErrUnconfigured
				}
				return zero, err
			}
			original, err := ParseHAAlias(alias)
			if err != nil || !r.manifest.permits(original) || retired {
				return zero, ErrUnconfigured
			}
			current, err := scanMetadata(tx.QueryRow(bounded, `SELECT api_display_name,api_metadata_revision,api_create_time,api_update_time FROM public.registry_entities WHERE id=$1`, text), id)
			if err != nil {
				return zero, err
			}
			if current.ETag() != update.ETag() {
				return zero, ErrMetadataConflict
			}
			if !selected || current.Label() == label {
				return current, nil
			}
			if current.Revision() == math.MaxInt64 {
				return zero, ErrMetadataCapacity
			}
			updated, err := scanMetadata(tx.QueryRow(bounded, `UPDATE public.registry_entities SET api_display_name=$2,api_metadata_revision=api_metadata_revision+1,api_update_time=greatest(clock_timestamp(),api_create_time,api_update_time) WHERE id=$1 RETURNING api_display_name,api_metadata_revision,api_create_time,api_update_time`, text, label.String()), id)
			if err == nil {
				changed = true
			}
			return updated, err
		})
		return err
	})
	if outcome == storage.TransactionIndeterminate {
		r.invalidateAPIProjection()
		return zero, ErrMetadataCommitUnknown
	}
	if err != nil {
		if errors.Is(err, authorization.ErrDenied) || errors.Is(err, ErrMetadataConflict) || errors.Is(err, ErrMetadataCapacity) || errors.Is(err, ErrUnconfigured) || apimutations.IsResultError(err) {
			return zero, err
		}
		return zero, classify(err)
	}
	if outcome != storage.TransactionCommitted {
		return zero, ErrStorage
	}
	if changed {
		if err := r.publishAPIProjection(ctx); err != nil {
			return zero, err
		}
	}
	return result, nil
}

func (r *Repository) invalidateAPIProjection() {
	r.mu.Lock()
	projection := r.projection
	if projection != nil {
		r.projectionDirty = true
	}
	r.mu.Unlock()
	if projection != nil {
		projection.InvalidateRegistry()
	}
}

func (r *Repository) publishAPIProjection(ctx context.Context) error {
	r.mu.RLock()
	projection := r.projection
	r.mu.RUnlock()
	if projection == nil {
		return nil
	}
	background, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	records, err := r.listUnlocked(background)
	if err == nil {
		err = projection.PublishRegistry(background, records)
	}
	if err != nil {
		r.invalidateAPIProjection()
		return ErrProjectionUnavailable
	}
	return nil
}
