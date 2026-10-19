package lists

import (
	"context"
	"errors"
	"math"
	"time"

	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/jackc/pgx/v5"
)

func sqlItemFields(p ItemPayload) (any, any, any, any, any, error) {
	var description, dueDate, dueAt, dueZone, completedAt any
	if p.description != nil {
		description = *p.description
	}
	date, at, zone, err := p.due.Values()
	if err != nil {
		return nil, nil, nil, nil, nil, failure(Invalid)
	}
	if date != "" {
		parsed, parseErr := time.Parse("2006-01-02", date)
		if parseErr != nil {
			return nil, nil, nil, nil, nil, failure(Invalid)
		}
		dueDate = parsed
	}
	if !at.IsZero() {
		dueAt = at
		dueZone = zone
	}
	if p.completedAt != nil {
		completedAt = *p.completedAt
	}
	return description, dueDate, dueAt, dueZone, completedAt, nil
}
func nextListRevision(ctx context.Context, tx pgx.Tx, id domain.ListID) error {
	text, _ := id.Value()
	_, err := tx.Exec(ctx, `UPDATE public.homeward_lists SET revision=revision+1,updated_at=now() WHERE list_id=$1 AND lifecycle=1`, text)
	return err
}
func (s *Service) mutateItem(ctx context.Context, id domain.ListID, expected ListRevision,
	fn func(context.Context, pgx.Tx, ListSnapshot) (ItemUID, int64, error)) (MutationReceipt, error) {
	object, err := listObject(id)
	if err != nil {
		return MutationReceipt{}, failure(Invalid)
	}
	return executeWithUnknown(s, ctx, authv1.ScopeOperation_SCOPE_OPERATION_WRITE, object, true,
		func(ctx context.Context, tx pgx.Tx, _ authorization.RightsEvidence, _ authorization.ListSelection) (MutationReceipt, error) {
			if _, err := lockCatalog(ctx, tx); err != nil {
				return MutationReceipt{}, err
			}
			current, err := readList(ctx, tx, id, true)
			if err != nil {
				return MutationReceipt{}, err
			}
			if err := expectList(id, expected, current); err != nil {
				return MutationReceipt{}, err
			}
			uid, itemNumber, err := fn(ctx, tx, current)
			if err != nil {
				return MutationReceipt{}, err
			}
			if err := nextListRevision(ctx, tx, id); err != nil {
				return MutationReceipt{}, err
			}
			if err := bumpCatalog(ctx, tx); err != nil {
				return MutationReceipt{}, err
			}
			_, number, _ := current.revision.Values()
			return itemReceipt(id, uid, number+1, itemNumber), nil
		}, func(r MutationReceipt) { afterListCommit(s, r) }, func(r MutationReceipt) { s.resyncList(r.list) })
}

