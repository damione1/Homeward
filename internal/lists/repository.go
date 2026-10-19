package lists

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
)

type Repository struct {
	store    *storage.Store
	manifest registry.Manifest
	// Isolated tests can coordinate SQL races without changing the lock order.
	beforeFence func()
	afterFence  func()
}

func NewRepository(store *storage.Store, manifest registry.Manifest) (*Repository, error) {
	if store == nil {
		return nil, failure(Invalid)
	}
	if _, ok := manifest.Digest(); !ok {
		return nil, failure(Invalid)
	}
	return &Repository{store: store, manifest: manifest}, nil
}

// fence is the first SQL work in every list transaction. It never calls back
// into Authority while holding these share locks; account writers may hold the
// memory barrier while waiting for the same SQL rows.
func fence(ctx context.Context, tx pgx.Tx, e authorization.RightsEvidence) error {
	account, err := e.Account()
	if err != nil {
		return failure(Denied)
	}
	id, err := account.Value()
	if err != nil {
		return failure(Denied)
	}
	var role, status int16
	var revocation int64
	err = tx.QueryRow(ctx, `SELECT role,status,revocation_revision FROM public.auth_accounts WHERE id=$1 FOR SHARE`, id).Scan(&role, &status, &revocation)
	if errors.Is(err, pgx.ErrNoRows) {
		return failure(Denied)
	}
	if err != nil {
		return err
	}
	var rights int64
	var semantic []byte
	err = tx.QueryRow(ctx, `SELECT rights_revision,semantic_sha256 FROM public.authorization_policy_active WHERE account_id=$1 FOR SHARE`, id).Scan(&rights, &semantic)
	if errors.Is(err, pgx.ErrNoRows) {
		return failure(Denied)
	}
	if err != nil {
		return err
	}
	if !e.MatchesSQL(role, status, revocation, rights, semantic) {
		return failure(Denied)
	}
	return nil
}

func (r *Repository) transaction(ctx context.Context, e authorization.RightsEvidence, fn func(context.Context, pgx.Tx) error) error {
	if r == nil || r.store == nil || ctx == nil || fn == nil {
		return failure(Invalid)
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	outcome, err := r.store.InTransactionOutcome(bounded, func(tx pgx.Tx) error {
		if r.beforeFence != nil {
			r.beforeFence()
		}
		if err := fence(bounded, tx, e); err != nil {
			return err
		}
		if r.afterFence != nil {
			r.afterFence()
		}
		return fn(bounded, tx)
	})
	if outcome == storage.TransactionIndeterminate {
		return failure(CommitUnknown)
	}
	if err != nil {
		var own *Error
		if errors.As(err, &own) {
			return own
		}
		if bounded.Err() != nil {
			return failure(Unavailable)
		}
		return failure(Unavailable)
	}
	if outcome != storage.TransactionCommitted {
		return failure(Unavailable)
	}
	return nil
}

func lockCatalog(ctx context.Context, tx pgx.Tx) (int64, error) {
	var revision int64
	err := tx.QueryRow(ctx, `SELECT revision FROM public.homeward_lists_catalog WHERE singleton=true FOR UPDATE`).Scan(&revision)
	if err != nil || revision <= 0 {
		return 0, failure(Unavailable)
	}
	if revision == math.MaxInt64 {
		return 0, failure(Capacity)
	}
	return revision, nil
}
func bumpCatalog(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `UPDATE public.homeward_lists_catalog SET revision=revision+1 WHERE singleton=true AND revision<$1`, int64(math.MaxInt64))
	return err
}
func readCatalog(ctx context.Context, tx pgx.Tx) (int64, error) {
	var revision int64
	err := tx.QueryRow(ctx, `SELECT revision FROM public.homeward_lists_catalog WHERE singleton=true FOR SHARE`).Scan(&revision)
	if err != nil || revision <= 0 {
		return 0, failure(Unavailable)
	}
	return revision, nil
}

