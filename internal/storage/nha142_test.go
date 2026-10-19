package storage

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestNHA142MFAEnvelopeDomainsAndRoundTrip(t *testing.T) {
	key, err := newMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{pool: &pgxpool.Pool{}}
	protector := &SecretRepository{store: store, key: key}
	repository, err := store.NewMFARepository(protector)
	if err != nil {
		t.Fatal(err)
	}
	account, _ := contracts.ParseAccountID("mfa-account")
	instance, _ := contracts.ParseInstanceID("mfa-instance")
	party, _ := authmfa.NewRelyingParty(instance, "https://mfa.example.test")
	factor, _ := contracts.ParseFactorCredentialID("mfa-factor")
	version, _ := authmfa.NewRecordVersion(1)
	binding, _ := NewMFACredentialBinding(party, account, factor, version)
	plain := []byte("NHA142-PLAINTEXT-CANARY")
	envelope, err := repository.prepareEnvelope(binding, plain)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := repository.openEnvelope(binding, envelope.keyID[:], envelope.salt[:], envelope.ciphertext)
	if err != nil || !bytes.Equal(opened, plain) {
		t.Fatal("envelope roundtrip")
	}
	opened[0] = '!'
	again, _ := repository.openEnvelope(binding, envelope.keyID[:], envelope.salt[:], envelope.ciphertext)
	if !bytes.Equal(again, plain) {
		t.Fatal("returned bytes alias protected data")
	}
	otherAccount, _ := contracts.ParseAccountID("other-account")
	otherVersion, _ := version.Next()
	otherFactor, _ := contracts.ParseFactorCredentialID("other-factor")
	otherParty, _ := authmfa.NewRelyingParty(instance, "https://other.example.test")
	otherInstance, _ := contracts.ParseInstanceID("other-instance")
	instanceParty, _ := authmfa.NewRelyingParty(otherInstance, "https://mfa.example.test")
	for _, tuple := range []struct {
		party   authmfa.RelyingParty
		account contracts.AccountID
		factor  contracts.FactorCredentialID
		version authmfa.RecordVersion
	}{{party, otherAccount, factor, version}, {party, account, otherFactor, version}, {party, account, factor, otherVersion}, {otherParty, account, factor, version}, {instanceParty, account, factor, version}} {
		wrong, _ := NewMFACredentialBinding(tuple.party, tuple.account, tuple.factor, tuple.version)
		if _, err := repository.openEnvelope(wrong, envelope.keyID[:], envelope.salt[:], envelope.ciphertext); authmfa.KindOf(err) != authmfa.ErrorIntegrity {
			t.Fatal("cross-binding envelope opened")
		}
	}
	challenge, _ := contracts.ParseChallengeID("mfa-factor")
	proofBinding, _ := authmfa.NewBinding(account, challenge, instance)
	ceremonyBinding, _ := NewMFACeremonyBinding(party, proofBinding)
	if _, err := repository.openEnvelope(ceremonyBinding, envelope.keyID[:], envelope.salt[:], envelope.ciphertext); err == nil {
		t.Fatal("credential moved into ceremony domain")
	}
	// Independent legacy derivation opens integration ciphertext produced by the
	// factored helper, proving the existing domain/AAD/nonce encoding is retained.
	integration, _ := domain.ParseIntegrationInstanceID("nha142-test")
	name, _ := ParseSecretName("test-token")
	handle, _ := NewSecretHandle(integration, name)
	salt := bytes.Repeat([]byte{7}, 32)
	protected, err := protector.seal(handle, 1, salt, plain)
	if err != nil {
		t.Fatal(err)
	}
	derived, err := hkdf.Key(sha256.New, key.bytes[:], salt, "homeward/integration-secret/aes256gcm/v1", 32)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(derived)
	legacy, _ := cipher.NewGCMWithRandomNonce(block)
	legacyPlain, err := legacy.Open(nil, nil, protected, secretAAD(handle, 1, key.id[:], salt))
	if err != nil || !bytes.Equal(legacyPlain, plain) {
		t.Fatal("integration ciphertext encoding changed")
	}
	for _, value := range []any{envelope, struct{ private MFAEnvelope }{envelope}, struct{ private any }{envelope}, repository, struct{ private any }{repository}} {
		for _, verb := range []string{"%v", "%+v", "%#v"} {
			if strings.Contains(fmt.Sprintf(verb, value), string(plain)) {
				t.Fatal("nested sensitive diagnostic")
			}
		}
	}
	if _, err := store.NewMFARepository(nil); err == nil {
		t.Fatal("nil protector admitted")
	}
	if _, err := (&Store{pool: &pgxpool.Pool{}}).NewMFARepository(protector); err == nil {
		t.Fatal("foreign protector admitted")
	}
	if _, err := repository.PrepareFinalization(authmfa.VerifiedLogin{}, MFALoginSnapshot{}); err == nil {
		t.Fatal("zero proof or snapshot admitted")
	}
}
func TestNHA142PolicySourceCASAndWireAdmission(t *testing.T) {
	for _, required := range []bool{false, true} {
		source := authmfa.NewPolicySource(required)
		raw, _ := source.Bytes()
		parsed, err := authmfa.AdmitPolicySource(raw)
		if err != nil {
			t.Fatal(err)
		}
		value, _ := parsed.Required()
		if value != required {
			t.Fatal("policy source changed")
		}
		hash, _ := source.SHA256()
		if sha256.Sum256(raw) != hash {
			t.Fatal("source hash mismatch")
		}
		raw[0] = '!'
		if _, err := authmfa.AdmitPolicySource(raw); err == nil {
			t.Fatal("noncanonical policy source admitted")
		}
	}
	hash := bytes.Repeat([]byte{0x61}, 32)
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, revision := range []uint64{0, uint64(math.MaxInt64) + 1} {
		if _, err := authmfa.NewExpectedPolicyVersion(revision, hash); err == nil {
			t.Fatal("invalid SQL revision admitted")
		}
	}
	for _, request := range []*authv1.SetSecondFactorPolicyRequest{{}, {Expected: &authv1.PolicyVersion{Revision: 1, SourceSha256: hash[:31]}}, {Expected: &authv1.PolicyVersion{Revision: uint64(math.MaxInt64) + 1, SourceSha256: hash}}} {
		if _, err := contracts.AdmitSetSecondFactorPolicy(request); err == nil {
			t.Fatal("invalid CAS admitted")
		}
	}
	request := &authv1.SetSecondFactorPolicyRequest{Expected: &authv1.PolicyVersion{Revision: 1, SourceSha256: hash}}
	admitted, err := contracts.AdmitSetSecondFactorPolicy(request)
	if err != nil {
		t.Fatal(err)
	}
	request.Expected.SourceSha256[0] = 0
	snapshot, _ := admitted.Snapshot()
	if snapshot.Expected.SourceSha256[0] != 0x61 {
		t.Fatal("CAS snapshot aliases input")
	}
	policy := &authv1.SecondFactorPolicy{Version: &authv1.PolicyVersion{Revision: 1, SourceSha256: bytes.Repeat([]byte{1}, 32)}, UpdatedAt: timestamppb.New(now)}
	for _, continuation := range []authv1.SecondFactorSessionContinuation{authv1.SecondFactorSessionContinuation_SECOND_FACTOR_SESSION_CONTINUATION_UNSPECIFIED, authv1.SecondFactorSessionContinuation(99)} {
		if _, err := contracts.AdmitSetSecondFactorPolicyResponse(&authv1.SetSecondFactorPolicyResponse{Policy: policy, Continuation: continuation}); err == nil {
			t.Fatal("unknown continuation admitted")
		}
	}
}

