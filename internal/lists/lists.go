package lists

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"

	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func collection() authorization.Object { return authorization.ListCollectionObject() }
func listReceipt(id domain.ListID, number int64) MutationReceipt {
	rev, _ := NewListRevision(id, number)
	return MutationReceipt{list: id, listRevision: rev, outcome: Committed}
}
func itemReceipt(id domain.ListID, uid ItemUID, listNumber, itemNumber int64) MutationReceipt {
	receipt := listReceipt(id, listNumber)
	receipt.item = uid
	receipt.itemRevision, _ = NewItemRevision(id, uid, itemNumber)
	return receipt
}
func afterListCommit(s *Service, receipt MutationReceipt) {
	rev, ok := receipt.ListRevision()
	if !ok {
		return
	}
	_, number, _ := rev.Values()
	s.publish(receipt.list, number)
}

func (s *Service) CreateList(ctx context.Context, payload ListPayload) (MutationReceipt, error) {
	if !payload.validPayload() {
		return MutationReceipt{}, failure(Invalid)
	}
	return executeWithUnknown(s, ctx, authv1.ScopeOperation_SCOPE_OPERATION_WRITE, collection(), true,
		func(ctx context.Context, tx pgx.Tx, _ authorization.RightsEvidence, _ authorization.ListSelection) (MutationReceipt, error) {
			if _, err := lockCatalog(ctx, tx); err != nil {
				return MutationReceipt{}, err
			}
			id, err := freshListID()
			if err != nil {
				return MutationReceipt{}, err
			}
			originSQL, err := Native.sql()
			if err != nil {
				return MutationReceipt{}, err
			}
			text, _ := id.Value()
			_, err = tx.Exec(ctx, `INSERT INTO public.homeward_lists(list_id,origin,name,lifecycle,revision) VALUES($1,$2,$3,1,1)`, text, originSQL, payload.name)
			if err != nil {
				return MutationReceipt{}, err
			}
			if err := bumpCatalog(ctx, tx); err != nil {
				return MutationReceipt{}, err
			}
			return listReceipt(id, 1), nil
		}, func(r MutationReceipt) { afterListCommit(s, r) }, func(r MutationReceipt) { s.resyncList(r.list) })
}

func originForAlias(manifestSource string) (Origin, error) {
	switch manifestSource {
	case "local_todo":
		return LocalTodo, nil
	case "shopping_list":
		return ShoppingList, nil
	default:
		return OriginInvalid, failure(Invalid)
	}
}
func (s *Service) ImportLegacyList(ctx context.Context, alias domain.HAAlias, payload LegacyListPayload) (MutationReceipt, error) {
	if _, err := alias.Value(); err != nil || !payload.validPayload() {
		return MutationReceipt{}, failure(Invalid)
	}
	return executeWithUnknown(s, ctx, authv1.ScopeOperation_SCOPE_OPERATION_WRITE, collection(), true,
		func(ctx context.Context, tx pgx.Tx, _ authorization.RightsEvidence, _ authorization.ListSelection) (MutationReceipt, error) {
			source, present := s.repo.manifest.SourceFor(alias)
			sourceText, valid := source.Value()
			if !present || !valid {
				return MutationReceipt{}, failure(Invalid)
			}
			origin, err := originForAlias(sourceText)
			if err != nil {
				return MutationReceipt{}, err
			}
			originSQL, err := origin.sql()
			if err != nil {
				return MutationReceipt{}, err
			}
			if _, err := lockCatalog(ctx, tx); err != nil {
				return MutationReceipt{}, err
			}
			aliasText, _ := alias.Value()
			hash, err := listSourceHash(alias, payload)
			if err != nil {
				return MutationReceipt{}, err
			}
			var oldHash []byte
			var oldID string
			err = tx.QueryRow(ctx, `SELECT l.source_sha256,h.list_id FROM public.homeward_legacy_import_ledger l JOIN public.homeward_lists h ON h.legacy_alias=l.legacy_alias WHERE l.legacy_alias=$1 AND l.item_uid=''`, aliasText).Scan(&oldHash, &oldID)
			if err == nil {
				if !equalHash(oldHash, hash) {
					return MutationReceipt{}, failure(ImportConflict)
				}
				id, parseErr := domain.ParseListID(oldID)
				if parseErr != nil {
					return MutationReceipt{}, failure(Unavailable)
				}
				return MutationReceipt{list: id, outcome: AlreadyImported}, nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return MutationReceipt{}, err
			}
			id, err := freshListID()
			if err != nil {
				return MutationReceipt{}, err
			}
			text, _ := id.Value()
			_, err = tx.Exec(ctx, `INSERT INTO public.homeward_lists(list_id,origin,legacy_alias,name,lifecycle,revision) VALUES($1,$2,$3,$4,1,1)`, text, originSQL, aliasText, payload.name)
			if err != nil {
				return MutationReceipt{}, importInsertError(err)
			}
			_, err = tx.Exec(ctx, `INSERT INTO public.homeward_legacy_import_ledger(legacy_alias,item_uid,source_sha256,first_revision) VALUES($1,'',$2,1)`, aliasText, hash[:])
			if err != nil {
				return MutationReceipt{}, err
			}
			if err := bumpCatalog(ctx, tx); err != nil {
				return MutationReceipt{}, err
			}
			return listReceipt(id, 1), nil
		}, func(r MutationReceipt) {
			if r.outcome == Committed {
				afterListCommit(s, r)
			}
		}, func(r MutationReceipt) { s.resyncList(r.list) })
}
func equalHash(raw []byte, want [sha256.Size]byte) bool {
	if len(raw) != sha256.Size {
		return false
	}
	var got [sha256.Size]byte
	copy(got[:], raw)
	return got == want
}

func importInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return failure(ImportConflict)
	}
	return err
}

func (s *Service) GetList(ctx context.Context, id domain.ListID) (ListSnapshot, error) {
	object, err := listObject(id)
	if err != nil {
		return ListSnapshot{}, failure(Invalid)
	}
	return execute(s, ctx, authv1.ScopeOperation_SCOPE_OPERATION_READ, object, false,
		func(ctx context.Context, tx pgx.Tx, _ authorization.RightsEvidence, _ authorization.ListSelection) (ListSnapshot, error) {
			return readList(ctx, tx, id, false)
		}, nil)
}

// The explicit token is computed only from rows selected by the admitted ID
// predicate. The catalog share lock held by ListLists keeps it coherent with
// each page while writes, including unrelated ones, wait for that transaction.
func authorizedView(ctx context.Context, tx pgx.Tx, selection authorization.ListSelection) (cursorViewKind, [sha256.Size]byte, error) {
	if selection.All() {
		return cursorViewAll, [sha256.Size]byte{}, nil
	}
	ids, err := selection.IDs()
	if err != nil {
		return cursorViewInvalid, [sha256.Size]byte{}, failure(Denied)
	}
	if len(ids) > authorization.MaxLists {
		return cursorViewInvalid, [sha256.Size]byte{}, failure(Capacity)
	}
	texts := make([]string, 0, len(ids))
	for _, id := range ids {
		text, valueErr := id.Value()
		if valueErr != nil {
			return cursorViewInvalid, [sha256.Size]byte{}, failure(Denied)
		}
		texts = append(texts, text)
	}
	rows, err := tx.Query(ctx, `SELECT list_id,revision FROM public.homeward_lists WHERE lifecycle=1 AND list_id=ANY($1::text[]) ORDER BY list_id`, texts)
	if err != nil {
		return cursorViewInvalid, [sha256.Size]byte{}, err
	}
	hash := sha256.New()
	var size [8]byte
	for rows.Next() {
		var id string
		var revision int64
		if scanErr := rows.Scan(&id, &revision); scanErr != nil {
			rows.Close()
			return cursorViewInvalid, [sha256.Size]byte{}, scanErr
		}
		if _, parseErr := domain.ParseListID(id); parseErr != nil || revision <= 0 {
			rows.Close()
			return cursorViewInvalid, [sha256.Size]byte{}, failure(Unavailable)
		}
		binary.BigEndian.PutUint64(size[:], uint64(len(id)))
		hash.Write(size[:])
		hash.Write([]byte(id))
		binary.BigEndian.PutUint64(size[:], uint64(revision))
		hash.Write(size[:])
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return cursorViewInvalid, [sha256.Size]byte{}, err
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return cursorViewExplicit, digest, nil
}
func (s *Service) ListLists(ctx context.Context, cursor ListCursor, limit int) (ListPage, error) {
	if limit < 1 || limit > MaxPage {
		return ListPage{}, failure(Invalid)
	}
	return execute(s, ctx, authv1.ScopeOperation_SCOPE_OPERATION_READ, collection(), false,
		func(ctx context.Context, tx pgx.Tx, e authorization.RightsEvidence, selection authorization.ListSelection) (ListPage, error) {
			revision, err := readCatalog(ctx, tx)
			if err != nil {
				return ListPage{}, err
			}
			kind, view, err := authorizedView(ctx, tx, selection)
			if err != nil {
				return ListPage{}, err
			}
			rights, semantic, err := e.Binding()
			if err != nil {
				return ListPage{}, failure(Denied)
			}
			last := ""
			if cursor.valid {
				if cursor.kind != kind || cursor.rights != rights || cursor.semantic != semantic ||
					(kind == cursorViewAll && cursor.revision != revision) ||
					(kind == cursorViewExplicit && cursor.view != view) {
					return ListPage{}, failure(StaleCursor)
				}
				last, err = cursor.last.Value()
				if err != nil {
					return ListPage{}, failure(Invalid)
				}
			}
			ids := make([]domain.ListID, 0, limit+1)
			var rows pgx.Rows
			if selection.All() {
				rows, err = tx.Query(ctx, `SELECT list_id FROM public.homeward_lists WHERE lifecycle=1 AND list_id>$1 ORDER BY list_id LIMIT $2`, last, limit+1)
			} else {
				allowed, selectionErr := selection.IDs()
				if selectionErr != nil {
					return ListPage{}, failure(Denied)
				}
				texts := make([]string, 0, len(allowed))
				for _, id := range allowed {
					text, _ := id.Value()
					texts = append(texts, text)
				}
				rows, err = tx.Query(ctx, `SELECT list_id FROM public.homeward_lists WHERE lifecycle=1 AND list_id=ANY($1::text[]) AND list_id>$2 ORDER BY list_id LIMIT $3`, texts, last, limit+1)
			}
			if err != nil {
				return ListPage{}, err
			}
			for rows.Next() {
				var text string
				if scanErr := rows.Scan(&text); scanErr != nil {
					rows.Close()
					return ListPage{}, scanErr
				}
				id, parseErr := domain.ParseListID(text)
				if parseErr != nil {
					rows.Close()
					return ListPage{}, failure(Unavailable)
				}
				ids = append(ids, id)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return ListPage{}, err
			}
			page := ListPage{entries: make([]ListSnapshot, 0, min(len(ids), limit))}
			for index, id := range ids {
				if index >= limit {
					break
				}
				snapshot, readErr := readList(ctx, tx, id, false)
				if readErr != nil {
					return ListPage{}, readErr
				}
				page.entries = append(page.entries, snapshot)
			}
			if len(ids) > limit {
				cursorRevision := int64(0)
				if kind == cursorViewAll {
					cursorRevision = revision
				}
				page.next = ListCursor{valid: true, kind: kind, revision: cursorRevision, view: view, last: ids[limit-1], rights: rights, semantic: semantic}
			}
			return page, nil
		}, nil)
}
func (s *Service) CountLists(ctx context.Context) (uint64, error) {
	return execute(s, ctx, authv1.ScopeOperation_SCOPE_OPERATION_READ, collection(), false,
		func(ctx context.Context, tx pgx.Tx, _ authorization.RightsEvidence, selection authorization.ListSelection) (uint64, error) {
			if _, err := readCatalog(ctx, tx); err != nil {
				return 0, err
			}
			var count int64
			var err error
			if selection.All() {
				err = tx.QueryRow(ctx, `SELECT count(*) FROM public.homeward_lists WHERE lifecycle=1`).Scan(&count)
			} else {
				allowed, selectionErr := selection.IDs()
				if selectionErr != nil {
					return 0, failure(Denied)
				}
				texts := make([]string, 0, len(allowed))
				for _, id := range allowed {
					text, _ := id.Value()
					texts = append(texts, text)
				}
				err = tx.QueryRow(ctx, `SELECT count(*) FROM public.homeward_lists WHERE lifecycle=1 AND list_id=ANY($1::text[])`, texts).Scan(&count)
			}
			if err != nil || count < 0 {
				return 0, failure(Unavailable)
			}
			return uint64(count), nil
		}, nil)
}

func (s *Service) RenameList(ctx context.Context, id domain.ListID, expected ListRevision, name string) (MutationReceipt, error) {
	if !validText(name, MaxNameBytes, true) {
		return MutationReceipt{}, failure(Invalid)
	}
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
			text, _ := id.Value()
			_, number, _ := current.revision.Values()
			_, err = tx.Exec(ctx, `UPDATE public.homeward_lists SET name=$2,revision=revision+1,updated_at=now() WHERE list_id=$1`, text, name)
			if err != nil {
				return MutationReceipt{}, err
			}
			if err := bumpCatalog(ctx, tx); err != nil {
				return MutationReceipt{}, err
			}
			return listReceipt(id, number+1), nil
		}, func(r MutationReceipt) { afterListCommit(s, r) }, func(r MutationReceipt) { s.resyncList(r.list) })
}
func (s *Service) DeleteList(ctx context.Context, id domain.ListID, expected ListRevision) (MutationReceipt, error) {
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
			text, _ := id.Value()
			_, number, _ := current.revision.Values()
			_, err = tx.Exec(ctx, `UPDATE public.homeward_lists SET lifecycle=2,revision=revision+1,updated_at=now() WHERE list_id=$1`, text)
			if err != nil {
				return MutationReceipt{}, err
			}
			if err := bumpCatalog(ctx, tx); err != nil {
				return MutationReceipt{}, err
			}
			return listReceipt(id, number+1), nil
		}, func(r MutationReceipt) { afterListCommit(s, r) }, func(r MutationReceipt) { s.resyncList(r.list) })
}
