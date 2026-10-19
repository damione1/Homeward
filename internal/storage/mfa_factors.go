package storage

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/jackc/pgx/v5"
)

func sortedMFAAccounts(input []contracts.AccountID) ([]contracts.AccountID, error) {
	result := append([]contracts.AccountID(nil), input...)
	for _, id := range result {
		if _, err := sqlAccountID(id); err != nil {
			return nil, mfaError(authmfa.ErrorInvalidInput)
		}
	}
	sort.Slice(result, func(i, j int) bool { a, _ := result[i].Value(); b, _ := result[j].Value(); return a < b })
	for i := 1; i < len(result); i++ {
		if result[i] == result[i-1] {
			return nil, mfaError(authmfa.ErrorInvalidInput)
		}
	}
	return result, nil
}
func revokeMFAAccountTx(ctx context.Context, tx pgx.Tx, account contracts.AccountID, now time.Time) error {
	id, err := sqlAccountID(account)
	if err != nil || now.IsZero() {
		return mfaError(authmfa.ErrorInvalidInput)
	}
	tag, err := tx.Exec(ctx, `UPDATE public.auth_accounts SET revocation_revision=revocation_revision+1,updated_at=$2 WHERE id=$1 AND revocation_revision<9223372036854775807`, id, now)
	if err != nil || tag.RowsAffected() != 1 {
		return mfaError(authmfa.ErrorConflict)
	}
	if _, err := tx.Exec(ctx, `UPDATE public.auth_sessions SET revoked_at=$2 WHERE account_id=$1 AND revoked_at IS NULL`, id, now); err != nil {
		return mfaError(authmfa.ErrorUnavailable)
	}
	for _, query := range []string{`DELETE FROM public.auth_login_challenges WHERE account_id=$1`, `DELETE FROM public.auth_enrollment_grants WHERE account_id=$1`, `DELETE FROM public.auth_mfa_ceremonies WHERE account_id=$1`} {
		if _, err := tx.Exec(ctx, query, id); err != nil {
			return mfaError(authmfa.ErrorUnavailable)
		}
	}
	return nil
}

type MFAResetResult struct {
	record   AccountRecord
	account  contracts.AccountID
	revision RevocationRevision
	valid    bool
}

func (v MFAResetResult) Account() (contracts.AccountID, RevocationRevision, error) {
	if !v.valid || !v.revision.validFor(v.account) {
		return contracts.AccountID{}, RevocationRevision{}, mfaError(authmfa.ErrorInvalidInput)
	}
	return v.account, v.revision, nil
}
func (v MFAResetResult) Record() (AccountRecord, error) {
	id, err := v.record.ID()
	revision, re := v.record.Revision()
	if !v.valid || err != nil || re != nil || id != v.account || revision != v.revision {
		return AccountRecord{}, mfaError(authmfa.ErrorIntegrity)
	}
	return v.record, nil
}
func mfaResetResultTx(ctx context.Context, tx pgx.Tx, account contracts.AccountID) (MFAResetResult, error) {
	id, err := account.Value()
	if err != nil {
		return MFAResetResult{}, mfaError(authmfa.ErrorInvalidInput)
	}
	record, err := scanAccount(tx.QueryRow(ctx, `SELECT `+accountColumns+` FROM public.auth_accounts WHERE id=$1`, id))
	if err != nil {
		return MFAResetResult{}, mfaError(authmfa.ErrorIntegrity)
	}
	revision, err := record.Revision()
	if err != nil {
		return MFAResetResult{}, mfaError(authmfa.ErrorIntegrity)
	}
	return MFAResetResult{record: record, account: account, revision: revision, valid: true}, nil
}

