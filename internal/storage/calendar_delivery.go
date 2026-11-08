package storage

import (
	"context"
	"github.com/jackc/pgx/v5"
	"time"
)

const calendarDeliverySchemaSQL = `
CREATE TABLE public.calendar_delivery_claims (
 identity bytea PRIMARY KEY CHECK(octet_length(identity)=32),
 claimed_at timestamptz NOT NULL
);
CREATE TABLE public.calendar_coverage (
 source text PRIMARY KEY CHECK(octet_length(source) BETWEEN 1 AND 256),
 through_at timestamptz NOT NULL
);`

// ClaimCalendarDelivery never treats an uncertain COMMIT as permission to emit.
// Claims survive source revisions and are deliberately not time-evicted: a
// moved occurrence must not become a new delivery after a process restart.
func (s *Store) ClaimCalendarDelivery(ctx context.Context, key [32]byte, at time.Time) (bool, error) {
	if key == ([32]byte{}) || at.IsZero() {
		return false, &Error{kind: InvalidSettings}
	}
	claimed := false
	outcome, err := s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO public.calendar_delivery_claims(identity,claimed_at) VALUES($1,$2) ON CONFLICT DO NOTHING`, key[:], at)
		claimed = tag.RowsAffected() == 1
		return err
	})
	return claimed && outcome == TransactionCommitted && err == nil, err
}

// AdvanceCalendarCoverage atomically returns the previous lower bound. It
// records only observed present time, never a future projected window end.
func (s *Store) AdvanceCalendarCoverage(ctx context.Context, source string, at time.Time) (time.Time, error) {
	if !remoteText(source, 256, true) || at.IsZero() {
		return time.Time{}, &Error{kind: InvalidSettings}
	}
	var previous time.Time
	err := s.InTransaction(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO public.calendar_coverage(source,through_at) VALUES($1,$2) ON CONFLICT DO NOTHING`, source, at); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT through_at FROM public.calendar_coverage WHERE source=$1 FOR UPDATE`, source).Scan(&previous); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE public.calendar_coverage SET through_at=GREATEST(through_at,$2) WHERE source=$1`, source, at)
		return err
	})
	return previous, err
}

func (s *Store) ReadCalendarCoverage(ctx context.Context, source string) (time.Time, error) {
	if !remoteText(source, 256, true) {
		return time.Time{}, &Error{kind: InvalidSettings}
	}
	var previous time.Time
	err := s.InTransaction(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT through_at FROM public.calendar_coverage WHERE source=$1`, source).Scan(&previous)
		if err == pgx.ErrNoRows {
			return nil
		}
		return err
	})
	return previous, err
}

// CommitCalendarCoverage atomically persists every classified missed identity
// with the safe processed watermark. A crash before result draining cannot
// erase a consumed occurrence while retaining later coverage.
func (s *Store) CommitCalendarCoverage(ctx context.Context, source string, through time.Time, keys [][32]byte) error {
	if !remoteText(source, 256, true) || through.IsZero() || len(keys) > 4096 {
		return &Error{kind: InvalidSettings}
	}
	raw := make([][]byte, len(keys))
	for i, key := range keys {
		if key == ([32]byte{}) {
			return &Error{kind: InvalidSettings}
		}
		raw[i] = append([]byte(nil), key[:]...)
	}
	return s.InTransaction(ctx, func(tx pgx.Tx) error {
		if len(raw) > 0 {
			if _, err := tx.Exec(ctx, `INSERT INTO public.calendar_delivery_claims(identity,claimed_at) SELECT unnest($1::bytea[]),$2 ON CONFLICT DO NOTHING`, raw, through); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO public.calendar_coverage(source,through_at) VALUES($1,$2) ON CONFLICT(source) DO UPDATE SET through_at=GREATEST(calendar_coverage.through_at,EXCLUDED.through_at)`, source, through)
		return err
	})
}
