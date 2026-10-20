package storage

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type HistoryPurgeResult struct {
	Cutoff                                        time.Time
	DeletedRecords, DeletedLosses, DeletedBatches int64
	MoreExpired                                   bool
}

// PurgeExecutionHistory advances the UTC retention fence and removes at most
// 512 rows in one transaction. The writer uses the same locked head row, so a
// delayed batch cannot resurrect data older than the advanced cutoff.
func (s *Store) PurgeExecutionHistory(ctx context.Context, cutoff time.Time) (HistoryPurgeResult, error) {
	if s == nil || s.pool == nil || !validHistoryContext(ctx) || cutoff.IsZero() || cutoff.Location() != time.UTC || cutoff.Year() < 2000 || cutoff.Year() > 9999 {
		return HistoryPurgeResult{}, ErrHistoryInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var result HistoryPurgeResult
	outcome, err := s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		if e := tx.QueryRow(ctx, `SELECT retention_cutoff FROM public.execution_history_head WHERE singleton=true FOR UPDATE`).Scan(&result.Cutoff); e != nil {
			return e
		}
		cutoff = cutoff.Truncate(time.Microsecond)
		if cutoff.After(result.Cutoff) {
			result.Cutoff = cutoff
			if _, e := tx.Exec(ctx, `UPDATE public.execution_history_head SET retention_cutoff=$1 WHERE singleton=true`, cutoff); e != nil {
				return e
			}
		}
		// Ordered key selection is bounded. No OFFSET scan is used; repeated
		// calls continue from the first surviving expired key.
		if e := tx.QueryRow(ctx, `WITH selected AS (
			SELECT epoch,ordinal FROM public.execution_history_records WHERE emission_time < $1
			ORDER BY emission_time,epoch,ordinal LIMIT 512
		), removed AS (
			DELETE FROM public.execution_history_records r USING selected s
			WHERE r.epoch=s.epoch AND r.ordinal=s.ordinal RETURNING 1
		) SELECT count(*) FROM removed`, result.Cutoff).Scan(&result.DeletedRecords); e != nil {
			return e
		}
		remaining := 512 - result.DeletedRecords
		if remaining > 0 {
			if e := tx.QueryRow(ctx, `WITH selected AS (
				SELECT epoch,batch_number,reason FROM public.execution_history_loss WHERE emission_time < $1
				ORDER BY emission_time,epoch,batch_number,reason LIMIT $2
			), removed AS (
				DELETE FROM public.execution_history_loss l USING selected s WHERE l.epoch=s.epoch AND l.batch_number=s.batch_number AND l.reason=s.reason RETURNING 1
			) SELECT count(*) FROM removed`, result.Cutoff, remaining).Scan(&result.DeletedLosses); e != nil {
				return e
			}
		}
		remaining -= result.DeletedLosses
		if remaining > 0 {
			if e := tx.QueryRow(ctx, `WITH selected AS (
				SELECT epoch,batch_number FROM public.execution_history_batches WHERE retention_time < $1
				ORDER BY retention_time,epoch,batch_number LIMIT $2
			), removed AS (
				DELETE FROM public.execution_history_batches b USING selected s WHERE b.epoch=s.epoch AND b.batch_number=s.batch_number RETURNING 1
			) SELECT count(*) FROM removed`, result.Cutoff, remaining).Scan(&result.DeletedBatches); e != nil {
				return e
			}
		}
		return tx.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM public.execution_history_records WHERE emission_time < $1
			UNION ALL SELECT 1 FROM public.execution_history_loss WHERE emission_time < $1
			UNION ALL SELECT 1 FROM public.execution_history_batches WHERE retention_time < $1
		)`, result.Cutoff).Scan(&result.MoreExpired)
	})
	if err != nil {
		if outcome == TransactionIndeterminate {
			return HistoryPurgeResult{}, ErrHistoryCommitUnknown
		}
		return HistoryPurgeResult{}, historyFailure(ErrHistoryUnavailable, err)
	}
	return result, nil
}
