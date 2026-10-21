package storage

import (
	"context"
	"errors"
	"time"

	"github.com/damione1/Homeward/internal/recordermaintenance"
	"github.com/jackc/pgx/v5"
)

// This advisory lock is only maintenance metadata serialization, never a command
// ledger or an operational mutex. Every raw writer acquires it before epoch locks.
const recorderMaintenanceLock = `SELECT pg_advisory_xact_lock(481316,32)`

// Reserve a quarter of the existing transaction deadline for progress/CAS and
// COMMIT instead of restarting the same expensive prefix after each timeout.
func maintenanceBudgetLow(ctx context.Context) bool {
	deadline, ok := ctx.Deadline()
	return ok && time.Until(deadline) < recordermaintenance.AttemptTimeout/4
}

func maintenanceFailure(outcome TransactionOutcome, err error) error {
	if err == nil {
		return nil
	}
	if outcome == TransactionIndeterminate {
		return recordermaintenance.ErrCommitUnknown
	}
	if errors.Is(err, recordermaintenance.ErrInvalid) || errors.Is(err, recordermaintenance.ErrIntegrity) || errors.Is(err, recordermaintenance.ErrBudget) {
		return err
	}
	return recordermaintenance.ErrUnavailable
}

// Maintain implements the internal worker port with independent class commits.
// Each class gets one bounded attempt even if another class fails. Counts are
// returned only for committed operations; an ambiguous outcome remains an error.
func (s *Store) Maintain(ctx context.Context, asOf time.Time, b recordermaintenance.Boundary) (recordermaintenance.CycleResult, error) {
	var result recordermaintenance.CycleResult
	if s == nil || s.pool == nil || !validHistoryContext(ctx) || asOf.IsZero() || asOf.Location() != time.UTC {
		return result, recordermaintenance.ErrInvalid
	}
	if _, _, _, _, err := b.Components(); err != nil {
		return result, err
	}
	asOf = asOf.Truncate(time.Microsecond)
	civil, err := recordermaintenance.CivilCutoff(asOf)
	if err != nil {
		return result, err
	}
	var errs []error
	n, more, e := s.AggregateRecorderPrefix(ctx, b)
	result.Processed = n
	result.More = more
	errs = append(errs, e)
	n, more, e = s.PurgeRecorderRaw(ctx, asOf.Add(-7*24*time.Hour))
	result.DeletedRaw = n
	result.More = result.More || more
	errs = append(errs, e)
	trace, e := s.PurgeExecutionHistory(ctx, asOf.Add(-7*24*time.Hour))
	if e == nil {
		result.DeletedTraces = trace.DeletedRecords + trace.DeletedLosses + trace.DeletedBatches
		result.More = result.More || trace.MoreExpired
	}
	errs = append(errs, e)
	n, more, e = s.PurgeRecorderAggregates(ctx, civil)
	result.DeletedAggregates = n
	result.More = result.More || more
	errs = append(errs, e)
	n, more, e = s.PurgeRecorderAudit(ctx, asOf.Add(-90*24*time.Hour))
	result.DeletedAudit = n
	result.More = result.More || more
	errs = append(errs, e)
	return result, errors.Join(errs...)
}

func (s *Store) maintenanceTx(ctx context.Context, fn func(context.Context, pgx.Tx) error) (TransactionOutcome, error) {
	if s == nil || s.pool == nil || !validHistoryContext(ctx) {
		return TransactionNotCommitted, recordermaintenance.ErrInvalid
	}
	bounded, cancel := context.WithTimeout(ctx, recordermaintenance.AttemptTimeout)
	defer cancel()
	tx, err := s.pool.BeginTx(bounded, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return TransactionNotCommitted, recordermaintenance.ErrUnavailable
	}
	defer func() { _ = tx.Rollback(bounded) }()
	if _, err = tx.Exec(bounded, recorderMaintenanceLock); err != nil {
		return TransactionNotCommitted, err
	}
	if err = fn(bounded, tx); err != nil {
		return TransactionNotCommitted, err
	}
	if err = tx.Commit(bounded); err != nil {
		return failedCommit(bounded, err)
	}
	return TransactionCommitted, nil
}