func (s *Service) AddItem(ctx context.Context, id domain.ListID, expected ListRevision, payload ItemPayload) (MutationReceipt, error) {
	return s.mutateItem(ctx, id, expected, func(ctx context.Context, tx pgx.Tx, list ListSnapshot) (ItemUID, int64, error) {
		if !payload.validFor(list.origin) {
			return ItemUID{}, 0, failure(Invalid)
		}
		text, _ := id.Value()
		var highest *int64
		err := tx.QueryRow(ctx, `SELECT max(position) FROM public.homeward_list_items WHERE list_id=$1 AND lifecycle=1`, text).Scan(&highest)
		if err != nil {
			return ItemUID{}, 0, err
		}
		position := int64(0)
		if highest != nil {
			if *highest == math.MaxInt64 {
				return ItemUID{}, 0, failure(Capacity)
			}
			position = *highest + 1
		}
		uid, err := freshItemUID()
		if err != nil {
			return ItemUID{}, 0, err
		}
		uidText, _ := uid.Value()
		originSQL, err := list.origin.sql()
		if err != nil {
			return ItemUID{}, 0, err
		}
		description, date, at, zone, completedAt, err := sqlItemFields(payload)
		if err != nil {
			return ItemUID{}, 0, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO public.homeward_list_items(list_id,item_uid,origin,summary,completed,description,due_date,due_at,due_zone,completed_at,position,lifecycle,revision)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,1,1)`, text, uidText, originSQL, payload.summary, payload.completed, description, date, at, zone, completedAt, position)
		if err != nil {
			return ItemUID{}, 0, err
		}
		return uid, 1, nil
	})
}

func (s *Service) ImportLegacyItem(ctx context.Context, id domain.ListID, payload LegacyItemPayload, expected ListRevision) (MutationReceipt, error) {
	_, err := listObject(id)
	if err != nil || !payload.valid {
		return MutationReceipt{}, failure(Invalid)
	}
	return executeWithUnknown(s, ctx, authv1.ScopeOperation_SCOPE_OPERATION_WRITE, collection(), true,
		func(ctx context.Context, tx pgx.Tx, _ authorization.RightsEvidence, _ authorization.ListSelection) (MutationReceipt, error) {
			if _, err := lockCatalog(ctx, tx); err != nil {
				return MutationReceipt{}, err
			}
			text, _ := id.Value()
			var aliasText, originText string
			var lifecycle int16
			var listNumber int64
			err := tx.QueryRow(ctx, `SELECT legacy_alias,origin::text,lifecycle,revision FROM public.homeward_lists WHERE list_id=$1 FOR UPDATE`, text).Scan(&aliasText, &originText, &lifecycle, &listNumber)
			if errors.Is(err, pgx.ErrNoRows) {
				return MutationReceipt{}, failure(NotFound)
			}
			if err != nil {
				return MutationReceipt{}, err
			}
			state, stateErr := lifecycleFromSQL(lifecycle)
			if stateErr != nil {
				return MutationReceipt{}, stateErr
			}
			alias, err := domain.ParseHAAlias(aliasText)
			if err != nil {
				return MutationReceipt{}, failure(Invalid)
			}
			source, present := s.repo.manifest.SourceFor(alias)
			sourceText, valid := source.Value()
			if !present || !valid {
				return MutationReceipt{}, failure(Invalid)
			}
			origin, err := originForAlias(sourceText)
			if err != nil || !payload.validFor(origin) {
				return MutationReceipt{}, failure(Invalid)
			}
			originSQL, err := origin.sql()
			if err != nil || originSQL != originText {
				return MutationReceipt{}, failure(Invalid)
			}
			uidText, _ := payload.uid.Value()
			hash, err := itemSourceHash(alias, payload)
			if err != nil {
				return MutationReceipt{}, err
			}
			var oldHash []byte
			err = tx.QueryRow(ctx, `SELECT source_sha256 FROM public.homeward_legacy_import_ledger WHERE legacy_alias=$1 AND item_uid=$2`, aliasText, uidText).Scan(&oldHash)
			if err == nil {
				if !equalHash(oldHash, hash) {
					return MutationReceipt{}, failure(ImportConflict)
				}
				return MutationReceipt{list: id, item: payload.uid, outcome: AlreadyImported}, nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return MutationReceipt{}, err
			}
			if state != lifecycleActive {
				return MutationReceipt{}, failure(NotFound)
			}
			gotID, number, err := expected.Values()
			if err != nil || gotID != id {
				return MutationReceipt{}, failure(Invalid)
			}
			if number != listNumber {
				return MutationReceipt{}, failure(Conflict)
			}
			if listNumber == math.MaxInt64 {
				return MutationReceipt{}, failure(Capacity)
			}
			description, date, at, zone, completedAt, err := sqlItemFields(payload.item)
			if err != nil {
				return MutationReceipt{}, err
			}
			_, err = tx.Exec(ctx, `INSERT INTO public.homeward_list_items(list_id,item_uid,origin,summary,completed,description,due_date,due_at,due_zone,completed_at,position,lifecycle,revision)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,1,1)`, text, uidText, originSQL, payload.item.summary, payload.item.completed, description, date, at, zone, completedAt, payload.position)
			if err != nil {
				return MutationReceipt{}, importInsertError(err)
			}
			_, err = tx.Exec(ctx, `INSERT INTO public.homeward_legacy_import_ledger(legacy_alias,item_uid,source_sha256,first_revision) VALUES($1,$2,$3,1)`, aliasText, uidText, hash[:])
			if err != nil {
				return MutationReceipt{}, err
			}
			if err := nextListRevision(ctx, tx, id); err != nil {
				return MutationReceipt{}, err
			}
			if err := bumpCatalog(ctx, tx); err != nil {
				return MutationReceipt{}, err
			}
			return itemReceipt(id, payload.uid, listNumber+1, 1), nil
		}, func(r MutationReceipt) {
			if r.outcome == Committed {
				afterListCommit(s, r)
			}
		}, func(r MutationReceipt) { s.resyncList(r.list) })
}

func (s *Service) GetItem(ctx context.Context, id domain.ListID, uid ItemUID) (ItemSnapshot, error) {
	object, err := listObject(id)
	if err != nil {
		return ItemSnapshot{}, failure(Invalid)
	}
	if _, err := uid.Value(); err != nil {
		return ItemSnapshot{}, failure(Invalid)
	}
	return execute(s, ctx, authv1.ScopeOperation_SCOPE_OPERATION_READ, object, false,
		func(ctx context.Context, tx pgx.Tx, _ authorization.RightsEvidence, _ authorization.ListSelection) (ItemSnapshot, error) {
			if _, err := readList(ctx, tx, id, false); err != nil {
				return ItemSnapshot{}, err
			}
			return readItem(ctx, tx, id, uid, false)
		}, nil)
}
func (s *Service) ListItems(ctx context.Context, id domain.ListID, cursor ItemCursor, limit int) (ItemPage, error) {
	object, err := listObject(id)
	if err != nil || limit < 1 || limit > MaxPage {
		return ItemPage{}, failure(Invalid)
	}
	return execute(s, ctx, authv1.ScopeOperation_SCOPE_OPERATION_READ, object, false,
		func(ctx context.Context, tx pgx.Tx, _ authorization.RightsEvidence, _ authorization.ListSelection) (ItemPage, error) {
			list, err := readList(ctx, tx, id, false)
			if err != nil {
				return ItemPage{}, err
			}
			_, listNumber, _ := list.revision.Values()
			lastPos := int64(-1)
			lastUID := ""
			if cursor.valid {
				if cursor.list != id || cursor.revision != listNumber {
					return ItemPage{}, failure(StaleCursor)
				}
				lastPos = cursor.lastPosition
				lastUID, err = cursor.last.Value()
				if err != nil || lastPos < 0 {
					return ItemPage{}, failure(Invalid)
				}
			}
			text, _ := id.Value()
			rows, err := tx.Query(ctx, `SELECT item_uid,position FROM public.homeward_list_items WHERE list_id=$1 AND lifecycle=1 AND (position,item_uid)>($2,$3) ORDER BY position,item_uid LIMIT $4`, text, lastPos, lastUID, limit+1)
			if err != nil {
				return ItemPage{}, err
			}
			type key struct {
				uid ItemUID
				pos int64
			}
			keys := make([]key, 0, limit+1)
			for rows.Next() {
				var uidText string
				var pos int64
				if scanErr := rows.Scan(&uidText, &pos); scanErr != nil {
					rows.Close()
					return ItemPage{}, scanErr
				}
				uid, parseErr := ParseItemUID(uidText)
				if parseErr != nil {
					rows.Close()
					return ItemPage{}, failure(Unavailable)
				}
				keys = append(keys, key{uid, pos})
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return ItemPage{}, err
			}
			page := ItemPage{entries: make([]ItemSnapshot, 0, min(len(keys), limit))}
			for i, key := range keys {
				if i >= limit {
					break
				}
				item, readErr := readItem(ctx, tx, id, key.uid, false)
				if readErr != nil {
					return ItemPage{}, readErr
				}
				page.entries = append(page.entries, item)
			}
			if len(keys) > limit {
				key := keys[limit-1]
				page.next = ItemCursor{valid: true, list: id, revision: listNumber, lastPosition: key.pos, last: key.uid}
			}
			return page, nil
		}, nil)
}
func (s *Service) CountItems(ctx context.Context, id domain.ListID) (uint64, error) {
	object, err := listObject(id)
	if err != nil {
		return 0, failure(Invalid)
	}
	return execute(s, ctx, authv1.ScopeOperation_SCOPE_OPERATION_READ, object, false,
		func(ctx context.Context, tx pgx.Tx, _ authorization.RightsEvidence, _ authorization.ListSelection) (uint64, error) {
			if _, err := readList(ctx, tx, id, false); err != nil {
				return 0, err
			}
			text, _ := id.Value()
			var count int64
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM public.homeward_list_items WHERE list_id=$1 AND lifecycle=1`, text).Scan(&count); err != nil || count < 0 {
				return 0, failure(Unavailable)
			}
			return uint64(count), nil
		}, nil)
}

