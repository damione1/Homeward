package storage

import (
	"context"
	"errors"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/jackc/pgx/v5"
)

const challengeLifetime = 5 * time.Minute
const challengeProofLimit = 5

type ChallengeCreation struct {
	ID       contracts.ChallengeID
	Account  contracts.AccountID
	Revision RevocationRevision
	Payload  []byte
	Now      time.Time
}

type ChallengeRecord struct {
	ID        contracts.ChallengeID
	Account   contracts.AccountID
	Revision  RevocationRevision
	Payload   []byte
	ExpiresAt time.Time
	Attempts  int
}

func (s *Store) IssueChallenge(ctx context.Context, input ChallengeCreation) (ChallengeRecord, error) {
	id, err := input.ID.Value()
	if err != nil || len(id) > 128 || !input.Revision.validFor(input.Account) ||
		len(input.Payload) == 0 || len(input.Payload) > 16384 || input.Now.IsZero() {
		return ChallengeRecord{}, ErrSessionInvalid
	}
	account, err := sqlAccountID(input.Account)
	if err != nil {
		return ChallengeRecord{}, ErrSessionInvalid
	}
	payload := append([]byte(nil), input.Payload...)
	return sessionTransaction(s, ctx, func(tx pgx.Tx) (ChallengeRecord, error) {
		var revision int64
		var status int16
		if err := tx.QueryRow(ctx, `SELECT revocation_revision,status FROM public.auth_accounts WHERE id=$1 FOR UPDATE`, account).Scan(&revision, &status); err != nil {
			return ChallengeRecord{}, ErrSessionNotFound
		}
		if status != 1 || revision != input.Revision.number {
			return ChallengeRecord{}, ErrSessionNotFound
		}
		expires := input.Now.Add(challengeLifetime)
		if _, err := tx.Exec(ctx, `INSERT INTO public.auth_login_challenges(account_id,id,account_revision,options,expires_at,attempts)
            VALUES($1,$2,$3,$4,$5,0) ON CONFLICT(account_id) DO UPDATE
            SET id=EXCLUDED.id,account_revision=EXCLUDED.account_revision,options=EXCLUDED.options,
                expires_at=EXCLUDED.expires_at,attempts=0,consumed_at=NULL`, account, id, revision, payload, expires); err != nil {
			return ChallengeRecord{}, ErrSessionUnavailable
		}
		if err := insertSessionAudit(ctx, tx, input.Account, authActionChallengeIssued, authOutcomeAdmitted); err != nil {
			return ChallengeRecord{}, err
		}
		return ChallengeRecord{ID: input.ID, Account: input.Account, Revision: input.Revision, Payload: append([]byte(nil), payload...), ExpiresAt: expires}, nil
	})
}

// LookupChallenge is preliminary: CompleteLogin must recheck and consume the
// same row under the account lock after the provider returns.
func (s *Store) LookupChallenge(ctx context.Context, id contracts.ChallengeID, now time.Time) (ChallengeRecord, error) {
	value, err := id.Value()
	if err != nil || now.IsZero() || accountContextError(ctx) != nil || s == nil || s.pool == nil {
		return ChallengeRecord{}, ErrSessionInvalid
	}
	var accountText string
	var payload []byte
	var expires time.Time
	var attempts int
	var revision int64
	err = s.pool.QueryRow(ctx, `SELECT account_id,account_revision,options,expires_at,attempts FROM public.auth_login_challenges
		WHERE id=$1 AND consumed_at IS NULL`, value).Scan(&accountText, &revision, &payload, &expires, &attempts)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !now.Before(expires) {
		return ChallengeRecord{}, ErrSessionNotFound
	}
	if err != nil {
		return ChallengeRecord{}, ErrSessionUnavailable
	}
	account, err := contracts.ParseAccountID(accountText)
	if err != nil || revision <= 0 || len(payload) == 0 || len(payload) > 16384 || attempts < 0 || attempts > challengeProofLimit {
		return ChallengeRecord{}, ErrSessionUnavailable
	}
	return ChallengeRecord{ID: id, Account: account, Revision: RevocationRevision{account: account, number: revision}, Payload: append([]byte(nil), payload...), ExpiresAt: expires, Attempts: attempts}, nil
}

// ReserveChallengeAttempt commits before invoking any proof provider. Failed
// or canceled verification still consumes one of five attempts.
func (s *Store) ReserveChallengeAttempt(ctx context.Context, known ChallengeRecord, now time.Time) error {
	account, err := sqlAccountID(known.Account)
	if err != nil || now.IsZero() {
		return ErrSessionInvalid
	}
	id, err := known.ID.Value()
	if err != nil {
		return ErrSessionInvalid
	}
	_, err = sessionTransaction(s, ctx, func(tx pgx.Tx) (struct{}, error) {
		var revision int64
		var status int16
		if err := tx.QueryRow(ctx, `SELECT revocation_revision,status FROM public.auth_accounts WHERE id=$1 FOR UPDATE`, account).Scan(&revision, &status); err != nil {
			return struct{}{}, ErrSessionNotFound
		}
		if status != 1 {
			return struct{}{}, ErrSessionNotFound
		}
		command, err := tx.Exec(ctx, `UPDATE public.auth_login_challenges SET attempts=attempts+1
            WHERE id=$1 AND account_id=$2 AND account_revision=$3 AND expires_at>$4
            AND consumed_at IS NULL AND attempts<$5`, id, account, revision, now, challengeProofLimit)
		if err != nil {
			return struct{}{}, ErrSessionUnavailable
		}
		if command.RowsAffected() != 1 {
			return struct{}{}, ErrSessionNotFound
		}
		return struct{}{}, nil
	})
	return err
}

func (s *Store) IssueEnrollmentGrant(ctx context.Context, accountID contracts.AccountID, revision RevocationRevision, digest accounts.EnrollmentDigest, now time.Time) (time.Time, error) {
	account, err := sqlAccountID(accountID)
	raw, digestErr := digest.Bytes()
	if err != nil || !revision.validFor(accountID) || digestErr != nil || now.IsZero() {
		return time.Time{}, ErrSessionInvalid
	}
	return sessionTransaction(s, ctx, func(tx pgx.Tx) (time.Time, error) {
		var current int64
		var status int16
		if err := tx.QueryRow(ctx, `SELECT revocation_revision,status FROM public.auth_accounts WHERE id=$1 FOR UPDATE`, account).Scan(&current, &status); err != nil {
			return time.Time{}, ErrSessionNotFound
		}
		if status != 1 || current != revision.number {
			return time.Time{}, ErrSessionNotFound
		}
		if _, err := tx.Exec(ctx, `DELETE FROM public.auth_enrollment_grants WHERE account_id=$1`, account); err != nil {
			return time.Time{}, ErrSessionUnavailable
		}
		expires := now.Add(challengeLifetime)
		if _, err := tx.Exec(ctx, `INSERT INTO public.auth_enrollment_grants(token_digest,account_id,account_revision,expires_at)
			VALUES($1,$2,$3,$4)`, raw[:], account, current, expires); err != nil {
			return time.Time{}, ErrSessionUnavailable
		}
		if err := insertSessionAudit(ctx, tx, accountID, authActionChallengeIssued, authOutcomeAdmitted); err != nil {
			return time.Time{}, err
		}
		return expires, nil
	})
}
