package storage

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// CloudAttemptQuota reserves each provider attempt durably before I/O. It
// counts Homeward only; consumption by HA or other clients is unknowable.
type CloudAttemptQuota struct {
	store   *Store
	account string
	limit   int64
	zone    *time.Location
}

func NewCloudAttemptQuota(store *Store, account string, limit int, zone string) (*CloudAttemptQuota, error) {
	if store == nil || store.pool == nil || account == "" || len(account) > 128 || !utf8.ValidString(account) || limit < 1 || limit > 30000 {
		return nil, &Error{kind: QuotaInvalid}
	}
	for _, r := range account {
		if r < 0x20 || r == 0x7f {
			return nil, &Error{kind: QuotaInvalid}
		}
	}
	location, err := time.LoadLocation(zone)
	if err != nil || zone == "" || zone == "Local" {
		return nil, &Error{kind: QuotaInvalid}
	}
	return &CloudAttemptQuota{store: store, account: account, limit: int64(limit), zone: location}, nil
}

func (q *CloudAttemptQuota) bounds(now time.Time) (time.Time, time.Time) {
	local := now.In(q.zone)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, q.zone)
	return start, start.AddDate(0, 0, 1)
}

// ReserveAttempt returns a nonzero retry time when either the configured
// civil day or conservative rolling 24h window has reached the local limit.
// Any uncertain commit fails closed, even if it may have consumed a slot.
func (q *CloudAttemptQuota) ReserveAttempt(ctx context.Context) (time.Time, error) {
	if q == nil || q.store == nil || ctx == nil {
		return time.Time{}, &Error{kind: QuotaInvalid}
	}
	var retry time.Time
	var callbackErr error
	outcome, err := q.store.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		digest := sha256.Sum256([]byte("homeward/cloud-quota/v1\x00" + q.account))
		lock := int64(binary.BigEndian.Uint64(digest[:8]))
		if _, e := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lock); e != nil {
			callbackErr = &Error{kind: QuotaUnavailable}
			return callbackErr
		}
		var now time.Time
		if e := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); e != nil {
			callbackErr = &Error{kind: QuotaUnavailable}
			return callbackErr
		}
		dayStart, nextDay := q.bounds(now)
		var dayCount, rollingCount int64
		var oldest *time.Time
		if e := tx.QueryRow(ctx, `SELECT count(*) FROM public.cloud_request_reservations WHERE account_id=$1 AND reserved_at >= $2 AND reserved_at < $3`, q.account, dayStart, nextDay).Scan(&dayCount); e != nil {
			callbackErr = &Error{kind: QuotaUnavailable}
			return callbackErr
		}
		if e := tx.QueryRow(ctx, `SELECT count(*),min(reserved_at) FROM public.cloud_request_reservations WHERE account_id=$1 AND reserved_at > $2`, q.account, now.Add(-24*time.Hour)).Scan(&rollingCount, &oldest); e != nil {
			callbackErr = &Error{kind: QuotaUnavailable}
			return callbackErr
		}
		if dayCount >= q.limit {
			retry = nextDay
		}
		if rollingCount >= q.limit && oldest != nil {
			until := oldest.Add(24 * time.Hour)
			if until.After(retry) {
				retry = until
			}
		}
		if !retry.IsZero() {
			return nil
		}
		if _, e := tx.Exec(ctx, `INSERT INTO public.cloud_request_reservations(account_id,reserved_at) VALUES($1,$2)`, q.account, now); e != nil {
			callbackErr = &Error{kind: QuotaUnavailable}
			return callbackErr
		}
		return nil
	})
	if outcome == TransactionIndeterminate {
		return time.Time{}, &Error{kind: QuotaCommitUnknown}
	}
	if err != nil {
		if callbackErr != nil {
			return time.Time{}, callbackErr
		}
		return time.Time{}, &Error{kind: QuotaUnavailable}
	}
	return retry, nil
}

// Today counts committed Homeward reservations in the configured civil day.
func (q *CloudAttemptQuota) Today(ctx context.Context) (int64, error) {
	if q == nil || q.store == nil || ctx == nil {
		return 0, &Error{kind: QuotaInvalid}
	}
	var now time.Time
	if err := q.store.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return 0, &Error{kind: QuotaUnavailable}
	}
	start, next := q.bounds(now)
	var count int64
	if err := q.store.pool.QueryRow(ctx, `SELECT count(*) FROM public.cloud_request_reservations WHERE account_id=$1 AND reserved_at >= $2 AND reserved_at < $3`, q.account, start, next).Scan(&count); err != nil {
		return 0, &Error{kind: QuotaUnavailable}
	}
	return count, nil
}
