package storage

import (
	"context"
	"github.com/jackc/pgx/v5"
)

// Rows are exposed only for a confirmed commit. CompletionUnknown never
// becomes a confirmed count, and this UPDATE never dispatches a driver.
func (s *Store) RecoverCommandIntentsCounted(ctx context.Context, runtime *CommandRuntime, response []byte) (uint64, TransactionOutcome, error) {
	if len(response) == 0 || runtime == nil || runtime.owner != s {
		return 0, TransactionNotCommitted, &Error{kind: InvalidSettings}
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if err := runtime.pingLocked(ctx); err != nil {
		return 0, TransactionNotCommitted, err
	}
	var rows int64
	outcome, err := s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE public.command_intents SET state='indeterminate', response=$1, result_error='indeterminate', updated_at=now() WHERE state='dispatching'`, response)
		if err == nil {
			rows = tag.RowsAffected()
		}
		return err
	})
	if outcome != TransactionCommitted || err != nil || rows < 0 {
		return 0, outcome, err
	}
	return uint64(rows), outcome, nil
}