func (s *Service) EditItem(ctx context.Context, id domain.ListID, uid ItemUID, expectedList ListRevision, expectedItem ItemRevision, payload ItemPayload) (MutationReceipt, error) {
	return s.mutateItem(ctx, id, expectedList, func(ctx context.Context, tx pgx.Tx, list ListSnapshot) (ItemUID, int64, error) {
		if !payload.validFor(list.origin) {
			return ItemUID{}, 0, failure(Invalid)
		}
		item, err := readItem(ctx, tx, id, uid, true)
		if err != nil {
			return ItemUID{}, 0, err
		}
		if err := expectItem(id, uid, expectedItem, item); err != nil {
			return ItemUID{}, 0, err
		}
		listText, _ := id.Value()
		uidText, _ := uid.Value()
		_, _, number, _ := item.revision.Values()
		description, date, at, zone, completedAt, err := sqlItemFields(payload)
		if err != nil {
			return ItemUID{}, 0, err
		}
		_, err = tx.Exec(ctx, `UPDATE public.homeward_list_items SET summary=$3,completed=$4,description=$5,due_date=$6,due_at=$7,due_zone=$8,completed_at=$9,revision=revision+1,updated_at=now() WHERE list_id=$1 AND item_uid=$2`, listText, uidText, payload.summary, payload.completed, description, date, at, zone, completedAt)
		if err != nil {
			return ItemUID{}, 0, err
		}
		return uid, number + 1, nil
	})
}
func (s *Service) DeleteItem(ctx context.Context, id domain.ListID, uid ItemUID, expectedList ListRevision, expectedItem ItemRevision) (MutationReceipt, error) {
	return s.mutateItem(ctx, id, expectedList, func(ctx context.Context, tx pgx.Tx, _ ListSnapshot) (ItemUID, int64, error) {
		item, err := readItem(ctx, tx, id, uid, true)
		if err != nil {
			return ItemUID{}, 0, err
		}
		if err := expectItem(id, uid, expectedItem, item); err != nil {
			return ItemUID{}, 0, err
		}
		listText, _ := id.Value()
		uidText, _ := uid.Value()
		_, _, number, _ := item.revision.Values()
		_, err = tx.Exec(ctx, `UPDATE public.homeward_list_items SET lifecycle=2,revision=revision+1,updated_at=now() WHERE list_id=$1 AND item_uid=$2`, listText, uidText)
		if err != nil {
			return ItemUID{}, 0, err
		}
		return uid, number + 1, nil
	})
}
func (s *Service) SetCompleted(ctx context.Context, id domain.ListID, uid ItemUID, expectedList ListRevision, expectedItem ItemRevision, completed bool) (MutationReceipt, error) {
	return s.mutateItem(ctx, id, expectedList, func(ctx context.Context, tx pgx.Tx, list ListSnapshot) (ItemUID, int64, error) {
		item, err := readItem(ctx, tx, id, uid, true)
		if err != nil {
			return ItemUID{}, 0, err
		}
		if err := expectItem(id, uid, expectedItem, item); err != nil {
			return ItemUID{}, 0, err
		}
		listText, _ := id.Value()
		uidText, _ := uid.Value()
		_, _, number, _ := item.revision.Values()
		var completedAt any
		if completed && list.origin != ShoppingList {
			if item.payload.completedAt != nil {
				completedAt = *item.payload.completedAt
			} else {
				completedAt = time.Now().UTC()
			}
		}
		_, err = tx.Exec(ctx, `UPDATE public.homeward_list_items SET completed=$3,completed_at=$4,revision=revision+1,updated_at=now() WHERE list_id=$1 AND item_uid=$2`, listText, uidText, completed, completedAt)
		if err != nil {
			return ItemUID{}, 0, err
		}
		return uid, number + 1, nil
	})
}