// ResetSecondFactor is only composed by the same-UID Unix operator service.
// It never changes a password, issues a bearer or clears MFA rolling budgets.
func (r *MFARepository) ResetSecondFactor(ctx context.Context, account contracts.AccountID, now time.Time) (MFAResetResult, error) {
	id, err := sqlAccountID(account)
	if err != nil || now.IsZero() {
		return MFAResetResult{}, mfaError(authmfa.ErrorInvalidInput)
	}
	return mfaTransaction(r, ctx, func(tx pgx.Tx) (MFAResetResult, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return MFAResetResult{}, mfaError(authmfa.ErrorUnavailable)
		}
		if _, err := lockMFAPolicy(ctx, tx); err != nil {
			return MFAResetResult{}, err
		}
		var revision int64
		if err := tx.QueryRow(ctx, `SELECT revocation_revision FROM public.auth_accounts WHERE id=$1 FOR UPDATE`, id).Scan(&revision); err != nil {
			return MFAResetResult{}, mfaError(authmfa.ErrorInvalidInput)
		}
		if _, err := tx.Exec(ctx, `UPDATE public.auth_mfa_credentials SET removed_at=$2 WHERE account_id=$1 AND removed_at IS NULL`, id, now); err != nil {
			return MFAResetResult{}, mfaError(authmfa.ErrorUnavailable)
		}
		if _, err := tx.Exec(ctx, `UPDATE public.auth_mfa_recovery_batches SET retired_at=$2 WHERE account_id=$1 AND retired_at IS NULL`, id, now); err != nil {
			return MFAResetResult{}, mfaError(authmfa.ErrorUnavailable)
		}
		if _, err := tx.Exec(ctx, `UPDATE public.auth_accounts SET second_factor_enabled=false WHERE id=$1`, id); err != nil {
			return MFAResetResult{}, mfaError(authmfa.ErrorUnavailable)
		}
		if err := revokeMFAAccountTx(ctx, tx, account, now); err != nil {
			return MFAResetResult{}, err
		}
		if err := insertSessionAudit(ctx, tx, account, authActionMFAReset, authOutcomeAdmitted); err != nil {
			return MFAResetResult{}, mfaError(authmfa.ErrorUnavailable)
		}
		return mfaResetResultTx(ctx, tx, account)
	})
}

