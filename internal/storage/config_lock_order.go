package storage

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Every ConfigTx writer takes this coordination prefix before 151, including
// native bootstrap and account/MFA-policy callbacks. It admits no user and does
// not need a principal. The tables are created by the completed migration plan;
// the initial MFA-policy ROW may still be absent during BootstrapMFAPolicy.
func lockConfigSecurityPrefix(ctx context.Context, tx pgx.Tx) error {
	if ctx == nil || tx == nil {
		return &Error{kind: ConfigInvalid}
	}
	if _, err := lockAuthBootstrapOnly(ctx, tx); err != nil {
		return err
	}
	var singleton bool
	err := tx.QueryRow(ctx, `SELECT singleton FROM public.auth_mfa_policy WHERE singleton FOR UPDATE`).Scan(&singleton)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !singleton {
		return &Error{kind: ConfigIntegrity}
	}
	return nil
}
