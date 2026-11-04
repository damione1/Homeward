package lists

import (
	"context"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/jackc/pgx/v5"
)

// CheckListUpdate implements an empty PATCH: current write authority and CAS
// are checked under the existing SQL fence, but no revision is advanced.
func (s *Service) CheckListUpdate(ctx context.Context, id domain.ListID, expected ListPrecondition) (ListSnapshot, error) {
	object, err := listObject(id)
	if err != nil {
		return ListSnapshot{}, failure(Invalid)
	}
	return execute(s, ctx, auth.ScopeOperation_SCOPE_OPERATION_WRITE, object, false, func(ctx context.Context, tx pgx.Tx, _ authorization.RightsEvidence, _ authorization.ListSelection) (ListSnapshot, error) {
		current, err := readList(ctx, tx, id, false)
		if err != nil {
			return ListSnapshot{}, err
		}
		if err := expectList(ctx, tx, id, expected, current); err != nil {
			return ListSnapshot{}, err
		}
		return current, nil
	}, nil)
}

// CheckItemUpdate is the corresponding no-op for a task's compound etag.
func (s *Service) CheckItemUpdate(ctx context.Context, id domain.ListID, uid ItemUID, list ListPrecondition, item ItemPrecondition) (ItemSnapshot, error) {
	object, err := listObject(id)
	if err != nil {
		return ItemSnapshot{}, failure(Invalid)
	}
	return execute(s, ctx, auth.ScopeOperation_SCOPE_OPERATION_WRITE, object, false, func(ctx context.Context, tx pgx.Tx, _ authorization.RightsEvidence, _ authorization.ListSelection) (ItemSnapshot, error) {
		current, err := readList(ctx, tx, id, false)
		if err != nil {
			return ItemSnapshot{}, err
		}
		if err := expectList(ctx, tx, id, list, current); err != nil {
			return ItemSnapshot{}, err
		}
		value, err := readItem(ctx, tx, id, uid, false)
		if err != nil {
			return ItemSnapshot{}, err
		}
		value.listRevision = current.revision
		if err := expectItem(id, uid, item, value); err != nil {
			return ItemSnapshot{}, err
		}
		value.listRevision = current.revision
		return value, nil
	}, nil)
}