// A current interactive self session is necessary for enrollment, but its
// original authentication must also be recent. RemoveFactor and
// ReplaceRecoveryCodes apply that same recency predicate.
func requireCurrentMFASessionTx(ctx context.Context, tx pgx.Tx, owner SessionOwner, now time.Time) error {
	account, err := sqlAccountID(owner.Account)
	session, se := owner.Session.Value()
	if err != nil || se != nil || now.IsZero() {
		return mfaError(authmfa.ErrorInvalidInput)
	}
	var admitted bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.auth_sessions s JOIN public.auth_accounts c ON c.id=s.account_id CROSS JOIN public.auth_mfa_policy p WHERE p.singleton AND s.account_id=$1 AND s.id=$2 AND s.expires_at>$3 AND `+mfaSessionPredicate+`)`, account, session, now).Scan(&admitted)
	if err != nil {
		return mfaError(authmfa.ErrorUnavailable)
	}
	if !admitted {
		return mfaError(authmfa.ErrorInvalidProof)
	}
	return nil
}
func requireRecentMFASessionTx(ctx context.Context, tx pgx.Tx, owner SessionOwner, policy MFAPolicySnapshot, now time.Time) error {
	account, err := sqlAccountID(owner.Account)
	session, se := owner.Session.Value()
	if err != nil || se != nil || now.IsZero() {
		return mfaError(authmfa.ErrorInvalidInput)
	}
	var satisfaction int16
	var authenticated *time.Time
	var role, status int16
	var enabled bool
	err = tx.QueryRow(ctx, `SELECT s.satisfaction,s.authenticated_at,c.role,c.status,c.second_factor_enabled FROM public.auth_sessions s JOIN public.auth_accounts c ON c.id=s.account_id WHERE s.account_id=$1 AND s.id=$2 AND s.revoked_at IS NULL AND s.account_revision=c.revocation_revision AND s.expires_at>$3 FOR UPDATE OF s`, account, session, now).Scan(&satisfaction, &authenticated, &role, &status, &enabled)
	if err != nil {
		return mfaError(authmfa.ErrorInvalidProof)
	}
	admittedRole, roleErr := admittedSQLRole(role)
	admittedStatus, statusErr := admittedSQLStatus(status)
	if roleErr != nil || statusErr != nil {
		return mfaError(authmfa.ErrorIntegrity)
	}
	if admittedStatus != authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE || authenticated == nil || !authmfa.IsRecentAuthentication(*authenticated, now) {
		return mfaError(authmfa.ErrorInvalidProof)
	}
	required, _ := policy.source.Required()
	needs := enabled || (admittedRole == authv1.AccountRole_ACCOUNT_ROLE_ADMIN && required)
	switch authv1.AuthenticationSatisfaction(satisfaction) {
	case authv1.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_FIDO2, authv1.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_RECOVERY_CODE:
		return nil
	case authv1.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_ONLY:
		if !needs {
			return nil
		}
		return mfaError(authmfa.ErrorInvalidProof)
	case authv1.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_UNSPECIFIED:
		return mfaError(authmfa.ErrorInvalidProof)
	default:
		return mfaError(authmfa.ErrorIntegrity)
	}
}
func (r *MFARepository) RemoveFactor(ctx context.Context, owner SessionOwner, factor contracts.FactorCredentialID, now time.Time) (MFAResetResult, error) {
	id, err := factor.Value()
	account, ae := sqlAccountID(owner.Account)
	if err != nil || ae != nil || now.IsZero() {
		return MFAResetResult{}, mfaError(authmfa.ErrorInvalidInput)
	}
	return mfaTransaction(r, ctx, func(tx pgx.Tx) (MFAResetResult, error) {
		policy, err := lockMFAPolicy(ctx, tx)
		if err != nil {
			return MFAResetResult{}, err
		}
		var role, status int16
		if err := tx.QueryRow(ctx, `SELECT role,status FROM public.auth_accounts WHERE id=$1 FOR UPDATE`, account).Scan(&role, &status); err != nil {
			return MFAResetResult{}, mfaError(authmfa.ErrorInvalidProof)
		}
		admittedRole, roleErr := admittedSQLRole(role)
		admittedStatus, statusErr := admittedSQLStatus(status)
		if roleErr != nil || statusErr != nil {
			return MFAResetResult{}, mfaError(authmfa.ErrorIntegrity)
		}
		if admittedStatus != authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE {
			return MFAResetResult{}, mfaError(authmfa.ErrorInvalidProof)
		}
		if err := requireRecentMFASessionTx(ctx, tx, owner, policy, now); err != nil {
			return MFAResetResult{}, err
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM public.auth_mfa_credentials WHERE account_id=$1 AND removed_at IS NULL`, account).Scan(&count); err != nil || count < 1 || count > authmfa.CredentialLimit {
			return MFAResetResult{}, mfaError(authmfa.ErrorIntegrity)
		}
		required, _ := policy.source.Required()
		if count == 1 && admittedRole == authv1.AccountRole_ACCOUNT_ROLE_ADMIN && required {
			return MFAResetResult{}, mfaError(authmfa.ErrorInvalidProof)
		}
		tag, err := tx.Exec(ctx, `UPDATE public.auth_mfa_credentials SET removed_at=$3 WHERE account_id=$1 AND id=$2 AND removed_at IS NULL`, account, id, now)
		if err != nil || tag.RowsAffected() != 1 {
			return MFAResetResult{}, mfaError(authmfa.ErrorConsumed)
		}
		if count == 1 {
			if _, err := tx.Exec(ctx, `UPDATE public.auth_accounts SET second_factor_enabled=false WHERE id=$1`, account); err != nil {
				return MFAResetResult{}, mfaError(authmfa.ErrorUnavailable)
			}
			if _, err := tx.Exec(ctx, `UPDATE public.auth_mfa_recovery_batches SET retired_at=$2 WHERE account_id=$1 AND retired_at IS NULL`, account, now); err != nil {
				return MFAResetResult{}, mfaError(authmfa.ErrorUnavailable)
			}
		}
		if err := revokeMFAAccountTx(ctx, tx, owner.Account, now); err != nil {
			return MFAResetResult{}, err
		}
		if err := insertSessionAudit(ctx, tx, owner.Account, authActionMFARemove, authOutcomeAdmitted); err != nil {
			return MFAResetResult{}, mfaError(authmfa.ErrorUnavailable)
		}
		return mfaResetResultTx(ctx, tx, owner.Account)
	})
}

