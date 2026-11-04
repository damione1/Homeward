package authorization

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// FenceSQL locks the committed account then active policy before any protected
// domain rows. It never calls the in-memory authority while SQL locks are held.
// A rights snapshot is evidence, not a permission to choose another operation.
func FenceSQL(ctx context.Context, tx pgx.Tx, evidence RightsEvidence) error {
	if ctx == nil || tx == nil {
		return ErrDenied
	}
	account, err := evidence.Account()
	if err != nil {
		return ErrDenied
	}
	id, err := account.Value()
	if err != nil {
		return ErrDenied
	}
	var role, status int16
	var revocation, rights int64
	var semantic []byte
	err = tx.QueryRow(ctx, `SELECT role,status,revocation_revision FROM public.auth_accounts WHERE id=$1 FOR SHARE`, id).Scan(&role, &status, &revocation)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `SELECT rights_revision,semantic_sha256 FROM public.authorization_policy_active WHERE account_id=$1 FOR SHARE`, id).Scan(&rights, &semantic)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	if !evidence.MatchesSQL(role, status, revocation, rights, semantic) {
		return ErrDenied
	}
	return nil
}