// MoveItem computes a dense new order in PostgreSQL. It does not fetch an
// unbounded list into Go. Every changed position advances that item's revision.
func (s *Service) MoveItem(ctx context.Context, id domain.ListID, uid ItemUID, after *ItemUID, expectedList ListRevision, expectedItem ItemRevision) (MutationReceipt, error) {
	if after != nil {
		if _, err := after.Value(); err != nil || *after == uid {
			return MutationReceipt{}, failure(Invalid)
		}
	}
	return s.mutateItem(ctx, id, expectedList, func(ctx context.Context, tx pgx.Tx, _ ListSnapshot) (ItemUID, int64, error) {
		item, err := readItem(ctx, tx, id, uid, true)
		if err != nil {
			return ItemUID{}, 0, err
		}
		if err := expectItem(id, uid, expectedItem, item); err != nil {
			return ItemUID{}, 0, err
		}
		listText, _ := id.Value()
		uidText, _ := uid.Value()
		const rankSQL = `WITH ranked AS (SELECT item_uid,row_number() OVER (ORDER BY position,item_uid)-1 AS rank FROM public.homeward_list_items WHERE list_id=$1 AND lifecycle=1) SELECT rank FROM ranked WHERE item_uid=$2`
		var oldRank int64
		if err := tx.QueryRow(ctx, rankSQL, listText, uidText).Scan(&oldRank); err != nil {
			return ItemUID{}, 0, err
		}
		newRank := int64(0)
		if after != nil {
			afterText, _ := after.Value()
			var targetRank int64
			err := tx.QueryRow(ctx, rankSQL, listText, afterText).Scan(&targetRank)
			if errors.Is(err, pgx.ErrNoRows) {
				return ItemUID{}, 0, failure(NotFound)
			}
			if err != nil {
				return ItemUID{}, 0, err
			}
			if targetRank < oldRank {
				newRank = targetRank + 1
			} else {
				newRank = targetRank
			}
		}
		if newRank == oldRank {
			return ItemUID{}, 0, failure(Invalid)
		}
		// List FOR UPDATE serializes every item writer; the ranked CTE sees a
		// stable order. No user content is passed into dynamic SQL.
		var exhausted bool
		var updated int64
		err = tx.QueryRow(ctx, `WITH ranked AS (
			SELECT item_uid,row_number() OVER (ORDER BY position,item_uid)-1 AS rank
			FROM public.homeward_list_items WHERE list_id=$1 AND lifecycle=1
		), desired AS (
			SELECT item_uid,CASE
				WHEN rank=$2 THEN $3::bigint
				WHEN $3::bigint < $2::bigint AND rank >= $3::bigint AND rank < $2::bigint THEN rank+1
				WHEN $3::bigint > $2::bigint AND rank > $2::bigint AND rank <= $3::bigint THEN rank-1
			ELSE rank END AS new_position FROM ranked
		), exhausted AS (
			SELECT EXISTS(SELECT 1 FROM public.homeward_list_items i JOIN desired d ON i.item_uid=d.item_uid
				WHERE i.list_id=$1 AND i.lifecycle=1 AND (i.item_uid=$4 OR i.position<>d.new_position)
				AND i.revision=$5) AS blocked
		), updated AS (
			UPDATE public.homeward_list_items i SET position=d.new_position,revision=i.revision+1,updated_at=now()
			FROM desired d WHERE i.list_id=$1 AND i.item_uid=d.item_uid AND i.lifecycle=1
				AND (i.item_uid=$4 OR i.position<>d.new_position)
				AND NOT (SELECT blocked FROM exhausted)
			RETURNING i.item_uid
		)
		SELECT (SELECT blocked FROM exhausted),(SELECT count(*) FROM updated)`, listText, oldRank, newRank, uidText, int64(math.MaxInt64)).Scan(&exhausted, &updated)
		if err != nil {
			return ItemUID{}, 0, err
		}
		if exhausted {
			return ItemUID{}, 0, failure(Capacity)
		}
		if updated == 0 {
			return ItemUID{}, 0, failure(Unavailable)
		}
		_, _, number, _ := item.revision.Values()
		return uid, number + 1, nil
	})
}
