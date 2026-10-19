package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

type MFALoginSnapshot struct{ *mfaLoginSnapshotValue }
type mfaLoginSnapshotValue struct {
	account            contracts.AccountID
	binding            authmfa.Binding
	revision           RevocationRevision
	policy             MFAPolicySnapshot
	party              authmfa.RelyingParty
	expiry             time.Time
	options            []byte
	optionsHash        [32]byte
	ceremonyCiphertext []byte
	ceremony           authmfa.PreparedCeremony
	user               authmfa.UserSnapshot
	batch              authmfa.RecoveryBatchID
	candidates         []authmfa.RecoveryCandidate
}

func (s MFALoginSnapshot) valid() bool {
	if s.mfaLoginSnapshotValue == nil || s.expiry.IsZero() || !s.revision.validFor(s.account) || len(s.options) == 0 || len(s.options) > 16384 || mfaOptionsHash(s.options) != s.optionsHash {
		return false
	}
	account, err := s.binding.Account()
	_, pe := s.policy.Source()
	_, ce := s.ceremony.Bytes()
	_, ue := s.user.Records()
	return err == nil && account == s.account && pe == nil && ce == nil && ue == nil
}
func (MFALoginSnapshot) String() string                 { return "[REDACTED MFA LOGIN SNAPSHOT]" }
func (s MFALoginSnapshot) GoString() string             { return s.String() }
func (s MFALoginSnapshot) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }
func (s MFALoginSnapshot) Verification() (authmfa.Binding, authmfa.UserSnapshot, authmfa.PreparedCeremony, authmfa.RecoveryBatchID, []authmfa.RecoveryCandidate, error) {
	if !s.valid() {
		return authmfa.Binding{}, authmfa.UserSnapshot{}, authmfa.PreparedCeremony{}, authmfa.RecoveryBatchID{}, nil, mfaError(authmfa.ErrorInvalidInput)
	}
	return s.binding, s.user, s.ceremony, s.batch, append([]authmfa.RecoveryCandidate(nil), s.candidates...), nil
}
func (s MFALoginSnapshot) Offered() (*authv1.FactorChallenge, error) {
	if !s.valid() {
		return nil, mfaError(authmfa.ErrorInvalidInput)
	}
	var offered authv1.FactorChallenge
	if err := proto.Unmarshal(s.options, &offered); err != nil {
		return nil, mfaError(authmfa.ErrorIntegrity)
	}
	return &offered, nil
}

