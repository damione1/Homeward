package authmfa

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/fxamacker/cbor/v2"
	"google.golang.org/protobuf/proto"
)

func nha142Identities(t *testing.T) (Binding, RelyingParty) {
	t.Helper()
	account, _ := contracts.ParseAccountID("nha142-account")
	challenge, _ := contracts.ParseChallengeID("nha142-challenge")
	instance, _ := contracts.ParseInstanceID("nha142-instance")
	binding, err := NewBinding(account, challenge, instance)
	if err != nil {
		t.Fatal(err)
	}
	party, err := NewRelyingParty(instance, "https://homeward.example.test:443/config")
	if err != nil {
		t.Fatal(err)
	}
	return binding, party
}

func TestNHA142RecoveryAdmissionBindingAndRedaction(t *testing.T) {
	fixture := nha142NewFixture(t)
	fixture.enroll(t)
	binding := fixture.binding
	ceremony, err := fixture.verifier.BeginLogin(context.Background(), binding, fixture.user, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	batch, candidates, codes, err := GenerateRecoveryBatch(binding.account)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != RecoveryCodeCount || len(codes) != RecoveryCodeCount {
		t.Fatal("batch size")
	}
	text, err := codes[0].Text()
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{text, strings.ToLower(text), strings.ReplaceAll(text, "-", "")} {
		parsed, err := ParseRecoveryCode(input)
		if err != nil {
			t.Fatal(err)
		}
		verified, err := VerifyRecoveryCode(binding, ceremony, batch, parsed, candidates)
		if err != nil {
			t.Fatal(err)
		}
		proof, ok := verified.Recovery()
		if !ok || proof.code != candidates[0].id {
			t.Fatal("wrong recovery variant")
		}
		if _, ok := verified.FIDO2(); ok {
			t.Fatal("recovery admitted as FIDO2")
		}
	}
	for _, bad := range []string{"", text + "A", " " + text, strings.Replace(text, "-", "_", 1), strings.Repeat("A", 26), strings.Repeat("Ｏ", 26)} {
		if _, err := ParseRecoveryCode(bad); err == nil {
			t.Fatal("malformed recovery code admitted")
		}
	}
	otherAccount, _ := contracts.ParseAccountID("another-account")
	otherBinding, _ := NewBinding(otherAccount, binding.challenge, binding.instance)
	if _, err := VerifyRecoveryCode(otherBinding, ceremony, batch, codes[0], candidates); err == nil {
		t.Fatal("cross-account recovery admitted")
	}
	otherBatch, _ := ParseRecoveryBatchID("other-batch")
	if _, err := VerifyRecoveryCode(binding, ceremony, otherBatch, codes[0], candidates); KindOf(err) != ErrorIntegrity {
		t.Fatal("cross-batch recovery admitted")
	}
	if _, err := VerifyRecoveryCode(binding, ceremony, batch, codes[0], append(candidates, candidates[0])); err == nil {
		t.Fatal("overlong candidate list admitted")
	}
	duplicate := append([]RecoveryCandidate(nil), candidates...)
	duplicate[1] = duplicate[0]
	if _, err := VerifyRecoveryCode(binding, ceremony, batch, codes[0], duplicate); KindOf(err) != ErrorIntegrity {
		t.Fatal("duplicate recovery identity admitted")
	}
	digest, _ := codes[0].Digest(binding.account, batch)
	otherDigest, _ := codes[0].Digest(otherAccount, batch)
	if digest.raw == otherDigest.raw {
		t.Fatal("account is not part of digest domain")
	}
	for _, value := range []any{codes[0], digest} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, diagnostic := range []string{fmt.Sprint(value), fmt.Sprintf("%#v", value), string(encoded)} {
			if strings.Contains(diagnostic, text) || !strings.Contains(diagnostic, "REDACTED") {
				t.Fatal("recovery diagnostic is not redacted")
			}
		}
	}
	if _, err := (RecoveryCode{}).Text(); err == nil {
		t.Fatal("zero code admitted")
	}
	if _, err := (RecoveryDigest{}).Bytes(); err == nil {
		t.Fatal("zero digest admitted")
	}
	if _, err := (VerifiedLogin{}).Binding(); err == nil {
		t.Fatal("zero verification admitted")
	}
}

func TestNHA142SlidingBudgetsCopiesAndBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	input := []time.Time{now.Add(-BudgetWindow), now.Add(-time.Minute)}
	budget, err := AdmitBeginBudget(input, now)
	if err != nil {
		t.Fatal(err)
	}
	input[1] = now.Add(time.Hour)
	for index := 0; index < BeginBudgetLimit-1; index++ {
		budget, err = budget.Reserve(now)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := budget.Reserve(now); KindOf(err) != ErrorRateLimited {
		t.Fatal("fourth begin admitted")
	}
	exported, _ := budget.Timestamps()
	exported[0] = now.Add(time.Hour)
	if _, err := budget.Reserve(now.Add(BudgetWindow)); err != nil {
		t.Fatal("exact window boundary did not expire")
	}
	if _, err := AdmitBeginBudget([]time.Time{now.Add(time.Second)}, now); KindOf(err) != ErrorIntegrity {
		t.Fatal("future timestamp admitted")
	}
	if _, err := AdmitBeginBudget([]time.Time{now, now.Add(-time.Second)}, now); err == nil {
		t.Fatal("unordered budget admitted")
	}
	if _, err := (BeginBudget{}).Reserve(now); err == nil {
		t.Fatal("zero budget implicitly reset")
	}
	proofs, err := AdmitProofBudget(nil, now)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < ProofBudgetLimit; index++ {
		proofs, err = proofs.Reserve(now)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := proofs.Reserve(now); KindOf(err) != ErrorRateLimited {
		t.Fatal("eleventh proof admitted")
	}
	if IsRecentAuthentication(now, now.Add(RecentAuthenticationLifetime)) || IsRecentAuthentication(now.Add(time.Second), now) || IsRecentAuthentication(time.Time{}, now) {
		t.Fatal("invalid recent-authentication boundary")
	}
	if !IsRecentAuthentication(now, now.Add(RecentAuthenticationLifetime-time.Nanosecond)) {
		t.Fatal("recent original login rejected")
	}
	version, _ := NewRecordVersion(math.MaxInt64)
	if _, err := version.Next(); KindOf(err) != ErrorConflict {
		t.Fatal("record revision wrapped")
	}
	if _, err := NewRecordVersion(uint64(math.MaxInt64) + 1); err == nil {
		t.Fatal("out-of-SQL revision admitted")
	}
}

func TestNHA142CryptoGateCapacityCopiesCancellationAndDrain(t *testing.T) {
	gate := NewCryptoGate()
	permits := make([]*CryptoPermit, 0, CryptoActiveLimit)
	for index := 0; index < CryptoActiveLimit; index++ {
		permit, err := gate.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		permits = append(permits, permit)
	}
	defer func() {
		for _, permit := range permits {
			permit.Release()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan error, CryptoWaitingLimit)
	for index := 0; index < CryptoWaitingLimit; index++ {
		go func() {
			permit, err := gate.Acquire(ctx)
			if permit != nil {
				permit.Release()
			}
			results <- err
		}()
	}
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for len(gate.waiting) != CryptoWaitingLimit {
		select {
		case <-timeout.C:
			t.Fatal("waiting slots did not fill")
		case <-tick.C:
		}
	}
	if _, err := gate.Acquire(context.Background()); KindOf(err) != ErrorBusy {
		t.Fatal("thirteenth work item admitted")
	}
	cancel()
	for index := 0; index < CryptoWaitingLimit; index++ {
		select {
		case err := <-results:
			if !errors.Is(err, context.Canceled) {
				t.Fatal("queued cancellation lost")
			}
		case <-time.After(time.Second):
			t.Fatal("queued verifier leaked")
		}
	}
	// Copies share the private once-state: neither permit nor gate can be
	// duplicated into a second release/close authority by copying its wrapper.
	permitCopy := *permits[0]
	permits[0].Release()
	permitCopy.Release()
	gateCopy := *gate
	gate.CloseAdmissions()
	gateCopy.CloseAdmissions()
	if _, err := gate.Acquire(context.Background()); KindOf(err) != ErrorUnavailable {
		t.Fatal("closed gate admitted work")
	}
	for _, permit := range permits {
		permit.Release()
	}
	drainCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := gate.Drain(drainCtx); err != nil {
		t.Fatal(err)
	}
	if len(gate.active) != 0 || len(gate.waiting) != 0 {
		t.Fatal("retained gate slots")
	}
}

type nha142Fixture struct {
	binding  Binding
	party    RelyingParty
	verifier *Verifier
	user     UserSnapshot
	key      *ecdsa.PrivateKey
	id       []byte
	now      time.Time
}

func nha142NewFixture(t *testing.T) nha142Fixture {
	t.Helper()
	binding, party := nha142Identities(t)
	verifier, err := NewVerifier(party)
	if err != nil {
		t.Fatal(err)
	}
	handle := bytes.Repeat([]byte{0x31}, UserHandleBytes)
	user, err := NewUserSnapshot(binding.account, handle, "tester", "Test User", nil)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return nha142Fixture{binding: binding, party: party, verifier: verifier, user: user, key: key, id: bytes.Repeat([]byte{0x42}, 32), now: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
}

func nha142ClientData(t *testing.T, ceremony PreparedCeremony, kind, challenge, origin string) []byte {
	t.Helper()
	raw, err := json.Marshal(struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
		Origin    string `json:"origin"`
	}{Type: kind, Challenge: challenge, Origin: origin})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (f *nha142Fixture) enroll(t *testing.T) CredentialRecord {
	t.Helper()
	ceremony, err := f.verifier.BeginEnrollment(context.Background(), f.binding, f.user, f.now)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := ceremony.Challenge(f.now)
	if err != nil {
		t.Fatal(err)
	}
	clientData := nha142ClientData(t, ceremony, "webauthn.create", base64.RawURLEncoding.EncodeToString(challenge), f.party.origin)
	clientHash := sha256.Sum256(clientData)
	publicKey, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: f.key.X.FillBytes(make([]byte, 32)), -3: f.key.Y.FillBytes(make([]byte, 32))})
	if err != nil {
		t.Fatal(err)
	}
	rpHash := sha256.Sum256([]byte(f.party.rpID))
	authData := append([]byte(nil), rpHash[:]...)
	authData = append(authData, 0x45, 0, 0, 0, 0) // UP, UV, AT; initial counter zero
	authData = append(authData, make([]byte, 16)...)
	var size [2]byte
	binary.BigEndian.PutUint16(size[:], uint16(len(f.id)))
	authData = append(authData, size[:]...)
	authData = append(authData, f.id...)
	authData = append(authData, publicKey...)
	attestation, err := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": authData})
	if err != nil {
		t.Fatal(err)
	}
	factor, _ := contracts.ParseFactorCredentialID("nha142-factor")
	verified, err := f.verifier.VerifyEnrollment(context.Background(), f.user, ceremony, factor, &authv1.Fido2Attestation{CredentialId: f.id, ClientDataJson: clientData, AttestationObject: attestation, ClientDataHash: clientHash[:]}, f.now)
	if err != nil {
		t.Fatal(err)
	}
	record, err := verified.Record()
	if err != nil {
		t.Fatal(err)
	}
	f.user, err = NewUserSnapshot(f.binding.account, f.user.handle[:], f.user.name, f.user.display, []CredentialRecord{record})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func (f nha142Fixture) assertion(t *testing.T, ceremony PreparedCeremony, flags byte, counter uint32, challenge, rpID, origin string) *authv1.Fido2Assertion {
	t.Helper()
	clientData := nha142ClientData(t, ceremony, "webauthn.get", challenge, origin)
	clientHash := sha256.Sum256(clientData)
	rpHash := sha256.Sum256([]byte(rpID))
	authData := append([]byte(nil), rpHash[:]...)
	authData = append(authData, flags)
	var count [4]byte
	binary.BigEndian.PutUint32(count[:], counter)
	authData = append(authData, count[:]...)
	signed := append(append([]byte(nil), authData...), clientHash[:]...)
	digest := sha256.Sum256(signed)
	signature, err := ecdsa.SignASN1(rand.Reader, f.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return &authv1.Fido2Assertion{CredentialId: append([]byte(nil), f.id...), AuthenticatorData: authData, ClientDataJson: clientData, Signature: signature, UserHandle: f.user.WebAuthnID(), ClientDataHash: clientHash[:]}
}

func TestNHA142WebAuthnRealCryptoRecordRoundTripAndTampering(t *testing.T) {
	f := nha142NewFixture(t)
	record := f.enroll(t)
	raw, err := record.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCredentialRecord(f.binding.account, record.factor, f.party, record.version, raw)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := decoded.credential()
	if err != nil || byte(credential.Flags.ProtocolValue()) != 0x45 {
		t.Fatal("raw protocol flags lost on roundtrip")
	}
	raw[0] = '!'
	if _, err := decoded.credential(); err != nil {
		t.Fatal("caller mutated admitted record")
	}
	var document credentialDocument
	plain, _ := record.Bytes()
	if err := json.Unmarshal(plain, &document); err != nil {
		t.Fatal(err)
	}
	document.Flags = 0
	incoherent, _ := json.Marshal(document)
	if _, err := DecodeCredentialRecord(f.binding.account, record.factor, f.party, record.version, incoherent); KindOf(err) != ErrorIntegrity {
		t.Fatal("incoherent flags admitted")
	}
	ceremony, err := f.verifier.BeginLogin(context.Background(), f.binding, f.user, f.now)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := ceremony.Bytes()
	ceremony, err = DecodePreparedCeremony(f.binding, f.party, encoded, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePreparedCeremony(f.binding, f.party, encoded, f.now.Add(CeremonyLifetime)); KindOf(err) != ErrorExpired {
		t.Fatal("exact expiry admitted")
	}
	challenge, _ := ceremony.Challenge(f.now)
	challengeText := base64.RawURLEncoding.EncodeToString(challenge)
	valid := f.assertion(t, ceremony, 0x05, 0, challengeText, f.party.rpID, f.party.origin)
	verified, err := f.verifier.VerifyLogin(context.Background(), f.user, ceremony, valid, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := verified.FIDO2(); !ok {
		t.Fatal("valid zero-to-zero counter did not produce FIDO2 proof")
	}
	for _, item := range []struct {
		name                    string
		flags                   byte
		challenge, rpID, origin string
	}{
		{name: "challenge", flags: 0x05, challenge: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, ChallengeBytes)), rpID: f.party.rpID, origin: f.party.origin},
		{name: "rp", flags: 0x05, challenge: challengeText, rpID: "other.example.test", origin: f.party.origin},
		{name: "origin", flags: 0x05, challenge: challengeText, rpID: f.party.rpID, origin: "https://other.example.test"},
		{name: "without-uv", flags: 0x01, challenge: challengeText, rpID: f.party.rpID, origin: f.party.origin},
		{name: "without-up", flags: 0x04, challenge: challengeText, rpID: f.party.rpID, origin: f.party.origin},
	} {
		t.Run(item.name, func(t *testing.T) {
			// Each altered response is correctly re-signed so this tests its
			// own server guard independently of a corrupted signature.
			input := f.assertion(t, ceremony, item.flags, 1, item.challenge, item.rpID, item.origin)
			if _, err := f.verifier.VerifyLogin(context.Background(), f.user, ceremony, input, f.now); err == nil {
				t.Fatal("altered signed response admitted")
			}
		})
	}
	for _, mutate := range []func(*authv1.Fido2Assertion){
		func(input *authv1.Fido2Assertion) { input.Signature[0] ^= 1 },
		func(input *authv1.Fido2Assertion) { input.ClientDataHash[0] ^= 1 },
		func(input *authv1.Fido2Assertion) { input.UserHandle[0] ^= 1 },
		func(input *authv1.Fido2Assertion) { input.CredentialId[0] ^= 1 },
		func(input *authv1.Fido2Assertion) { input.ClientDataJson = make([]byte, MaxClientDataJSONBytes+1) },
	} {
		input := proto.Clone(valid).(*authv1.Fido2Assertion)
		mutate(input)
		if _, err := f.verifier.VerifyLogin(context.Background(), f.user, ceremony, input, f.now); err == nil {
			t.Fatal("invalid proof admitted")
		}
	}
}

func TestNHA142WebAuthnNonIncreasingCounterAndTypedNil(t *testing.T) {
	f := nha142NewFixture(t)
	f.enroll(t)
	ceremony, err := f.verifier.BeginLogin(context.Background(), f.binding, f.user, f.now)
	if err != nil {
		t.Fatal(err)
	}
	challenge, _ := ceremony.Challenge(f.now)
	input := f.assertion(t, ceremony, 0x05, 2, base64.RawURLEncoding.EncodeToString(challenge), f.party.rpID, f.party.origin)
	verified, err := f.verifier.VerifyLogin(context.Background(), f.user, ceremony, input, f.now)
	if err != nil {
		t.Fatal(err)
	}
	fido, _ := verified.FIDO2()
	updated, err := fido.Updated()
	if err != nil {
		t.Fatal(err)
	}
	f.user, err = NewUserSnapshot(f.binding.account, f.user.handle[:], f.user.name, f.user.display, []CredentialRecord{updated})
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range []uint32{2, 1} {
		input := f.assertion(t, ceremony, 0x05, count, base64.RawURLEncoding.EncodeToString(challenge), f.party.rpID, f.party.origin)
		if _, err := f.verifier.VerifyLogin(context.Background(), f.user, ceremony, input, f.now); KindOf(err) != ErrorInvalidProof {
			t.Fatal("clone warning was not refused")
		}
	}
	var verifier *Verifier
	if _, err := verifier.VerifyLogin(context.Background(), f.user, ceremony, input, f.now); err == nil {
		t.Fatal("typed nil verifier admitted")
	}
	if _, err := f.verifier.VerifyLogin(context.Background(), f.user, ceremony, nil, f.now); err == nil {
		t.Fatal("nil wire proof admitted")
	}
	if _, err := DecodeCredentialRecord(f.binding.account, updated.factor, f.party, updated.version, bytes.Repeat([]byte{'x'}, MaxCredentialRecordBytes+1)); err == nil {
		t.Fatal("overlong record admitted")
	}
	if _, err := NewRelyingParty(f.party.instance, "http://homeward.example.test"); err == nil {
		t.Fatal("non-TLS canonical URL admitted")
	}
	if _, err := NewRelyingParty(f.party.instance, "https://user@homeward.example.test"); err == nil {
		t.Fatal("URL userinfo admitted")
	}
	if _, err := NewUserSnapshot(f.binding.account, f.user.handle[:], f.user.name, f.user.display, []CredentialRecord{updated, updated}); err == nil {
		t.Fatal("duplicate credential admitted")
	}
}

func TestNHA142NestedUnexportedDiagnosticsAndGetterCopies(t *testing.T) {
	f := nha142NewFixture(t)
	record := f.enroll(t)
	ceremony, err := f.verifier.BeginLogin(context.Background(), f.binding, f.user, f.now)
	if err != nil {
		t.Fatal(err)
	}
	var raw [RecoveryCodeBytes]byte
	copy(raw[:], []byte("SECRET-CODE-0123")) // synthetic canary, never a deployed secret
	code, err := ParseRecoveryCode(recoveryEncoding.EncodeToString(raw[:]))
	if err != nil {
		t.Fatal(err)
	}
	batch, _ := ParseRecoveryBatchID("diagnostic-batch")
	digest, err := code.Digest(f.binding.account, batch)
	if err != nil {
		t.Fatal(err)
	}
	codeText, _ := code.Text()
	recordBytes, _ := record.Bytes()
	ceremonyBytes, _ := ceremony.Bytes()
	// The array/byte-list representations detect fallback traversal even when
	// the human-readable code is never present in the payload's representation.
	forbidden := []string{codeText, string(raw[:]), fmt.Sprint(raw), fmt.Sprintf("%#v", raw), fmt.Sprint(digest.raw), fmt.Sprintf("%#v", digest.raw), "nha142-factor",
		strings.TrimSuffix(fmt.Sprint(recordBytes[:12]), "]"), strings.TrimSuffix(fmt.Sprint(ceremonyBytes[:12]), "]")}
	values := []any{
		code, digest, record, ceremony, f.user,
		struct{ secret RecoveryCode }{code}, struct{ secret any }{code},
		struct{ secret RecoveryDigest }{digest}, struct{ secret any }{digest},
		struct{ secret CredentialRecord }{record}, struct{ secret any }{record},
		struct{ secret PreparedCeremony }{ceremony}, struct{ secret any }{ceremony},
		struct{ secret UserSnapshot }{f.user}, struct{ secret any }{f.user},
	}
	for _, value := range values {
		for _, verb := range []string{"%v", "%+v", "%#v"} {
			printed := fmt.Sprintf(verb, value)
			for _, canary := range forbidden {
				if strings.Contains(printed, canary) {
					t.Fatal("unexported diagnostic exposed a sensitive payload")
				}
			}
		}
	}
	returnedDigest, _ := digest.Bytes()
	admitted, err := AdmitRecoveryDigest(f.binding.account, batch, returnedDigest[:])
	if err != nil {
		t.Fatal(err)
	}
	returnedDigest[0] ^= 1
	again, _ := admitted.Bytes()
	if again != digest.raw {
		t.Fatal("digest admission retained caller storage")
	}
	recordBytes[0], ceremonyBytes[0] = '!', '!'
	if _, err := record.credential(); err != nil {
		t.Fatal("record getter leaked mutable storage")
	}
	if _, err := ceremony.session(f.now); err != nil {
		t.Fatal("ceremony getter leaked mutable storage")
	}
	userHandle := f.user.WebAuthnID()
	userHandle[0] ^= 1
	if bytes.Equal(userHandle, f.user.WebAuthnID()) {
		t.Fatal("user handle getter leaked mutable storage")
	}
	if _, err := (CredentialRecord{}).Bytes(); err == nil {
		t.Fatal("nil credential payload admitted")
	}
	if _, err := (PreparedCeremony{}).Bytes(); err == nil {
		t.Fatal("nil ceremony payload admitted")
	}
	if _, err := (RecoveryCandidate{}).ID(); err == nil {
		t.Fatal("nil candidate payload admitted")
	}
}

func TestNHA142VerifiedProofBindsExactCeremony(t *testing.T) {
	fixture := nha142NewFixture(t)
	fixture.enroll(t)
	first, err := fixture.verifier.BeginLogin(context.Background(), fixture.binding, fixture.user, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.verifier.BeginLogin(context.Background(), fixture.binding, fixture.user, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := first.Digest()
	replacement, _ := second.Digest()
	if expected == replacement {
		t.Fatal("fresh server ceremony reused state")
	}
	challenge, _ := first.Challenge(fixture.now)
	proof := fixture.assertion(t, first, 0x05, 1, base64.RawURLEncoding.EncodeToString(challenge), fixture.party.rpID, fixture.party.origin)
	verified, err := fixture.verifier.VerifyLogin(context.Background(), fixture.user, first, proof, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := verified.CeremonyDigest()
	if err != nil || got != expected || got == replacement {
		t.Fatal("FIDO2 evidence detached from server ceremony")
	}
	batch, candidates, codes, err := GenerateRecoveryBatch(fixture.binding.account)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := VerifyRecoveryCode(fixture.binding, first, batch, codes[0], candidates)
	if err != nil {
		t.Fatal(err)
	}
	got, err = recovery.CeremonyDigest()
	if err != nil || got != expected || got == replacement {
		t.Fatal("recovery evidence detached from server ceremony")
	}
	if _, err := VerifyRecoveryCode(fixture.binding, PreparedCeremony{}, batch, codes[0], candidates); err == nil {
		t.Fatal("recovery admitted without server ceremony")
	}
}

type nha142TypedNilContext struct{ context.Context }

func TestNHA142TypedNilContextsFailClosed(t *testing.T) {
	var missing *nha142TypedNilContext
	gate := NewCryptoGate()
	if _, err := gate.Acquire(missing); KindOf(err) != ErrorInvalidInput {
		t.Fatal("typed nil crypto context admitted")
	}
	if err := gate.Drain(missing); KindOf(err) != ErrorInvalidInput {
		t.Fatal("typed nil drain context admitted")
	}
	fixture := nha142NewFixture(t)
	if _, err := fixture.verifier.BeginEnrollment(missing, fixture.binding, fixture.user, fixture.now); KindOf(err) != ErrorInvalidInput {
		t.Fatal("typed nil verifier context admitted")
	}
}
