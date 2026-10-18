package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestNHA020TransactionOutcomes(t *testing.T) {
	port := testCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, testSettings(t, port))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.pool.Exec(ctx, `CREATE TABLE public.nha020_outcome(value integer NOT NULL)`); err != nil {
		t.Fatal(err)
	}

	outcome, err := store.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO public.nha020_outcome VALUES (1)`)
		return err
	})
	if outcome != TransactionCommitted || err != nil {
		t.Fatalf("committed outcome: %v %v", outcome, err)
	}

	marker := errors.New("callback rejected")
	outcome, err = store.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO public.nha020_outcome VALUES (2)`); err != nil {
			return err
		}
		return marker
	})
	if outcome != TransactionNotCommitted || !errors.Is(err, marker) {
		t.Fatalf("callback rollback outcome: %v %v", outcome, err)
	}

	beginCtx, stopBegin := context.WithCancel(ctx)
	stopBegin()
	called := false
	outcome, err = store.InTransactionOutcome(beginCtx, func(pgx.Tx) error {
		called = true
		return nil
	})
	if outcome != TransactionNotCommitted || !errors.Is(err, context.Canceled) || called {
		t.Fatalf("impossible Begin outcome: %v %v called=%t", outcome, err, called)
	}

	// PostgreSQL aborts the transaction after an SQL error. Its ROLLBACK
	// response to Commit is pgx.ErrTxCommitRollback: the absence of a commit
	// is known, even though Commit itself returned an error.
	outcome, err = store.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		_, statementErr := tx.Exec(ctx, `INSERT INTO public.nha020_outcome VALUES (3)`)
		if statementErr != nil {
			return statementErr
		}
		_, statementErr = tx.Exec(ctx, `SELECT 1 / 0`)
		if statementErr == nil {
			t.Fatal("expected statement failure")
		}
		return nil
	})
	if outcome != TransactionNotCommitted || !errors.Is(err, pgx.ErrTxCommitRollback) {
		t.Fatalf("explicit server rollback outcome: %v %v", outcome, err)
	}

	var values []int
	rows, err := store.pool.Query(ctx, `SELECT value FROM public.nha020_outcome ORDER BY value`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var value int
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0] != 1 {
		t.Fatalf("transaction contents: %v", values)
	}
}

func TestNHA020CommitFailureClassification(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	outcome, err := failedCommit(ctx, context.Canceled)
	if outcome != TransactionIndeterminate || !errors.Is(err, context.Canceled) {
		t.Fatalf("commit cancellation outcome: %v %v", outcome, err)
	}
	transportFailure := errors.New("transport failed at commit")
	outcome, err = failedCommit(context.Background(), transportFailure)
	if outcome != TransactionIndeterminate || !errors.Is(err, transportFailure) {
		t.Fatalf("commit transport failure outcome: %v %v", outcome, err)
	}
	outcome, err = failedCommit(context.Background(), pgx.ErrTxCommitRollback)
	if outcome != TransactionNotCommitted || !errors.Is(err, pgx.ErrTxCommitRollback) {
		t.Fatalf("known rollback outcome: %v %v", outcome, err)
	}
}