// MFARecoveryBatch contains only admitted digests; raw one-time codes stay in
// the server's first-response scope and are never passed to persistence.
type MFARecoveryBatch struct{ *mfaRecoveryBatchValue }
type mfaRecoveryBatchValue struct {
	account contracts.AccountID
	batch   authmfa.RecoveryBatchID
	codes   []authmfa.RecoveryCandidate
}

func NewMFARecoveryBatch(account contracts.AccountID, batch authmfa.RecoveryBatchID, codes []authmfa.RecoveryCandidate) (MFARecoveryBatch, error) {
	if _, err := sqlAccountID(account); err != nil {
		return MFARecoveryBatch{}, mfaError(authmfa.ErrorInvalidInput)
	}
	if _, err := batch.Value(); err != nil || len(codes) != authmfa.RecoveryCodeCount {
		return MFARecoveryBatch{}, mfaError(authmfa.ErrorInvalidInput)
	}
	ids := map[authmfa.RecoveryCodeID]bool{}
	digests := map[[32]byte]bool{}
	for _, code := range codes {
		id, ie := code.ID()
		digest, de := code.Digest()
		raw, re := digest.Bytes()
		admitted, ae := authmfa.AdmitRecoveryDigest(account, batch, raw[:])
		if ie != nil || de != nil || re != nil || ae != nil || !digest.MatchesBinding(account, batch) || ids[id] || digests[raw] {
			return MFARecoveryBatch{}, mfaError(authmfa.ErrorIntegrity)
		}
		_ = admitted
		ids[id] = true
		digests[raw] = true
	}
	return MFARecoveryBatch{&mfaRecoveryBatchValue{account: account, batch: batch, codes: append([]authmfa.RecoveryCandidate(nil), codes...)}}, nil
}
func replaceMFARecoveryTx(ctx context.Context, tx pgx.Tx, batch MFARecoveryBatch, now time.Time) error {
	if batch.mfaRecoveryBatchValue == nil {
		return mfaError(authmfa.ErrorInvalidInput)
	}
	if _, err := NewMFARecoveryBatch(batch.account, batch.batch, batch.codes); err != nil {
		return err
	}
	account, _ := batch.account.Value()
	id, _ := batch.batch.Value()
	if _, err := tx.Exec(ctx, `UPDATE public.auth_mfa_recovery_batches SET retired_at=$2 WHERE account_id=$1 AND retired_at IS NULL`, account, now); err != nil {
		return mfaError(authmfa.ErrorUnavailable)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO public.auth_mfa_recovery_batches(account_id,id,created_at) VALUES($1,$2,$3)`, account, id, now); err != nil {
		return mfaError(authmfa.ErrorUnavailable)
	}
	for _, candidate := range batch.codes {
		code, _ := candidate.ID()
		codeText, _ := code.Value()
		digest, _ := candidate.Digest()
		raw, _ := digest.Bytes()
		if _, err := tx.Exec(ctx, `INSERT INTO public.auth_mfa_recovery_codes(account_id,batch_id,id,digest) VALUES($1,$2,$3,$4)`, account, id, codeText, raw[:]); err != nil {
			return mfaError(authmfa.ErrorUnavailable)
		}
	}
	return nil
}
func (r *MFARepository) ReplaceRecoveryCodes(ctx context.Context, owner SessionOwner, batch MFARecoveryBatch, now time.Time) (MFAResetResult, error) {
	if batch.mfaRecoveryBatchValue == nil || batch.account != owner.Account || now.IsZero() {
		return MFAResetResult{}, mfaError(authmfa.ErrorInvalidInput)
	}
	return mfaTransaction(r, ctx, func(tx pgx.Tx) (MFAResetResult, error) {
		policy, err := lockMFAPolicy(ctx, tx)
		if err != nil {
			return MFAResetResult{}, err
		}
		id, _ := owner.Account.Value()
		var enabled bool
		if err := tx.QueryRow(ctx, `SELECT second_factor_enabled FROM public.auth_accounts WHERE id=$1 FOR UPDATE`, id).Scan(&enabled); err != nil || !enabled {
			return MFAResetResult{}, mfaError(authmfa.ErrorInvalidProof)
		}
		if err := requireRecentMFASessionTx(ctx, tx, owner, policy, now); err != nil {
			return MFAResetResult{}, err
		}
		if err := replaceMFARecoveryTx(ctx, tx, batch, now); err != nil {
			return MFAResetResult{}, err
		}
		if err := revokeMFAAccountTx(ctx, tx, owner.Account, now); err != nil {
			return MFAResetResult{}, err
		}
		if err := insertSessionAudit(ctx, tx, owner.Account, authActionMFARecoveryReplace, authOutcomeAdmitted); err != nil {
			return MFAResetResult{}, mfaError(authmfa.ErrorUnavailable)
		}
		return mfaResetResultTx(ctx, tx, owner.Account)
	})
}

// Explicitly use the existing grant digest type; no second secret vocabulary.
func lookupMFAEnrollmentGrant(ctx context.Context, tx pgx.Tx, digest accounts.EnrollmentDigest, now time.Time) (contracts.AccountID, RevocationRevision, error) {
	raw, err := digest.Bytes()
	if err != nil {
		return contracts.AccountID{}, RevocationRevision{}, mfaError(authmfa.ErrorInvalidInput)
	}
	var account string
	var revision int64
	err = tx.QueryRow(ctx, `SELECT account_id,account_revision FROM public.auth_enrollment_grants WHERE token_digest=$1 AND expires_at>$2 AND consumed_at IS NULL FOR UPDATE`, raw[:], now).Scan(&account, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.AccountID{}, RevocationRevision{}, mfaError(authmfa.ErrorInvalidProof)
	}
	parsed, pe := contracts.ParseAccountID(account)
	if err != nil || pe != nil || revision <= 0 {
		return contracts.AccountID{}, RevocationRevision{}, mfaError(authmfa.ErrorIntegrity)
	}
	return parsed, RevocationRevision{account: parsed, number: revision}, nil
}

// Called only after primary account locks have settled an uncertain mutation.
// No decryption, verification or new mutation occurs during reconciliation.
func readSettledMFAAccountTx(ctx context.Context, tx pgx.Tx, account contracts.AccountID) error {
	id, err := sqlAccountID(account)
	if err != nil {
		return mfaError(authmfa.ErrorInvalidInput)
	}
	var enabled bool
	var credentials, batches, codes int
	err = tx.QueryRow(ctx, `SELECT a.second_factor_enabled,
 (SELECT count(*) FROM public.auth_mfa_credentials c WHERE c.account_id=a.id AND c.removed_at IS NULL),
 (SELECT count(*) FROM public.auth_mfa_recovery_batches b WHERE b.account_id=a.id AND b.retired_at IS NULL),
 (SELECT count(*) FROM public.auth_mfa_recovery_codes c JOIN public.auth_mfa_recovery_batches b ON b.account_id=c.account_id AND b.id=c.batch_id WHERE b.account_id=a.id AND b.retired_at IS NULL)
 FROM public.auth_accounts a WHERE a.id=$1`, id).Scan(&enabled, &credentials, &batches, &codes)
	if err != nil || credentials < 0 || credentials > authmfa.CredentialLimit || enabled != (credentials > 0) {
		return mfaError(authmfa.ErrorIntegrity)
	}
	if enabled {
		if batches != 1 || codes != authmfa.RecoveryCodeCount {
			return mfaError(authmfa.ErrorIntegrity)
		}
	} else if batches != 0 || codes != 0 {
		return mfaError(authmfa.ErrorIntegrity)
	}
	return nil
}

// RecoveryAvailable reveals only whether the currently active batch has an
// unused code. Digests and code material never leave through this query.
func (r *MFARepository) RecoveryAvailable(ctx context.Context, account contracts.AccountID) (bool, error) {
	id, err := sqlAccountID(account)
	if !r.valid() || accountContextError(ctx) != nil || err != nil {
		return false, mfaError(authmfa.ErrorInvalidInput)
	}
	var present bool
	err = r.store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.auth_mfa_recovery_batches b JOIN public.auth_mfa_recovery_codes c ON c.account_id=b.account_id AND c.batch_id=b.id WHERE b.account_id=$1 AND b.retired_at IS NULL AND c.consumed_at IS NULL)`, id).Scan(&present)
	if err != nil {
		return false, mfaError(authmfa.ErrorUnavailable)
	}
	return present, nil
}
