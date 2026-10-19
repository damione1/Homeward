package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/jackc/pgx/v5"
)

type MFARepository struct{ *mfaRepositoryValue }
type mfaRepositoryValue struct {
	store     *Store
	protector *SecretRepository
}

func (s *Store) NewMFARepository(protector *SecretRepository) (*MFARepository, error) {
	if s == nil || s.pool == nil || !protector.Valid() || protector.store != s {
		return nil, mfaError(authmfa.ErrorInvalidInput)
	}
	return &MFARepository{&mfaRepositoryValue{store: s, protector: protector}}, nil
}
func (r *MFARepository) valid() bool {
	return r != nil && r.mfaRepositoryValue != nil && r.store != nil && r.protector.Valid() && r.protector.store == r.store
}
func (r *MFARepository) String() string               { return "[REDACTED MFA REPOSITORY]" }
func (r *MFARepository) GoString() string             { return r.String() }
func (r *MFARepository) MarshalJSON() ([]byte, error) { return json.Marshal(r.String()) }
func mfaError(kind authmfa.ErrorKind) error           { return authmfa.NewError(kind) }
func mfaTransaction[T any](r *MFARepository, ctx context.Context, fn func(pgx.Tx) (T, error)) (T, error) {
	if !r.valid() {
		var zero T
		return zero, mfaError(authmfa.ErrorInvalidInput)
	}
	return mfaStoreTransaction(r.store, ctx, fn)
}

// Policy initialization is configuration metadata, independent of a crypto
// protector. All MFA transactions keep the same commit-outcome classification.
func mfaStoreTransaction[T any](store *Store, ctx context.Context, fn func(pgx.Tx) (T, error)) (T, error) {
	var zero T
	if store == nil || store.pool == nil || accountContextError(ctx) != nil || fn == nil {
		return zero, mfaError(authmfa.ErrorInvalidInput)
	}
	var result T
	var decision error
	outcome, err := store.InTransactionOutcome(ctx, func(tx pgx.Tx) error { result, decision = fn(tx); return decision })
	if outcome == TransactionIndeterminate {
		return zero, mfaError(authmfa.ErrorCommitUnknown)
	}
	if decision != nil {
		if authmfa.KindOf(decision) != authmfa.ErrorUnspecified {
			return zero, decision
		}
		return zero, mfaError(authmfa.ErrorUnavailable)
	}
	if err != nil || outcome != TransactionCommitted {
		return zero, mfaError(authmfa.ErrorUnavailable)
	}
	return result, nil
}

// BootstrapMFAInstance admits the configured RP once. It never resets records
// on a mismatch or invents an identity for an already populated MFA database.
func (r *MFARepository) BootstrapMFAInstance(ctx context.Context, canonicalURL string) (authmfa.RelyingParty, error) {
	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return authmfa.RelyingParty{}, mfaError(authmfa.ErrorUnavailable)
	}
	candidate, _ := contracts.ParseInstanceID(hex.EncodeToString(entropy[:]))
	party, err := authmfa.NewRelyingParty(candidate, canonicalURL)
	if err != nil {
		return authmfa.RelyingParty{}, err
	}
	_, rp, origin, _ := party.Values()
	return mfaTransaction(r, ctx, func(tx pgx.Tx) (authmfa.RelyingParty, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return authmfa.RelyingParty{}, mfaError(authmfa.ErrorUnavailable)
		}
		var id, storedRP, storedOrigin string
		err := tx.QueryRow(ctx, `SELECT instance_id,rp_id,origin FROM public.auth_mfa_instance WHERE singleton FOR UPDATE`).Scan(&id, &storedRP, &storedOrigin)
		if errors.Is(err, pgx.ErrNoRows) {
			var populated bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.auth_mfa_users) OR EXISTS(SELECT 1 FROM public.auth_mfa_credentials) OR EXISTS(SELECT 1 FROM public.auth_mfa_ceremonies)`).Scan(&populated); err != nil || populated {
				return authmfa.RelyingParty{}, mfaError(authmfa.ErrorIntegrity)
			}
			id, _ = candidate.Value()
			if _, err := tx.Exec(ctx, `INSERT INTO public.auth_mfa_instance(singleton,instance_id,rp_id,origin) VALUES(true,$1,$2,$3)`, id, rp, origin); err != nil {
				return authmfa.RelyingParty{}, mfaError(authmfa.ErrorUnavailable)
			}
			return party, nil
		}
		if err != nil || storedRP != rp || storedOrigin != origin {
			return authmfa.RelyingParty{}, mfaError(authmfa.ErrorIntegrity)
		}
		instance, err := contracts.ParseInstanceID(id)
		if err != nil {
			return authmfa.RelyingParty{}, mfaError(authmfa.ErrorIntegrity)
		}
		return authmfa.NewRelyingParty(instance, canonicalURL)
	})
}
func reserveMFASalt(ctx context.Context, tx pgx.Tx, envelope MFAEnvelope) error {
	if !envelope.valid() {
		return mfaError(authmfa.ErrorIntegrity)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO public.secret_salts(salt) VALUES($1)`, envelope.salt[:]); err != nil {
		return mfaError(authmfa.ErrorUnavailable)
	}
	return nil
}
func lockMFAAccount(ctx context.Context, tx pgx.Tx, account contracts.AccountID, revision RevocationRevision) (bool, error) {
	id, err := sqlAccountID(account)
	if err != nil || !revision.validFor(account) {
		return false, mfaError(authmfa.ErrorInvalidInput)
	}
	var current int64
	var status int16
	var enabled bool
	if err := tx.QueryRow(ctx, `SELECT revocation_revision,status,second_factor_enabled FROM public.auth_accounts WHERE id=$1 FOR UPDATE`, id).Scan(&current, &status, &enabled); err != nil {
		return false, mfaError(authmfa.ErrorConflict)
	}
	admittedStatus, err := admittedSQLStatus(status)
	if err != nil {
		return false, mfaError(authmfa.ErrorIntegrity)
	}
	if admittedStatus != authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE || current != revision.number {
		return false, mfaError(authmfa.ErrorConflict)
	}
	return enabled, nil
}
func sameTime(a, b time.Time) bool { return !a.IsZero() && !b.IsZero() && a.Equal(b) }