func TestNHA142PolicyBootstrapCASRetentionAndCorruption(t *testing.T) {
	store, ctx := nha151UninitializedStore(t)
	// No protector, HTTPS endpoint or user exists yet. Bootstrap is versioned
	// metadata and cannot depend on credentials or reset a corrupt pointer.
	first, err := store.BootstrapMFAPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	source, err := first.Source()
	if err != nil {
		t.Fatal(err)
	}
	required, _ := source.Required()
	version, _ := first.Version()
	revision, hash, _ := version.Values()
	when, _ := first.UpdatedAt()
	if required || revision != 1 || when.IsZero() {
		t.Fatal("initial policy was not explicit version1 false")
	}
	want, _ := authmfa.NewPolicySource(false).SHA256()
	if hash != want {
		t.Fatal("initial policy lost exact source hash")
	}
	again, err := store.BootstrapMFAPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	againVersion, _ := again.Version()
	againTime, _ := again.UpdatedAt()
	if againVersion != version || !againTime.Equal(when) {
		t.Fatal("bootstrap rewrote existing policy")
	}
	var identities int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.auth_mfa_instance`).Scan(&identities); err != nil || identities != 0 {
		t.Fatal("metadata bootstrap invented an RP", err)
	}

	protector, err := store.BootstrapSecrets(ctx, filepath.Join(t.TempDir(), "private-data"))
	if err != nil {
		t.Fatal(err)
	}
	repository, err := store.NewMFARepository(protector)
	if err != nil {
		t.Fatal(err)
	}
	admin := nha049AccountInput(t, "NHA142 policy admin", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	if _, err := store.BootstrapAdmin(ctx, admin); err != nil {
		t.Fatal(err)
	}
	state, err := repository.AccountState(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	requirement, _, _, err := state.Requirement()
	if err != nil || requirement != MFALoginPasswordOnly {
		t.Fatal("default forced MFA before explicit policy", err)
	}
	if _, err := repository.SetMFAPolicy(ctx, admin.ID, version, authmfa.NewPolicySource(true), time.Now()); authmfa.KindOf(err) != authmfa.ErrorUnavailable {
		t.Fatal("policy activated without a persisted RP", err)
	}
	beforeRP, err := repository.MFAPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	beforeRPVersion, _ := beforeRP.Version()
	if beforeRPVersion != version {
		t.Fatal("refused activation changed the version")
	}
	if _, err := repository.BootstrapMFAInstance(ctx, "https://nha142.example.test"); err != nil {
		t.Fatal(err)
	}
	write, err := repository.SetMFAPolicy(ctx, admin.ID, version, authmfa.NewPolicySource(true), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	current, affected, changed, err := write.Result()
	if err != nil || !changed || len(affected) != 1 || affected[0] != admin.ID {
		t.Fatal("activation cohort was not exact", err)
	}
	state, err = repository.AccountState(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	requirement, _, _, err = state.Requirement()
	if err != nil || requirement != MFALoginEnrollment {
		t.Fatal("unenrolled admin did not require enrollment", err)
	}
	if _, err := repository.SetMFAPolicy(ctx, admin.ID, version, authmfa.NewPolicySource(true), time.Now()); authmfa.KindOf(err) != authmfa.ErrorConflict {
		t.Fatal("stale same-value CAS accepted", err)
	}
	currentVersion, _ := current.Version()
	currentTime, _ := current.UpdatedAt()
	noop, err := repository.SetMFAPolicy(ctx, admin.ID, currentVersion, authmfa.NewPolicySource(true), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	unchanged, cohort, changed, err := noop.Result()
	if err != nil || changed || len(cohort) != 0 {
		t.Fatal("exact no-op revoked accounts", err)
	}
	noopVersion, _ := unchanged.Version()
	noopTime, _ := unchanged.UpdatedAt()
	if noopVersion != currentVersion || !noopTime.Equal(currentTime) {
		t.Fatal("no-op created a version/date")
	}

	// ABA uses a new revision despite the same hash. GC must keep exactly20
	// versions and protect the currently active pointer, including this kind.
	for i := 0; i < 21; i++ {
		write, err := repository.SetMFAPolicy(ctx, admin.ID, currentVersion, authmfa.NewPolicySource(i%2 == 1), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		current, _, _, err = write.Result()
		if err != nil {
			t.Fatal(err)
		}
		currentVersion, _ = current.Version()
	}
	latest, _, _ := currentVersion.Values()
	var count int
	var oldest, newest int64
	if err := store.pool.QueryRow(ctx, `SELECT count(*),min(revision),max(revision) FROM public.config_versions WHERE document_id='security:mfa-policy'`).Scan(&count, &oldest, &newest); err != nil || count != 20 || oldest != int64(latest)-19 || newest != int64(latest) {
		t.Fatalf("policy retention count=%d range=%d..%d want latest=%d err=%v", count, oldest, newest, latest, err)
	}
	_, err = store.pool.Exec(ctx, `DELETE FROM public.config_versions WHERE document_id='security:mfa-policy' AND revision=$1`, newest)
	nha151SQLState(t, err, "23514")
	// Deliberate isolated corruption: bootstrap must fail closed instead of
	// fabricating a new false policy after losing its persisted pointer.
	if _, err := store.pool.Exec(ctx, `DROP TRIGGER auth_mfa_policy_guard ON public.auth_mfa_policy; DELETE FROM public.auth_mfa_policy`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapMFAPolicy(ctx); authmfa.KindOf(err) != authmfa.ErrorIntegrity {
		t.Fatal("corrupt policy silently reset", err)
	}
}