// EnsureUserHandle receives randomness prepared before the account gate/SQL.
// Conflicting proposals retain the original stable handle rather than rotate it.
func (r *MFARepository) EnsureUserHandle(ctx context.Context, account contracts.AccountID, revision RevocationRevision, party authmfa.RelyingParty, handle []byte) ([]byte, error) {
	id, err := sqlAccountID(account)
	instance, _, _, pe := party.Values()
	instanceText, _ := instance.Value()
	if err != nil || pe != nil || len(handle) != authmfa.UserHandleBytes || bytes.Equal(handle, make([]byte, 32)) {
		return nil, mfaError(authmfa.ErrorInvalidInput)
	}
	copyHandle := append([]byte(nil), handle...)
	return mfaTransaction(r, ctx, func(tx pgx.Tx) ([]byte, error) {
		if _, err := lockMFAPolicy(ctx, tx); err != nil {
			return nil, err
		}
		if _, err := lockMFAAccount(ctx, tx, account, revision); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public.auth_mfa_users(account_id,instance_id,user_handle) VALUES($1,$2,$3) ON CONFLICT(account_id) DO NOTHING`, id, instanceText, copyHandle); err != nil {
			return nil, mfaError(authmfa.ErrorUnavailable)
		}
		var stored []byte
		var storedInstance string
		if err := tx.QueryRow(ctx, `SELECT instance_id,user_handle FROM public.auth_mfa_users WHERE account_id=$1`, id).Scan(&storedInstance, &stored); err != nil || storedInstance != instanceText || len(stored) != 32 {
			return nil, mfaError(authmfa.ErrorIntegrity)
		}
		return append([]byte(nil), stored...), nil
	})
}
func (r *MFARepository) UserSnapshot(ctx context.Context, account contracts.AccountID, party authmfa.RelyingParty) (authmfa.UserSnapshot, error) {
	id, err := sqlAccountID(account)
	instance, rp, _, pe := party.Values()
	instanceText, _ := instance.Value()
	if !r.valid() || accountContextError(ctx) != nil || err != nil || pe != nil {
		return authmfa.UserSnapshot{}, mfaError(authmfa.ErrorInvalidInput)
	}
	var handle []byte
	var name, display, storedInstance string
	if err := r.store.pool.QueryRow(ctx, `SELECT u.user_handle,a.login_name,a.display_name,u.instance_id FROM public.auth_mfa_users u JOIN public.auth_accounts a ON a.id=u.account_id WHERE u.account_id=$1 AND a.status=1`, id).Scan(&handle, &name, &display, &storedInstance); err != nil || storedInstance != instanceText {
		return authmfa.UserSnapshot{}, mfaError(authmfa.ErrorIntegrity)
	}
	// Close the bounded cursor before decrypting/decoding any record.
	type storedCredential struct {
		id                    string
		version               int64
		key, salt, ciphertext []byte
		instance, rp, suite   string
	}
	var stored []storedCredential
	rows, err := r.store.pool.Query(ctx, `SELECT id,record_version,key_id,salt,ciphertext,instance_id,rp_id,cipher_suite FROM public.auth_mfa_credentials WHERE account_id=$1 AND removed_at IS NULL ORDER BY id LIMIT $2`, id, authmfa.CredentialLimit+1)
	if err != nil {
		return authmfa.UserSnapshot{}, mfaError(authmfa.ErrorUnavailable)
	}
	for rows.Next() {
		var raw storedCredential
		if err := rows.Scan(&raw.id, &raw.version, &raw.key, &raw.salt, &raw.ciphertext, &raw.instance, &raw.rp, &raw.suite); err != nil {
			rows.Close()
			return authmfa.UserSnapshot{}, mfaError(authmfa.ErrorUnavailable)
		}
		stored = append(stored, raw)
	}
	rows.Close()
	if rows.Err() != nil || len(stored) > authmfa.CredentialLimit {
		return authmfa.UserSnapshot{}, mfaError(authmfa.ErrorIntegrity)
	}
	records := make([]authmfa.CredentialRecord, 0, len(stored))
	for _, raw := range stored {
		factor, ie := contracts.ParseFactorCredentialID(raw.id)
		version, ve := authmfa.NewRecordVersion(uint64(raw.version))
		binding, be := NewMFACredentialBinding(party, account, factor, version)
		if ie != nil || ve != nil || be != nil || raw.version <= 0 || raw.instance != instanceText || raw.rp != rp || raw.suite != secretSuiteSQL {
			return authmfa.UserSnapshot{}, mfaError(authmfa.ErrorIntegrity)
		}
		plain, err := r.openEnvelope(binding, raw.key, raw.salt, raw.ciphertext)
		if err != nil {
			return authmfa.UserSnapshot{}, err
		}
		record, err := authmfa.DecodeCredentialRecord(account, factor, party, version, plain)
		if err != nil {
			return authmfa.UserSnapshot{}, err
		}
		records = append(records, record)
	}
	return authmfa.NewUserSnapshot(account, handle, name, display, records)
}
func (r *MFARepository) LoginSnapshot(ctx context.Context, known ChallengeRecord, now time.Time) (MFALoginSnapshot, error) {
	id, err := known.ID.Value()
	account, ae := sqlAccountID(known.Account)
	if !r.valid() || accountContextError(ctx) != nil || err != nil || ae != nil || now.IsZero() || !known.Revision.validFor(known.Account) {
		return MFALoginSnapshot{}, mfaError(authmfa.ErrorInvalidInput)
	}
	var instance, rp, origin, suite string
	var revision, policyRevision int64
	var expiry time.Time
	var options, key, salt, ciphertext, policyHash, optionsHash []byte
	err = r.store.pool.QueryRow(ctx, `SELECT c.instance_id,c.rp_id,i.origin,c.account_revision,c.policy_revision,c.policy_hash,c.options_hash,c.key_id,c.salt,c.ciphertext,c.cipher_suite,l.options,l.expires_at
 FROM public.auth_login_challenges l JOIN public.auth_mfa_ceremonies c ON c.account_id=l.account_id AND c.id=l.id AND c.kind=1
 JOIN public.auth_mfa_instance i ON i.instance_id=c.instance_id AND i.rp_id=c.rp_id
 WHERE l.account_id=$1 AND l.id=$2 AND l.account_revision=c.account_revision AND l.consumed_at IS NULL AND c.consumed_at IS NULL AND l.expires_at=c.expires_at AND l.expires_at>$3`, account, id, now).Scan(&instance, &rp, &origin, &revision, &policyRevision, &policyHash, &optionsHash, &key, &salt, &ciphertext, &suite, &options, &expiry)
	if errors.Is(err, pgx.ErrNoRows) {
		return MFALoginSnapshot{}, mfaError(authmfa.ErrorExpired)
	}
	if err != nil {
		return MFALoginSnapshot{}, mfaError(authmfa.ErrorUnavailable)
	}
	hash := mfaOptionsHash(options)
	if revision != known.Revision.number || !bytes.Equal(options, known.Payload) || !sameTime(expiry, known.ExpiresAt) || suite != secretSuiteSQL || !bytes.Equal(optionsHash, hash[:]) {
		return MFALoginSnapshot{}, mfaError(authmfa.ErrorConflict)
	}
	instanceID, ie := contracts.ParseInstanceID(instance)
	party, pe := authmfa.NewRelyingParty(instanceID, origin)
	_, actualRP, _, _ := party.Values()
	binding, be := authmfa.NewBinding(known.Account, known.ID, instanceID)
	if ie != nil || pe != nil || be != nil || actualRP != rp || policyRevision <= 0 {
		return MFALoginSnapshot{}, mfaError(authmfa.ErrorIntegrity)
	}
	policy, err := r.MFAPolicy(ctx)
	if err != nil {
		return MFALoginSnapshot{}, err
	}
	expected, ee := authmfa.NewExpectedPolicyVersion(uint64(policyRevision), policyHash)
	if ee != nil || !policy.same(expected) {
		return MFALoginSnapshot{}, mfaError(authmfa.ErrorConflict)
	}
	cipherBinding, err := NewMFACeremonyBinding(party, binding)
	if err != nil {
		return MFALoginSnapshot{}, err
	}
	plain, err := r.openEnvelope(cipherBinding, key, salt, ciphertext)
	if err != nil {
		return MFALoginSnapshot{}, err
	}
	ceremony, err := authmfa.DecodePreparedCeremony(binding, party, plain, now)
	if err != nil {
		return MFALoginSnapshot{}, err
	}
	kind, _ := ceremony.Kind()
	ceremonyExpiry, _ := ceremony.ExpiresAt()
	if kind != authmfa.CeremonyLogin || !sameTime(expiry, ceremonyExpiry) {
		return MFALoginSnapshot{}, mfaError(authmfa.ErrorIntegrity)
	}
	if err := validateMFALoginOptions(ceremony, options, now); err != nil {
		return MFALoginSnapshot{}, err
	}
	user, err := r.UserSnapshot(ctx, known.Account, party)
	if err != nil {
		return MFALoginSnapshot{}, err
	}
	snapshot := MFALoginSnapshot{&mfaLoginSnapshotValue{account: known.Account, binding: binding, revision: known.Revision, policy: policy, party: party, expiry: expiry, options: append([]byte(nil), options...), optionsHash: hash, ceremonyCiphertext: append([]byte(nil), ciphertext...), ceremony: ceremony, user: user}}
	var batchText string
	err = r.store.pool.QueryRow(ctx, `SELECT id FROM public.auth_mfa_recovery_batches WHERE account_id=$1 AND retired_at IS NULL`, account).Scan(&batchText)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return MFALoginSnapshot{}, mfaError(authmfa.ErrorUnavailable)
	}
	if err == nil {
		batch, err := authmfa.ParseRecoveryBatchID(batchText)
		if err != nil {
			return MFALoginSnapshot{}, err
		}
		snapshot.batch = batch
		rows, err := r.store.pool.Query(ctx, `SELECT id,digest FROM public.auth_mfa_recovery_codes WHERE account_id=$1 AND batch_id=$2 AND consumed_at IS NULL ORDER BY id LIMIT $3`, account, batchText, authmfa.RecoveryCodeCount+1)
		if err != nil {
			return MFALoginSnapshot{}, mfaError(authmfa.ErrorUnavailable)
		}
		for rows.Next() {
			var text string
			var raw []byte
			if err := rows.Scan(&text, &raw); err != nil {
				rows.Close()
				return MFALoginSnapshot{}, mfaError(authmfa.ErrorIntegrity)
			}
			id, ie := authmfa.ParseRecoveryCodeID(text)
			digest, de := authmfa.AdmitRecoveryDigest(known.Account, batch, raw)
			candidate, ce := authmfa.NewRecoveryCandidate(id, digest)
			if ie != nil || de != nil || ce != nil {
				rows.Close()
				return MFALoginSnapshot{}, mfaError(authmfa.ErrorIntegrity)
			}
			snapshot.candidates = append(snapshot.candidates, candidate)
		}
		rows.Close()
		if rows.Err() != nil || len(snapshot.candidates) > authmfa.RecoveryCodeCount {
			return MFALoginSnapshot{}, mfaError(authmfa.ErrorIntegrity)
		}
	}
	if !snapshot.valid() {
		return MFALoginSnapshot{}, mfaError(authmfa.ErrorIntegrity)
	}
	return snapshot, nil
}

type MFAFinalization struct{ *mfaFinalizationValue }
type mfaFinalizationValue struct {
	snapshot MFALoginSnapshot
	proof    authmfa.VerifiedLogin
	envelope MFAEnvelope
}

func (MFAFinalization) String() string                 { return "[REDACTED MFA FINALIZATION]" }
func (f MFAFinalization) GoString() string             { return f.String() }
func (f MFAFinalization) MarshalJSON() ([]byte, error) { return json.Marshal(f.String()) }
func (r *MFARepository) PrepareFinalization(proof authmfa.VerifiedLogin, snapshot MFALoginSnapshot) (MFAFinalization, error) {
	if !snapshot.valid() {
		return MFAFinalization{}, mfaError(authmfa.ErrorInvalidInput)
	}
	binding, err := proof.Binding()
	digest, de := proof.CeremonyDigest()
	expectedDigest, ee := snapshot.ceremony.Digest()
	if err != nil || de != nil || ee != nil || digest != expectedDigest || !snapshot.valid() || binding != snapshot.binding {
		return MFAFinalization{}, mfaError(authmfa.ErrorInvalidProof)
	}
	offered, err := snapshot.Offered()
	if err != nil {
		return MFAFinalization{}, err
	}
	fidoOffered, recoveryOffered := false, false
	for _, option := range offered.Options {
		if option == nil {
			return MFAFinalization{}, mfaError(authmfa.ErrorIntegrity)
		}
		switch method := option.Method.(type) {
		case *authv1.FactorOption_Fido2:
			if method == nil || method.Fido2 == nil {
				return MFAFinalization{}, mfaError(authmfa.ErrorIntegrity)
			}
			fidoOffered = true
		case *authv1.FactorOption_RecoveryCode:
			if method == nil || method.RecoveryCode == nil {
				return MFAFinalization{}, mfaError(authmfa.ErrorIntegrity)
			}
			recoveryOffered = true
		default:
			return MFAFinalization{}, mfaError(authmfa.ErrorIntegrity)
		}
	}
	result := MFAFinalization{&mfaFinalizationValue{snapshot: snapshot, proof: proof}}
	if fido, ok := proof.FIDO2(); ok {
		if !fidoOffered {
			return MFAFinalization{}, mfaError(authmfa.ErrorInvalidProof)
		}
		record, err := fido.Updated()
		if err != nil {
			return MFAFinalization{}, err
		}
		id, _ := record.ID()
		expected, _ := fido.Expected()
		records, _ := snapshot.user.Records()
		matched := false
		for _, known := range records {
			knownID, _ := known.ID()
			knownVersion, _ := known.Version()
			if knownID == id && knownVersion == expected {
				matched = true
			}
		}
		if !matched {
			return MFAFinalization{}, mfaError(authmfa.ErrorConflict)
		}
		next, _ := record.Version()
		cipherBinding, err := NewMFACredentialBinding(snapshot.party, snapshot.account, id, next)
		if err != nil {
			return MFAFinalization{}, err
		}
		result.envelope, err = r.PrepareCredentialEnvelope(cipherBinding, next, record)
		if err != nil {
			return MFAFinalization{}, err
		}
		return result, nil
	}
	if recovery, ok := proof.Recovery(); ok {
		if !recoveryOffered {
			return MFAFinalization{}, mfaError(authmfa.ErrorInvalidProof)
		}
		batch, _ := recovery.Batch()
		id, _ := recovery.Code()
		digest, _ := recovery.Digest()
		raw, _ := digest.Bytes()
		if batch != snapshot.batch {
			return MFAFinalization{}, mfaError(authmfa.ErrorConflict)
		}
		for _, candidate := range snapshot.candidates {
			cid, _ := candidate.ID()
			cd, _ := candidate.Digest()
			known, _ := cd.Bytes()
			if cid == id && raw == known {
				return result, nil
			}
		}
		return MFAFinalization{}, mfaError(authmfa.ErrorConflict)
	}
	return MFAFinalization{}, mfaError(authmfa.ErrorInvalidProof)
}
func (r *MFARepository) FinalizeLogin(ctx context.Context, creation SessionCreation, finalization MFAFinalization) (CreatedSession, error) {
	if finalization.mfaFinalizationValue == nil || !finalization.snapshot.valid() {
		return CreatedSession{}, mfaError(authmfa.ErrorInvalidInput)
	}
	snapshot := finalization.snapshot
	challenge, _ := snapshot.binding.Challenge()
	if creation.Account != snapshot.account || creation.Revision != snapshot.revision || creation.ChallengeID == nil || *creation.ChallengeID != challenge || creation.Now.IsZero() {
		return CreatedSession{}, mfaError(authmfa.ErrorConflict)
	}
	return mfaTransaction(r, ctx, func(tx pgx.Tx) (CreatedSession, error) {
		policy, err := lockMFAPolicy(ctx, tx)
		if err != nil {
			return CreatedSession{}, err
		}
		if !policy.same(snapshot.policy.version) {
			return CreatedSession{}, mfaError(authmfa.ErrorConflict)
		}
		enabled, err := lockMFAAccount(ctx, tx, creation.Account, creation.Revision)
		if err != nil {
			return CreatedSession{}, err
		}
		if !enabled {
			return CreatedSession{}, mfaError(authmfa.ErrorConflict)
		}
		account, _ := creation.Account.Value()
		id, _ := challenge.Value()
		var options, ciphertext []byte
		var expiry time.Time
		var revision int64
		var attempts int
		policyRevision, policyHash, _ := snapshot.policy.version.Values()
		err = tx.QueryRow(ctx, `SELECT l.options,l.expires_at,l.account_revision,l.attempts,c.ciphertext FROM public.auth_login_challenges l JOIN public.auth_mfa_ceremonies c ON c.account_id=l.account_id AND c.id=l.id AND c.kind=1 WHERE l.account_id=$1 AND l.id=$2 AND l.consumed_at IS NULL AND c.consumed_at IS NULL AND c.policy_revision=$3 AND c.policy_hash=$4 AND c.options_hash=$5 AND c.attempts BETWEEN 1 AND 5 FOR UPDATE OF l,c`, account, id, policyRevision, policyHash[:], snapshot.optionsHash[:]).Scan(&options, &expiry, &revision, &attempts, &ciphertext)
		if err != nil || revision != snapshot.revision.number || attempts < 1 || attempts > 5 || !sameTime(expiry, snapshot.expiry) || !creation.Now.Before(expiry) || !bytes.Equal(options, snapshot.options) || !bytes.Equal(ciphertext, snapshot.ceremonyCiphertext) {
			return CreatedSession{}, mfaError(authmfa.ErrorConflict)
		}
		evidence := sessionAuthentication{authenticatedAt: creation.Now}
		if fido, ok := finalization.proof.FIDO2(); ok {
			envelope := finalization.envelope
			updated, _ := fido.Updated()
			factor, _ := updated.ID()
			factorID, _ := factor.Value()
			expected, _ := fido.Expected()
			expectedNumber, _ := expected.Number()
			next, _ := updated.Version()
			nextNumber, _ := next.Number()
			expectedBinding, _ := NewMFACredentialBinding(snapshot.party, snapshot.account, factor, next)
			if !envelope.valid() || !envelope.binding.same(expectedBinding) {
				return CreatedSession{}, mfaError(authmfa.ErrorIntegrity)
			}
			if err := reserveMFASalt(ctx, tx, envelope); err != nil {
				return CreatedSession{}, err
			}
			tag, err := tx.Exec(ctx, `UPDATE public.auth_mfa_credentials SET record_version=$4,key_id=$5,salt=$6,ciphertext=$7,last_used_at=$8 WHERE account_id=$1 AND id=$2 AND record_version=$3 AND removed_at IS NULL`, account, factorID, int64(expectedNumber), int64(nextNumber), envelope.keyID[:], envelope.salt[:], envelope.ciphertext, creation.Now)
			if err != nil || tag.RowsAffected() != 1 {
				return CreatedSession{}, mfaError(authmfa.ErrorConflict)
			}
			evidence.satisfaction = authv1.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_FIDO2
			evidence.factor = factorID
		} else if recovery, ok := finalization.proof.Recovery(); ok {
			batch, _ := recovery.Batch()
			batchID, _ := batch.Value()
			code, _ := recovery.Code()
			codeID, _ := code.Value()
			digest, _ := recovery.Digest()
			raw, _ := digest.Bytes()
			tag, err := tx.Exec(ctx, `UPDATE public.auth_mfa_recovery_codes c SET consumed_at=$5 FROM public.auth_mfa_recovery_batches b WHERE c.account_id=$1 AND c.batch_id=$2 AND c.id=$3 AND c.digest=$4 AND c.consumed_at IS NULL AND b.account_id=c.account_id AND b.id=c.batch_id AND b.retired_at IS NULL`, account, batchID, codeID, raw[:], creation.Now)
			if err != nil || tag.RowsAffected() != 1 {
				return CreatedSession{}, mfaError(authmfa.ErrorConsumed)
			}
			evidence.satisfaction = authv1.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_RECOVERY_CODE
			evidence.batch = batchID
			evidence.code = codeID
		} else {
			return CreatedSession{}, mfaError(authmfa.ErrorInvalidProof)
		}
		if _, err := tx.Exec(ctx, `UPDATE public.auth_mfa_ceremonies SET consumed_at=$3 WHERE account_id=$1 AND id=$2 AND kind=1`, account, id, creation.Now); err != nil {
			return CreatedSession{}, mfaError(authmfa.ErrorUnavailable)
		}
		created, err := r.store.createSessionTx(ctx, tx, creation, evidence)
		if err != nil {
			return CreatedSession{}, mfaError(authmfa.ErrorUnavailable)
		}
		return created, nil
	})
}

type MFALoginChallenge struct{ *mfaLoginChallengeValue }
type mfaLoginChallengeValue struct {
	creation ChallengeCreation
	policy   authmfa.ExpectedPolicyVersion
	ceremony authmfa.PreparedCeremony
	envelope MFAEnvelope
}

func (r *MFARepository) PrepareLoginChallenge(creation ChallengeCreation, policy authmfa.ExpectedPolicyVersion, ceremony authmfa.PreparedCeremony) (MFALoginChallenge, error) {
	binding, err := ceremony.Binding()
	account, ae := binding.Account()
	id, ie := binding.Challenge()
	kind, ke := ceremony.Kind()
	party, pe := ceremony.RelyingParty()
	expiry, ee := ceremony.ExpiresAt()
	_, _, ve := policy.Values()
	if err != nil || ae != nil || ie != nil || ke != nil || pe != nil || ee != nil || ve != nil || kind != authmfa.CeremonyLogin || account != creation.Account || id != creation.ID || !sameTime(expiry, creation.Now.Add(challengeLifetime).Truncate(time.Microsecond)) {
		return MFALoginChallenge{}, mfaError(authmfa.ErrorInvalidInput)
	}
	var options authv1.FactorChallenge
	if err := proto.Unmarshal(creation.Payload, &options); err != nil {
		return MFALoginChallenge{}, mfaError(authmfa.ErrorInvalidInput)
	}
	if _, err := contracts.AdmitLoginDecision(&authv1.LoginDecision{Outcome: &authv1.LoginDecision_Challenge{Challenge: &options}}, creation.Now); err != nil {
		return MFALoginChallenge{}, mfaError(authmfa.ErrorInvalidInput)
	}
	expected, err := ceremony.AssertionOptions(creation.Now)
	if err != nil {
		return MFALoginChallenge{}, err
	}
	matched := false
	for _, option := range options.Options {
		if fido, ok := option.Method.(*authv1.FactorOption_Fido2); ok && fido != nil && proto.Equal(fido.Fido2, expected) {
			matched = true
		}
	}
	idText, _ := id.Value()
	if !matched || options.Id == nil || options.Id.Value != idText || !options.ExpiresAt.AsTime().Equal(expiry) {
		return MFALoginChallenge{}, mfaError(authmfa.ErrorIntegrity)
	}
	cipherBinding, err := NewMFACeremonyBinding(party, binding)
	if err != nil {
		return MFALoginChallenge{}, err
	}
	envelope, err := r.PrepareCeremonyEnvelope(cipherBinding, ceremony)
	if err != nil {
		return MFALoginChallenge{}, err
	}
	creation.Payload = append([]byte(nil), creation.Payload...)
	return MFALoginChallenge{&mfaLoginChallengeValue{creation: creation, policy: policy, ceremony: ceremony, envelope: envelope}}, nil
}
func insertMFACeremonyTx(ctx context.Context, tx pgx.Tx, account contracts.AccountID, revision RevocationRevision, policy authmfa.ExpectedPolicyVersion, kind authmfa.CeremonyKind, envelope MFAEnvelope, expiry time.Time, optionsHash [32]byte, authority MFAEnrollmentAuthority) error {
	if !envelope.valid() || envelope.binding.account != account || !revision.validFor(account) || expiry.IsZero() {
		return mfaError(authmfa.ErrorIntegrity)
	}
	id, _ := account.Value()
	instance, rp, _, _ := envelope.binding.party.Values()
	instanceText, _ := instance.Value()
	policyRevision, policyHash, err := policy.Values()
	if err != nil {
		return err
	}
	authKind := int16(3)
	var session any
	var grant any
	if kind == authmfa.CeremonyEnrollment {
		if authority.mfaEnrollmentAuthorityValue == nil {
			return mfaError(authmfa.ErrorInvalidInput)
		}
		switch authority.kind {
		case mfaEnrollmentSession:
			authKind = 1
			text, _ := authority.owner.Session.Value()
			session = text
		case mfaEnrollmentGrant:
			authKind = 2
			raw, _ := authority.grant.Bytes()
			grant = raw[:]
		default:
			return mfaError(authmfa.ErrorInvalidInput)
		}
	} else if kind != authmfa.CeremonyLogin {
		return mfaError(authmfa.ErrorInvalidInput)
	}
	// Bounded opportunistic GC never deletes salts, budgets or current challenges.
	if _, err := tx.Exec(ctx, `DELETE FROM public.auth_mfa_ceremonies WHERE (account_id,kind) IN(SELECT account_id,kind FROM public.auth_mfa_ceremonies WHERE expires_at<=$1 ORDER BY expires_at,account_id,kind LIMIT 64 FOR UPDATE SKIP LOCKED)`, expiry.Add(-authmfa.CeremonyLifetime)); err != nil {
		return mfaError(authmfa.ErrorUnavailable)
	}
	if err := reserveMFASalt(ctx, tx, envelope); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO public.auth_mfa_ceremonies(account_id,kind,id,account_revision,policy_revision,policy_hash,instance_id,rp_id,authority,session_id,grant_digest,options_hash,key_id,cipher_suite,salt,ciphertext,expires_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
 ON CONFLICT(account_id,kind) DO UPDATE SET id=EXCLUDED.id,account_revision=EXCLUDED.account_revision,policy_revision=EXCLUDED.policy_revision,policy_hash=EXCLUDED.policy_hash,instance_id=EXCLUDED.instance_id,rp_id=EXCLUDED.rp_id,authority=EXCLUDED.authority,session_id=EXCLUDED.session_id,grant_digest=EXCLUDED.grant_digest,options_hash=EXCLUDED.options_hash,key_id=EXCLUDED.key_id,cipher_suite=EXCLUDED.cipher_suite,salt=EXCLUDED.salt,ciphertext=EXCLUDED.ciphertext,expires_at=EXCLUDED.expires_at,attempts=0,consumed_at=NULL`, id, int16(kind), envelope.binding.record, revision.number, int64(policyRevision), policyHash[:], instanceText, rp, authKind, session, grant, optionsHash[:], envelope.keyID[:], secretSuiteSQL, envelope.salt[:], envelope.ciphertext, expiry)
	if err != nil {
		return mfaError(authmfa.ErrorUnavailable)
	}
	return nil
}
func (r *MFARepository) IssueLoginChallenge(ctx context.Context, input MFALoginChallenge) (ChallengeRecord, error) {
	if input.mfaLoginChallengeValue == nil {
		return ChallengeRecord{}, mfaError(authmfa.ErrorInvalidInput)
	}
	return mfaTransaction(r, ctx, func(tx pgx.Tx) (ChallengeRecord, error) {
		policy, err := lockMFAPolicy(ctx, tx)
		if err != nil {
			return ChallengeRecord{}, err
		}
		if !policy.same(input.policy) {
			return ChallengeRecord{}, mfaError(authmfa.ErrorConflict)
		}
		enabled, err := lockMFAAccount(ctx, tx, input.creation.Account, input.creation.Revision)
		if err != nil {
			return ChallengeRecord{}, err
		}
		if !enabled {
			return ChallengeRecord{}, mfaError(authmfa.ErrorConflict)
		}
		record, err := r.store.issueChallengeTx(ctx, tx, input.creation)
		if err != nil {
			return ChallengeRecord{}, mfaError(authmfa.ErrorUnavailable)
		}
		if err := insertMFACeremonyTx(ctx, tx, input.creation.Account, input.creation.Revision, input.policy, authmfa.CeremonyLogin, input.envelope, record.ExpiresAt, mfaOptionsHash(input.creation.Payload), MFAEnrollmentAuthority{}); err != nil {
			return ChallengeRecord{}, err
		}
		return record, nil
	})
}

type mfaEnrollmentAuthorityKind uint8

const (
	mfaEnrollmentUnknown mfaEnrollmentAuthorityKind = iota
	mfaEnrollmentSession
	mfaEnrollmentGrant
)

type MFAEnrollmentAuthority struct{ *mfaEnrollmentAuthorityValue }
type mfaEnrollmentAuthorityValue struct {
	kind  mfaEnrollmentAuthorityKind
	owner SessionOwner
	grant accounts.EnrollmentDigest
}

func MFAEnrollmentFromSession(owner SessionOwner) (MFAEnrollmentAuthority, error) {
	if _, err := sqlAccountID(owner.Account); err != nil {
		return MFAEnrollmentAuthority{}, mfaError(authmfa.ErrorInvalidInput)
	}
	if _, err := owner.Session.Value(); err != nil {
		return MFAEnrollmentAuthority{}, mfaError(authmfa.ErrorInvalidInput)
	}
	return MFAEnrollmentAuthority{&mfaEnrollmentAuthorityValue{kind: mfaEnrollmentSession, owner: owner}}, nil
}
func MFAEnrollmentFromGrant(grant accounts.EnrollmentDigest) (MFAEnrollmentAuthority, error) {
	if _, err := grant.Bytes(); err != nil {
		return MFAEnrollmentAuthority{}, mfaError(authmfa.ErrorInvalidInput)
	}
	return MFAEnrollmentAuthority{&mfaEnrollmentAuthorityValue{kind: mfaEnrollmentGrant, grant: grant}}, nil
}
func (MFAEnrollmentAuthority) String() string                 { return "[REDACTED MFA ENROLLMENT AUTHORITY]" }
func (a MFAEnrollmentAuthority) GoString() string             { return a.String() }
func (a MFAEnrollmentAuthority) MarshalJSON() ([]byte, error) { return json.Marshal(a.String()) }
func admitEnrollmentAuthorityTx(ctx context.Context, tx pgx.Tx, authority MFAEnrollmentAuthority, account contracts.AccountID, revision RevocationRevision, policy MFAPolicySnapshot, now time.Time) error {
	if authority.mfaEnrollmentAuthorityValue == nil {
		return mfaError(authmfa.ErrorInvalidProof)
	}
	switch authority.kind {
	case mfaEnrollmentSession:
		if authority.owner.Account != account {
			return mfaError(authmfa.ErrorInvalidProof)
		}
		if err := requireCurrentMFASessionTx(ctx, tx, authority.owner, now); err != nil {
			return err
		}
		// Enrollment creates or replaces the recovery batch. Recheck original-auth
		// recency under the account lock even for the first factor and legacy
		// sessions without original-authentication evidence.
		return requireRecentMFASessionTx(ctx, tx, authority.owner, policy, now)
	case mfaEnrollmentGrant:
		found, current, err := lookupMFAEnrollmentGrant(ctx, tx, authority.grant, now)
		if err != nil || found != account || current != revision {
			return mfaError(authmfa.ErrorInvalidProof)
		}
		return nil
	default:
		return mfaError(authmfa.ErrorInvalidInput)
	}
}

// LookupEnrollmentGrant is a preliminary identity read, never authorization.
func (r *MFARepository) LookupEnrollmentGrant(ctx context.Context, digest accounts.EnrollmentDigest, now time.Time) (contracts.AccountID, RevocationRevision, error) {
	raw, err := digest.Bytes()
	if !r.valid() || accountContextError(ctx) != nil || err != nil || now.IsZero() {
		return contracts.AccountID{}, RevocationRevision{}, mfaError(authmfa.ErrorInvalidInput)
	}
	var text string
	var revision int64
	if err := r.store.pool.QueryRow(ctx, `SELECT account_id,account_revision FROM public.auth_enrollment_grants WHERE token_digest=$1 AND expires_at>$2 AND consumed_at IS NULL`, raw[:], now).Scan(&text, &revision); err != nil {
		return contracts.AccountID{}, RevocationRevision{}, mfaError(authmfa.ErrorInvalidProof)
	}
	account, err := contracts.ParseAccountID(text)
	if err != nil || revision <= 0 {
		return contracts.AccountID{}, RevocationRevision{}, mfaError(authmfa.ErrorIntegrity)
	}
	return account, RevocationRevision{account: account, number: revision}, nil
}

type MFABegin struct{ *mfaBeginValue }
type mfaBeginValue struct {
	account     contracts.AccountID
	revision    RevocationRevision
	policy      authmfa.ExpectedPolicyVersion
	ceremony    authmfa.PreparedCeremony
	envelope    MFAEnvelope
	authority   MFAEnrollmentAuthority
	user        authmfa.UserSnapshot
	now         time.Time
	optionsHash [32]byte
}
type MFACeremony struct{ *mfaCeremonyValue }
type mfaCeremonyValue struct {
	account     contracts.AccountID
	revision    RevocationRevision
	policy      MFAPolicySnapshot
	ceremony    authmfa.PreparedCeremony
	user        authmfa.UserSnapshot
	authority   MFAEnrollmentAuthority
	ciphertext  []byte
	optionsHash [32]byte
}

func (MFACeremony) String() string                 { return "[REDACTED MFA ENROLLMENT SNAPSHOT]" }
func (v MFACeremony) GoString() string             { return v.String() }
func (v MFACeremony) MarshalJSON() ([]byte, error) { return json.Marshal(v.String()) }
func (r *MFARepository) PrepareEnrollmentBegin(account contracts.AccountID, revision RevocationRevision, policy authmfa.ExpectedPolicyVersion, ceremony authmfa.PreparedCeremony, user authmfa.UserSnapshot, authority MFAEnrollmentAuthority, now time.Time) (MFABegin, error) {
	binding, err := ceremony.Binding()
	boundAccount, ae := binding.Account()
	kind, ke := ceremony.Kind()
	party, pe := ceremony.RelyingParty()
	expiry, ee := ceremony.ExpiresAt()
	if err != nil || ae != nil || ke != nil || pe != nil || ee != nil || kind != authmfa.CeremonyEnrollment || boundAccount != account || !revision.validFor(account) || !sameTime(expiry, now.Add(authmfa.CeremonyLifetime).Truncate(time.Microsecond)) || authority.mfaEnrollmentAuthorityValue == nil {
		return MFABegin{}, mfaError(authmfa.ErrorInvalidInput)
	}
	options, err := ceremony.EnrollmentOptions(user, now)
	if err != nil {
		return MFABegin{}, err
	}
	raw, err := proto.Marshal(options)
	if err != nil || len(raw) > 16384 {
		return MFABegin{}, mfaError(authmfa.ErrorInvalidInput)
	}
	cipherBinding, err := NewMFACeremonyBinding(party, binding)
	if err != nil {
		return MFABegin{}, err
	}
	envelope, err := r.PrepareCeremonyEnvelope(cipherBinding, ceremony)
	if err != nil {
		return MFABegin{}, err
	}
	return MFABegin{&mfaBeginValue{account: account, revision: revision, policy: policy, ceremony: ceremony, envelope: envelope, authority: authority, user: user, now: now, optionsHash: mfaOptionsHash(raw)}}, nil
}
func (r *MFARepository) BeginEnrollment(ctx context.Context, input MFABegin) (MFACeremony, error) {
	if input.mfaBeginValue == nil {
		return MFACeremony{}, mfaError(authmfa.ErrorInvalidInput)
	}
	return mfaTransaction(r, ctx, func(tx pgx.Tx) (MFACeremony, error) {
		policy, err := lockMFAPolicy(ctx, tx)
		if err != nil {
			return MFACeremony{}, err
		}
		if !policy.same(input.policy) {
			return MFACeremony{}, mfaError(authmfa.ErrorConflict)
		}
		if _, err := lockMFAAccount(ctx, tx, input.account, input.revision); err != nil {
			return MFACeremony{}, err
		}
		if err := admitEnrollmentAuthorityTx(ctx, tx, input.authority, input.account, input.revision, policy, input.now); err != nil {
			return MFACeremony{}, err
		}
		expiry, _ := input.ceremony.ExpiresAt()
		if err := insertMFACeremonyTx(ctx, tx, input.account, input.revision, input.policy, authmfa.CeremonyEnrollment, input.envelope, expiry, input.optionsHash, input.authority); err != nil {
			return MFACeremony{}, err
		}
		return MFACeremony{&mfaCeremonyValue{account: input.account, revision: input.revision, policy: policy, ceremony: input.ceremony, user: input.user, authority: input.authority, ciphertext: append([]byte(nil), input.envelope.ciphertext...), optionsHash: input.optionsHash}}, nil
	})
}
func (v MFACeremony) Verification() (authmfa.UserSnapshot, authmfa.PreparedCeremony, error) {
	if v.mfaCeremonyValue == nil {
		return authmfa.UserSnapshot{}, authmfa.PreparedCeremony{}, mfaError(authmfa.ErrorInvalidInput)
	}
	return v.user, v.ceremony, nil
}
func (r *MFARepository) EnrollmentSnapshot(ctx context.Context, id contracts.ChallengeID, authority MFAEnrollmentAuthority, now time.Time) (MFACeremony, error) {
	text, err := id.Value()
	if !r.valid() || accountContextError(ctx) != nil || err != nil || now.IsZero() || authority.mfaEnrollmentAuthorityValue == nil {
		return MFACeremony{}, mfaError(authmfa.ErrorInvalidInput)
	}
	var accountText, instanceText, rp, origin, suite string
	var revision, policyRevision int64
	var key, salt, ciphertext, policyHash, optionsHash, grant []byte
	var expiry time.Time
	var authKind int16
	var session *string
	err = r.store.pool.QueryRow(ctx, `SELECT c.account_id,c.account_revision,c.policy_revision,c.policy_hash,c.options_hash,c.instance_id,c.rp_id,i.origin,c.key_id,c.salt,c.ciphertext,c.cipher_suite,c.expires_at,c.authority,c.session_id,c.grant_digest FROM public.auth_mfa_ceremonies c JOIN public.auth_mfa_instance i ON i.instance_id=c.instance_id AND i.rp_id=c.rp_id WHERE c.id=$1 AND c.kind=2 AND c.consumed_at IS NULL AND c.expires_at>$2`, text, now).Scan(&accountText, &revision, &policyRevision, &policyHash, &optionsHash, &instanceText, &rp, &origin, &key, &salt, &ciphertext, &suite, &expiry, &authKind, &session, &grant)
	if err != nil {
		return MFACeremony{}, mfaError(authmfa.ErrorExpired)
	}
	account, ae := contracts.ParseAccountID(accountText)
	instance, ie := contracts.ParseInstanceID(instanceText)
	party, pe := authmfa.NewRelyingParty(instance, origin)
	_, actualRP, _, _ := party.Values()
	binding, be := authmfa.NewBinding(account, id, instance)
	if ae != nil || ie != nil || pe != nil || be != nil || rp != actualRP || suite != secretSuiteSQL || revision <= 0 || policyRevision <= 0 || len(optionsHash) != 32 {
		return MFACeremony{}, mfaError(authmfa.ErrorIntegrity)
	}
	switch authority.kind {
	case mfaEnrollmentSession:
		sessionText, se := authority.owner.Session.Value()
		if se != nil || authority.owner.Account != account || authKind != 1 || session == nil || *session != sessionText || grant != nil {
			return MFACeremony{}, mfaError(authmfa.ErrorInvalidProof)
		}
	case mfaEnrollmentGrant:
		raw, _ := authority.grant.Bytes()
		if authKind != 2 || session != nil || !bytes.Equal(raw[:], grant) {
			return MFACeremony{}, mfaError(authmfa.ErrorInvalidProof)
		}
	default:
		return MFACeremony{}, mfaError(authmfa.ErrorInvalidInput)
	}
	policy, err := r.MFAPolicy(ctx)
	if err != nil {
		return MFACeremony{}, err
	}
	expected, ee := authmfa.NewExpectedPolicyVersion(uint64(policyRevision), policyHash)
	if ee != nil || !policy.same(expected) {
		return MFACeremony{}, mfaError(authmfa.ErrorConflict)
	}
	cipherBinding, err := NewMFACeremonyBinding(party, binding)
	if err != nil {
		return MFACeremony{}, err
	}
	plain, err := r.openEnvelope(cipherBinding, key, salt, ciphertext)
	if err != nil {
		return MFACeremony{}, err
	}
	ceremony, err := authmfa.DecodePreparedCeremony(binding, party, plain, now)
	if err != nil {
		return MFACeremony{}, err
	}
	kind, _ := ceremony.Kind()
	deadline, _ := ceremony.ExpiresAt()
	if kind != authmfa.CeremonyEnrollment || !sameTime(deadline, expiry) {
		return MFACeremony{}, mfaError(authmfa.ErrorIntegrity)
	}
	user, err := r.UserSnapshot(ctx, account, party)
	if err != nil {
		return MFACeremony{}, err
	}
	var hash [32]byte
	copy(hash[:], optionsHash)
	options, err := ceremony.EnrollmentOptions(user, now)
	if err != nil {
		return MFACeremony{}, err
	}
	raw, err := proto.Marshal(options)
	if err != nil || mfaOptionsHash(raw) != hash {
		return MFACeremony{}, mfaError(authmfa.ErrorConflict)
	}
	return MFACeremony{&mfaCeremonyValue{account: account, revision: RevocationRevision{account: account, number: revision}, policy: policy, ceremony: ceremony, user: user, authority: authority, ciphertext: append([]byte(nil), ciphertext...), optionsHash: hash}}, nil
}
func (r *MFARepository) ReserveEnrollmentProof(ctx context.Context, snapshot MFACeremony, now time.Time) error {
	if snapshot.mfaCeremonyValue == nil || now.IsZero() {
		return mfaError(authmfa.ErrorInvalidInput)
	}
	_, err := mfaTransaction(r, ctx, func(tx pgx.Tx) (struct{}, error) {
		policy, err := lockMFAPolicy(ctx, tx)
		if err != nil {
			return struct{}{}, err
		}
		if !policy.same(snapshot.policy.version) {
			return struct{}{}, mfaError(authmfa.ErrorConflict)
		}
		if _, err := lockMFAAccount(ctx, tx, snapshot.account, snapshot.revision); err != nil {
			return struct{}{}, err
		}
		if err := admitEnrollmentAuthorityTx(ctx, tx, snapshot.authority, snapshot.account, snapshot.revision, policy, now); err != nil {
			return struct{}{}, err
		}
		binding, _ := snapshot.ceremony.Binding()
		challenge, _ := binding.Challenge()
		id, _ := challenge.Value()
		account, _ := snapshot.account.Value()
		tag, err := tx.Exec(ctx, `UPDATE public.auth_mfa_ceremonies SET attempts=attempts+1 WHERE account_id=$1 AND id=$2 AND kind=2 AND account_revision=$3 AND ciphertext=$4 AND options_hash=$5 AND expires_at>$6 AND consumed_at IS NULL AND attempts<5`, account, id, snapshot.revision.number, snapshot.ciphertext, snapshot.optionsHash[:], now)
		if err != nil || tag.RowsAffected() != 1 {
			return struct{}{}, mfaError(authmfa.ErrorExpired)
		}
		return struct{}{}, reserveMFABudgetTx(ctx, tx, snapshot.account, mfaBudgetProof, now)
	})
	return err
}

type MFAEnrollmentCommit struct{ *mfaEnrollmentCommitValue }
type mfaEnrollmentCommitValue struct {
	snapshot MFACeremony
	proof    authmfa.VerifiedEnrollment
	envelope MFAEnvelope
	batch    MFARecoveryBatch
}
type MFAActivation struct {
	created time.Time
	result  MFAResetResult
	factor  contracts.FactorCredentialID
}

func (v MFAActivation) Result() (MFAResetResult, contracts.FactorCredentialID, error) {
	if _, _, err := v.result.Account(); err != nil {
		return MFAResetResult{}, contracts.FactorCredentialID{}, err
	}
	if _, err := v.factor.Value(); err != nil {
		return MFAResetResult{}, contracts.FactorCredentialID{}, err
	}
	return v.result, v.factor, nil
}
func (r *MFARepository) PrepareEnrollmentCommit(proof authmfa.VerifiedEnrollment, snapshot MFACeremony, batch MFARecoveryBatch) (MFAEnrollmentCommit, error) {
	if snapshot.mfaCeremonyValue == nil || batch.mfaRecoveryBatchValue == nil || batch.account != snapshot.account {
		return MFAEnrollmentCommit{}, mfaError(authmfa.ErrorInvalidInput)
	}
	binding, err := proof.Binding()
	expected, ee := snapshot.ceremony.Binding()
	digest, de := proof.CeremonyDigest()
	expectedDigest, ce := snapshot.ceremony.Digest()
	if err != nil || ee != nil || de != nil || ce != nil || digest != expectedDigest || binding != expected {
		return MFAEnrollmentCommit{}, mfaError(authmfa.ErrorInvalidProof)
	}
	record, err := proof.Record()
	if err != nil {
		return MFAEnrollmentCommit{}, err
	}
	party, _ := record.RelyingParty()
	factor, _ := record.ID()
	version, _ := record.Version()
	number, _ := version.Number()
	if number != 1 {
		return MFAEnrollmentCommit{}, mfaError(authmfa.ErrorIntegrity)
	}
	cipherBinding, err := NewMFACredentialBinding(party, snapshot.account, factor, version)
	if err != nil {
		return MFAEnrollmentCommit{}, err
	}
	envelope, err := r.PrepareCredentialEnvelope(cipherBinding, version, record)
	if err != nil {
		return MFAEnrollmentCommit{}, err
	}
	return MFAEnrollmentCommit{&mfaEnrollmentCommitValue{snapshot: snapshot, proof: proof, envelope: envelope, batch: batch}}, nil
}
func (r *MFARepository) CompleteEnrollment(ctx context.Context, input MFAEnrollmentCommit, now time.Time) (MFAActivation, error) {
	if input.mfaEnrollmentCommitValue == nil || input.snapshot.mfaCeremonyValue == nil || now.IsZero() || !input.envelope.valid() {
		return MFAActivation{}, mfaError(authmfa.ErrorInvalidInput)
	}
	snapshot := input.snapshot
	return mfaTransaction(r, ctx, func(tx pgx.Tx) (MFAActivation, error) {
		policy, err := lockMFAPolicy(ctx, tx)
		if err != nil {
			return MFAActivation{}, err
		}
		if !policy.same(snapshot.policy.version) {
			return MFAActivation{}, mfaError(authmfa.ErrorConflict)
		}
		if _, err := lockMFAAccount(ctx, tx, snapshot.account, snapshot.revision); err != nil {
			return MFAActivation{}, err
		}
		if err := admitEnrollmentAuthorityTx(ctx, tx, snapshot.authority, snapshot.account, snapshot.revision, policy, now); err != nil {
			return MFAActivation{}, err
		}
		binding, _ := snapshot.ceremony.Binding()
		challenge, _ := binding.Challenge()
		challengeID, _ := challenge.Value()
		account, _ := snapshot.account.Value()
		var cipher, optionsHash []byte
		var expiry time.Time
		var attempts int
		if err := tx.QueryRow(ctx, `SELECT ciphertext,options_hash,expires_at,attempts FROM public.auth_mfa_ceremonies WHERE account_id=$1 AND id=$2 AND kind=2 AND account_revision=$3 AND consumed_at IS NULL FOR UPDATE`, account, challengeID, snapshot.revision.number).Scan(&cipher, &optionsHash, &expiry, &attempts); err != nil || !now.Before(expiry) || attempts < 1 || attempts > 5 || !bytes.Equal(cipher, snapshot.ciphertext) || !bytes.Equal(optionsHash, snapshot.optionsHash[:]) {
			return MFAActivation{}, mfaError(authmfa.ErrorConflict)
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM public.auth_mfa_credentials WHERE account_id=$1 AND removed_at IS NULL`, account).Scan(&count); err != nil || count >= authmfa.CredentialLimit {
			return MFAActivation{}, mfaError(authmfa.ErrorBusy)
		}
		record, err := input.proof.Record()
		if err != nil {
			return MFAActivation{}, err
		}
		factor, _ := record.ID()
		factorText, _ := factor.Value()
		envelope := input.envelope
		instance, rp, _, _ := envelope.binding.party.Values()
		instanceText, _ := instance.Value()
		if err := reserveMFASalt(ctx, tx, envelope); err != nil {
			return MFAActivation{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public.auth_mfa_credentials(account_id,id,instance_id,rp_id,record_version,key_id,cipher_suite,salt,ciphertext,created_at) VALUES($1,$2,$3,$4,1,$5,$6,$7,$8,$9)`, account, factorText, instanceText, rp, envelope.keyID[:], secretSuiteSQL, envelope.salt[:], envelope.ciphertext, now); err != nil {
			return MFAActivation{}, mfaError(authmfa.ErrorConflict)
		}
		if err := replaceMFARecoveryTx(ctx, tx, input.batch, now); err != nil {
			return MFAActivation{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE public.auth_accounts SET second_factor_enabled=true WHERE id=$1`, account); err != nil {
			return MFAActivation{}, mfaError(authmfa.ErrorUnavailable)
		}
		// revokeMFAAccountTx deletes this exact ceremony and the grant together with
		// every stale challenge; their absence is durable consumption in this tx.
		if err := revokeMFAAccountTx(ctx, tx, snapshot.account, now); err != nil {
			return MFAActivation{}, err
		}
		if err := insertSessionAudit(ctx, tx, snapshot.account, authActionMFAEnroll, authOutcomeAdmitted); err != nil {
			return MFAActivation{}, mfaError(authmfa.ErrorUnavailable)
		}
		result, err := mfaResetResultTx(ctx, tx, snapshot.account)
		if err != nil {
			return MFAActivation{}, err
		}
		return MFAActivation{result: result, factor: factor, created: now.Truncate(time.Microsecond)}, nil
	})
}

func validateMFALoginOptions(ceremony authmfa.PreparedCeremony, raw []byte, now time.Time) error {
	var offered authv1.FactorChallenge
	if len(raw) == 0 || len(raw) > 16384 {
		return mfaError(authmfa.ErrorIntegrity)
	}
	if err := proto.Unmarshal(raw, &offered); err != nil {
		return mfaError(authmfa.ErrorIntegrity)
	}
	if _, err := contracts.AdmitLoginDecision(&authv1.LoginDecision{Outcome: &authv1.LoginDecision_Challenge{Challenge: &offered}}, now); err != nil {
		return mfaError(authmfa.ErrorIntegrity)
	}
	binding, err := ceremony.Binding()
	if err != nil {
		return err
	}
	id, _ := binding.Challenge()
	text, _ := id.Value()
	expiry, _ := ceremony.ExpiresAt()
	expected, err := ceremony.AssertionOptions(now)
	if err != nil {
		return err
	}
	if offered.Id == nil || offered.Id.Value != text || !offered.ExpiresAt.AsTime().Equal(expiry) {
		return mfaError(authmfa.ErrorIntegrity)
	}
	found := false
	for _, option := range offered.Options {
		switch method := option.Method.(type) {
		case *authv1.FactorOption_Fido2:
			if method == nil || !proto.Equal(method.Fido2, expected) {
				return mfaError(authmfa.ErrorIntegrity)
			}
			found = true
		case *authv1.FactorOption_RecoveryCode:
			if method == nil || method.RecoveryCode == nil {
				return mfaError(authmfa.ErrorIntegrity)
			}
		default:
			return mfaError(authmfa.ErrorIntegrity)
		}
	}
	if !found {
		return mfaError(authmfa.ErrorIntegrity)
	}
	return nil
}
