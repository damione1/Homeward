package storage

import (
	"context"
	"crypto/sha256"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/jackc/pgx/v5"
)

type mfaBudgetKind uint8

const (
	mfaBudgetInvalid mfaBudgetKind = iota
	mfaBudgetBegin
	mfaBudgetProof
)

func reserveMFABudgetTx(ctx context.Context, tx pgx.Tx, account contracts.AccountID, kind mfaBudgetKind, now time.Time) error {
	id, err := sqlAccountID(account)
	if err != nil || now.IsZero() {
		return mfaError(authmfa.ErrorInvalidInput)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO public.auth_mfa_budgets(account_id) VALUES($1) ON CONFLICT DO NOTHING`, id); err != nil {
		return mfaError(authmfa.ErrorUnavailable)
	}
	var begins, proofs []time.Time
	if err := tx.QueryRow(ctx, `SELECT begins,proofs FROM public.auth_mfa_budgets WHERE account_id=$1 FOR UPDATE`, id).Scan(&begins, &proofs); err != nil {
		return mfaError(authmfa.ErrorUnavailable)
	}
	switch kind {
	case mfaBudgetBegin:
		budget, err := authmfa.AdmitBeginBudget(begins, now)
		if err != nil {
			return err
		}
		budget, err = budget.Reserve(now)
		if err != nil {
			return err
		}
		begins, _ = budget.Timestamps()
	case mfaBudgetProof:
		budget, err := authmfa.AdmitProofBudget(proofs, now)
		if err != nil {
			return err
		}
		budget, err = budget.Reserve(now)
		if err != nil {
			return err
		}
		proofs, _ = budget.Timestamps()
	default:
		return mfaError(authmfa.ErrorInvalidInput)
	}
	if _, err := tx.Exec(ctx, `UPDATE public.auth_mfa_budgets SET begins=$2,proofs=$3 WHERE account_id=$1`, id, begins, proofs); err != nil {
		return mfaError(authmfa.ErrorUnavailable)
	}
	return nil
}

// ReserveBegin runs before BeginRegistration and cannot be reset by replacing
// a grant, ceremony, recovery batch or the account's second factor.
func (r *MFARepository) ReserveBegin(ctx context.Context, authority MFAEnrollmentAuthority, account contracts.AccountID, revision RevocationRevision, policy authmfa.ExpectedPolicyVersion, now time.Time) error {
	_, err := mfaTransaction(r, ctx, func(tx pgx.Tx) (struct{}, error) {
		current, err := lockMFAPolicy(ctx, tx)
		if err != nil {
			return struct{}{}, err
		}
		if !current.same(policy) {
			return struct{}{}, mfaError(authmfa.ErrorConflict)
		}
		if _, err := lockMFAAccount(ctx, tx, account, revision); err != nil {
			return struct{}{}, err
		}
		if err := admitEnrollmentAuthorityTx(ctx, tx, authority, account, revision, current, now); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, reserveMFABudgetTx(ctx, tx, account, mfaBudgetBegin, now)
	})
	return err
}

// ReserveProof consumes both budgets in one small transaction before any wire
// proof admission or crypto. Its immutable snapshot must still be revalidated
// by FinalizeLogin after verification; this reservation is not proof evidence.
func (r *MFARepository) ReserveProof(ctx context.Context, snapshot MFALoginSnapshot, now time.Time) error {
	if !snapshot.valid() || now.IsZero() {
		return mfaError(authmfa.ErrorInvalidInput)
	}
	_, err := mfaTransaction(r, ctx, func(tx pgx.Tx) (struct{}, error) {
		current, err := lockMFAPolicy(ctx, tx)
		if err != nil {
			return struct{}{}, err
		}
		if !current.same(snapshot.policy.version) {
			return struct{}{}, mfaError(authmfa.ErrorConflict)
		}
		if _, err := lockMFAAccount(ctx, tx, snapshot.account, snapshot.revision); err != nil {
			return struct{}{}, err
		}
		account, _ := snapshot.account.Value()
		id, _ := snapshot.binding.Challenge()
		challenge, _ := id.Value()
		tag, err := tx.Exec(ctx, `UPDATE public.auth_login_challenges SET attempts=attempts+1 WHERE account_id=$1 AND id=$2 AND account_revision=$3 AND options=$4 AND expires_at=$5 AND expires_at>$6 AND consumed_at IS NULL AND attempts<5`, account, challenge, snapshot.revision.number, snapshot.options, snapshot.expiry, now)
		if err != nil || tag.RowsAffected() != 1 {
			return struct{}{}, mfaError(authmfa.ErrorExpired)
		}
		tag, err = tx.Exec(ctx, `UPDATE public.auth_mfa_ceremonies SET attempts=attempts+1 WHERE account_id=$1 AND kind=1 AND id=$2 AND ciphertext=$3 AND options_hash=$4 AND expires_at>$5 AND consumed_at IS NULL AND attempts<5`, account, challenge, snapshot.ceremonyCiphertext, snapshot.optionsHash[:], now)
		if err != nil || tag.RowsAffected() != 1 {
			return struct{}{}, mfaError(authmfa.ErrorConflict)
		}
		return struct{}{}, reserveMFABudgetTx(ctx, tx, snapshot.account, mfaBudgetProof, now)
	})
	return err
}
func mfaOptionsHash(raw []byte) [32]byte { return sha256.Sum256(raw) }