func readList(ctx context.Context, tx pgx.Tx, id domain.ListID, write bool) (ListSnapshot, error) {
	text, err := id.Value()
	if err != nil {
		return ListSnapshot{}, failure(Invalid)
	}
	query := `SELECT origin::text,legacy_alias,name,lifecycle,revision,updated_at FROM public.homeward_lists WHERE list_id=$1`
	if write {
		query += ` FOR UPDATE`
	} else {
		query += ` FOR SHARE`
	}
	var originText, name string
	var aliasText *string
	var lifecycle int16
	var revision int64
	var updated time.Time
	err = tx.QueryRow(ctx, query, text).Scan(&originText, &aliasText, &name, &lifecycle, &revision, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return ListSnapshot{}, failure(NotFound)
	}
	if err != nil {
		return ListSnapshot{}, err
	}
	state, stateErr := lifecycleFromSQL(lifecycle)
	if stateErr != nil {
		return ListSnapshot{}, stateErr
	}
	if state != lifecycleActive || revision <= 0 {
		return ListSnapshot{}, failure(NotFound)
	}
	origin, err := originFromSQL(originText)
	if err != nil {
		return ListSnapshot{}, err
	}
	if !validText(name, MaxNameBytes, true) {
		return ListSnapshot{}, failure(Unavailable)
	}
	rev, _ := NewListRevision(id, revision)
	snapshot := ListSnapshot{id: id, origin: origin, name: name, revision: rev, updated: updated}
	if aliasText != nil {
		alias, parseErr := domain.ParseHAAlias(*aliasText)
		if parseErr != nil {
			return ListSnapshot{}, failure(Unavailable)
		}
		snapshot.alias = alias
	}
	return snapshot, nil
}
func readItem(ctx context.Context, tx pgx.Tx, list domain.ListID, uid ItemUID, write bool) (ItemSnapshot, error) {
	listText, err := list.Value()
	if err != nil {
		return ItemSnapshot{}, failure(Invalid)
	}
	uidText, err := uid.Value()
	if err != nil {
		return ItemSnapshot{}, failure(Invalid)
	}
	query := `SELECT origin::text,summary,completed,description,due_date::text,due_at,due_zone,completed_at,position,lifecycle,revision,updated_at FROM public.homeward_list_items WHERE list_id=$1 AND item_uid=$2`
	if write {
		query += ` FOR UPDATE`
	} else {
		query += ` FOR SHARE`
	}
	var originText, summary string
	var completed bool
	var description, dueDate, dueZone *string
	var dueAt, completedAt *time.Time
	var position, revision int64
	var lifecycle int16
	var updated time.Time
	err = tx.QueryRow(ctx, query, listText, uidText).Scan(&originText, &summary, &completed, &description, &dueDate, &dueAt, &dueZone, &completedAt, &position, &lifecycle, &revision, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return ItemSnapshot{}, failure(NotFound)
	}
	if err != nil {
		return ItemSnapshot{}, err
	}
	state, stateErr := lifecycleFromSQL(lifecycle)
	if stateErr != nil {
		return ItemSnapshot{}, stateErr
	}
	if state != lifecycleActive || revision <= 0 || position < 0 {
		return ItemSnapshot{}, failure(NotFound)
	}
	origin, err := originFromSQL(originText)
	if err != nil {
		return ItemSnapshot{}, err
	}
	due := NoDue()
	if dueDate != nil {
		due, err = NewDueDate(*dueDate)
	} else if dueAt != nil && dueZone != nil {
		due, err = NewDueInstant(*dueAt, *dueZone)
	}
	if err != nil {
		return ItemSnapshot{}, failure(Unavailable)
	}
	payload, err := NewItemPayload(ItemInput{Summary: summary, Completed: completed, Description: description, Due: due, CompletedAt: completedAt})
	if err != nil || !payload.validFor(origin) {
		return ItemSnapshot{}, failure(Unavailable)
	}
	rev, _ := NewItemRevision(list, uid, revision)
	return ItemSnapshot{list: list, uid: uid, origin: origin, payload: payload, position: position, revision: rev, updated: updated}, nil
}

func expectList(id domain.ListID, expected ListRevision, current ListSnapshot) error {
	gotID, number, err := expected.Values()
	if err != nil || gotID != id {
		return failure(Invalid)
	}
	_, actual, _ := current.revision.Values()
	if number != actual {
		return failure(Conflict)
	}
	if number == math.MaxInt64 {
		return failure(Capacity)
	}
	return nil
}
func expectItem(list domain.ListID, uid ItemUID, expected ItemRevision, current ItemSnapshot) error {
	gotList, gotUID, number, err := expected.Values()
	if err != nil || gotList != list || gotUID != uid {
		return failure(Invalid)
	}
	_, _, actual, _ := current.revision.Values()
	if number != actual {
		return failure(Conflict)
	}
	if number == math.MaxInt64 {
		return failure(Capacity)
	}
	return nil
}
