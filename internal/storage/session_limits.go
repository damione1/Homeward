package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/damione1/Homeward/internal/accounts"
	"github.com/jackc/pgx/v5"
)

const loginAttemptWindow = 15 * time.Minute
const loginAttemptMax = 5
const loginAttemptKeyMax = 4096

var ErrLoginRateLimited = errors.New("sessions: login temporarily limited")

func loginLimitDigest(login accounts.LoginName) ([32]byte, error) {
	_, key, err := login.Values()
	if err != nil {
		return [32]byte{}, ErrSessionInvalid
	}
	return sha256.Sum256([]byte("homeward/login-limit/v1" + key)), nil
}

// ReserveLoginAttempt persists the exact timestamps of the last five starts.
// It commits before any password KDF; a denied or canceled KDF consumes a slot.
func (s *Store) ReserveLoginAttempt(ctx context.Context, login accounts.LoginName, now time.Time) error {
	digest, err := loginLimitDigest(login)
	if err != nil || now.IsZero() {
		return ErrSessionInvalid
	}
	_, err = sessionTransaction(s, ctx, func(tx pgx.Tx) (struct{}, error) {
		// The existing singleton serializes table capacity checks and insertions;
		// it is released at commit, before any password derivation.
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return struct{}{}, ErrSessionUnavailable
		}
		cutoff := now.Add(-loginAttemptWindow)
		if _, err := tx.Exec(ctx, `DELETE FROM public.auth_login_attempts a
            WHERE NOT EXISTS (SELECT 1 FROM unnest(a.attempts) AS t(ts) WHERE t.ts>$1)`, cutoff); err != nil {
			return struct{}{}, ErrSessionUnavailable
		}
		var timestamps []time.Time
		err := tx.QueryRow(ctx, `SELECT attempts FROM public.auth_login_attempts WHERE login_digest=$1 FOR UPDATE`, digest[:]).Scan(&timestamps)
		if errors.Is(err, pgx.ErrNoRows) {
			var count int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM public.auth_login_attempts`).Scan(&count); err != nil {
				return struct{}{}, ErrSessionUnavailable
			}
			if count >= loginAttemptKeyMax {
				return struct{}{}, ErrLoginRateLimited
			}
			timestamps = nil
		} else if err != nil {
			return struct{}{}, ErrSessionUnavailable
		}
		kept := make([]time.Time, 0, loginAttemptMax)
		for _, timestamp := range timestamps {
			if timestamp.After(cutoff) {
				kept = append(kept, timestamp)
			}
		}
		if len(kept) >= loginAttemptMax {
			return struct{}{}, ErrLoginRateLimited
		}
		kept = append(kept, now)
		if _, err := tx.Exec(ctx, `INSERT INTO public.auth_login_attempts(login_digest,attempts) VALUES($1,$2)
            ON CONFLICT(login_digest) DO UPDATE SET attempts=EXCLUDED.attempts`, digest[:], kept); err != nil {
			return struct{}{}, ErrSessionUnavailable
		}
		return struct{}{}, nil
	})
	return err
}

func clearLoginAttempts(ctx context.Context, tx pgx.Tx, login accounts.LoginName) error {
	digest, err := loginLimitDigest(login)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM public.auth_login_attempts WHERE login_digest=$1`, digest[:])
	if err != nil {
		return ErrSessionUnavailable
	}
	return nil
}

func (s *Store) RecordLoginDenial(ctx context.Context, limited bool) error {
	if err := accountContextError(ctx); err != nil || s == nil || s.pool == nil {
		return ErrSessionInvalid
	}
	action := authActionLoginDenied
	if limited {
		action = authActionLoginLimited
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO public.auth_audit(action,outcome) VALUES($1,$2)`, int16(action), authOutcomeDenied)
	if err != nil {
		return ErrSessionUnavailable
	}
	return nil
}