// MFAAccountState is read from primary before preparing a login. It is a
// preliminary snapshot; every issuance/mutation checks it again in SQL.
type MFAAccountState struct {
	account  contracts.AccountID
	revision RevocationRevision
	role     authv1.AccountRole
	enabled  bool
	policy   MFAPolicySnapshot
}
type MFALoginRequirement uint8

const (
	MFALoginUnspecified MFALoginRequirement = iota
	MFALoginPasswordOnly
	MFALoginFactorChallenge
	MFALoginEnrollment
)

func (v MFAAccountState) Requirement() (MFALoginRequirement, RevocationRevision, authmfa.ExpectedPolicyVersion, error) {
	if !v.revision.validFor(v.account) {
		return MFALoginUnspecified, RevocationRevision{}, authmfa.ExpectedPolicyVersion{}, mfaError(authmfa.ErrorInvalidInput)
	}
	source, err := v.policy.Source()
	if err != nil {
		return MFALoginUnspecified, RevocationRevision{}, authmfa.ExpectedPolicyVersion{}, err
	}
	required, _ := source.Required()
	switch v.role {
	case authv1.AccountRole_ACCOUNT_ROLE_ADMIN, authv1.AccountRole_ACCOUNT_ROLE_USER:
	default:
		return MFALoginUnspecified, RevocationRevision{}, authmfa.ExpectedPolicyVersion{}, mfaError(authmfa.ErrorIntegrity)
	}
	if v.enabled {
		return MFALoginFactorChallenge, v.revision, v.policy.version, nil
	}
	if v.role == authv1.AccountRole_ACCOUNT_ROLE_ADMIN && required {
		return MFALoginEnrollment, v.revision, v.policy.version, nil
	}
	return MFALoginPasswordOnly, v.revision, v.policy.version, nil
}
func (r *MFARepository) AccountState(ctx context.Context, account contracts.AccountID) (MFAAccountState, error) {
	id, err := sqlAccountID(account)
	if !r.valid() || accountContextError(ctx) != nil || err != nil {
		return MFAAccountState{}, mfaError(authmfa.ErrorInvalidInput)
	}
	var revision int64
	var role, status int16
	var enabled bool
	var count int
	err = r.store.pool.QueryRow(ctx, `SELECT a.revocation_revision,a.role,a.status,a.second_factor_enabled,(SELECT count(*) FROM public.auth_mfa_credentials c WHERE c.account_id=a.id AND c.removed_at IS NULL) FROM public.auth_accounts a WHERE a.id=$1`, id).Scan(&revision, &role, &status, &enabled, &count)
	if errors.Is(err, pgx.ErrNoRows) {
		return MFAAccountState{}, mfaError(authmfa.ErrorInvalidProof)
	}
	if err != nil {
		return MFAAccountState{}, mfaError(authmfa.ErrorUnavailable)
	}
	admittedRole, roleErr := admittedSQLRole(role)
	admittedStatus, statusErr := admittedSQLStatus(status)
	if roleErr != nil || statusErr != nil || revision <= 0 || count < 0 || count > authmfa.CredentialLimit || enabled != (count > 0) {
		return MFAAccountState{}, mfaError(authmfa.ErrorIntegrity)
	}
	if admittedStatus != authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE {
		return MFAAccountState{}, mfaError(authmfa.ErrorInvalidProof)
	}
	policy, err := r.MFAPolicy(ctx)
	if err != nil {
		return MFAAccountState{}, err
	}
	state := MFAAccountState{account: account, revision: RevocationRevision{account: account, number: revision}, role: admittedRole, enabled: enabled, policy: policy}
	if _, _, _, err := state.Requirement(); err != nil {
		return MFAAccountState{}, err
	}
	return state, nil
}
