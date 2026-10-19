package storage

import (
	"context"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/jackc/pgx/v5"
)

// Metadata never carries a COSE key, ciphertext, ceremony or recovery digest.
type MFAFactorMetadata struct {
	account contracts.AccountID
	factor  contracts.FactorCredentialID
	created time.Time
	used    *time.Time
}

func (v MFAFactorMetadata) Values() (contracts.AccountID, contracts.FactorCredentialID, time.Time, *time.Time, error) {
	_, ae := v.account.Value()
	_, fe := v.factor.Value()
	if ae != nil || fe != nil || v.created.IsZero() || v.used != nil && (v.used.IsZero() || v.used.Before(v.created)) {
		return contracts.AccountID{}, contracts.FactorCredentialID{}, time.Time{}, nil, mfaError(authmfa.ErrorIntegrity)
	}
	var used *time.Time
	if v.used != nil {
		value := *v.used
		used = &value
	}
	return v.account, v.factor, v.created, used, nil
}
func (v MFAActivation) Metadata() (MFAFactorMetadata, error) {
	account, _, err := v.result.Account()
	if err != nil {
		return MFAFactorMetadata{}, err
	}
	value := MFAFactorMetadata{account: account, factor: v.factor, created: v.created}
	if _, _, _, _, err := value.Values(); err != nil {
		return MFAFactorMetadata{}, err
	}
	return value, nil
}
func (r *MFARepository) ListFactors(ctx context.Context, owner SessionOwner, now time.Time) ([]MFAFactorMetadata, error) {
	return mfaTransaction(r, ctx, func(tx pgx.Tx) ([]MFAFactorMetadata, error) {
		if err := requireCurrentMFASessionTx(ctx, tx, owner, now); err != nil {
			return nil, err
		}
		account, _ := owner.Account.Value()
		rows, err := tx.Query(ctx, `SELECT id,created_at,last_used_at FROM public.auth_mfa_credentials WHERE account_id=$1 AND removed_at IS NULL ORDER BY id LIMIT $2`, account, authmfa.CredentialLimit+1)
		if err != nil {
			return nil, mfaError(authmfa.ErrorUnavailable)
		}
		defer rows.Close()
		values := make([]MFAFactorMetadata, 0, authmfa.CredentialLimit)
		for rows.Next() {
			var id string
			var created time.Time
			var used *time.Time
			if err := rows.Scan(&id, &created, &used); err != nil {
				return nil, mfaError(authmfa.ErrorUnavailable)
			}
			factor, err := contracts.ParseFactorCredentialID(id)
			if err != nil || len(values) == authmfa.CredentialLimit {
				return nil, mfaError(authmfa.ErrorIntegrity)
			}
			value := MFAFactorMetadata{account: owner.Account, factor: factor, created: created, used: used}
			if _, _, _, _, err := value.Values(); err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		if rows.Err() != nil {
			return nil, mfaError(authmfa.ErrorUnavailable)
		}
		return values, nil
	})
}
