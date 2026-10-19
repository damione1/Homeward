// Package authmfa admits the bounded, detached values used by local MFA.
// Persistence, session issuance and transport authorization belong to their
// existing authorities; a decoded record does not authenticate a caller.
package authmfa

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"math"
	"reflect"
	"time"
	"unicode/utf8"

	"github.com/damione1/Homeward/contracts"
)

type ErrorKind uint8

const (
	ErrorUnspecified ErrorKind = iota
	ErrorInvalidInput
	ErrorInvalidProof
	ErrorExpired
	ErrorConsumed
	ErrorConflict
	ErrorRateLimited
	ErrorBusy
	ErrorIntegrity
	ErrorUnavailable
	ErrorCommitUnknown
)

// Error deliberately retains neither provider errors nor untrusted payloads.
type Error struct{ kind ErrorKind }

func (e *Error) Kind() ErrorKind {
	if e == nil {
		return ErrorUnspecified
	}
	return e.kind
}

func (e *Error) Error() string {
	if e == nil {
		return "mfa: unavailable"
	}
	switch e.kind {
	case ErrorInvalidInput:
		return "mfa: invalid input"
	case ErrorInvalidProof:
		return "mfa: authentication failed"
	case ErrorExpired:
		return "mfa: ceremony expired"
	case ErrorConsumed:
		return "mfa: proof unavailable"
	case ErrorConflict:
		return "mfa: version conflict"
	case ErrorRateLimited:
		return "mfa: temporarily limited"
	case ErrorBusy:
		return "mfa: capacity exhausted"
	case ErrorIntegrity:
		return "mfa: record integrity failure"
	case ErrorUnavailable:
		return "mfa: unavailable"
	case ErrorCommitUnknown:
		return "mfa: commit outcome unknown"
	default:
		return "mfa: unavailable"
	}
}

func errorOf(kind ErrorKind) error { return &Error{kind: kind} }

func KindOf(err error) ErrorKind {
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Kind()
	}
	return ErrorUnspecified
}

type RecordVersion struct{ revision int64 }

func NewRecordVersion(number uint64) (RecordVersion, error) {
	if number == 0 || number > math.MaxInt64 {
		return RecordVersion{}, errorOf(ErrorInvalidInput)
	}
	return RecordVersion{revision: int64(number)}, nil
}

func (v RecordVersion) Number() (uint64, error) {
	if v.revision <= 0 {
		return 0, errorOf(ErrorInvalidInput)
	}
	return uint64(v.revision), nil
}

func (v RecordVersion) Next() (RecordVersion, error) {
	if v.revision <= 0 || v.revision == math.MaxInt64 {
		return RecordVersion{}, errorOf(ErrorConflict)
	}
	return RecordVersion{revision: v.revision + 1}, nil
}

type CeremonyKind uint8

const (
	CeremonyUnspecified CeremonyKind = iota
	CeremonyLogin
	CeremonyEnrollment
)

func validCeremonyKind(kind CeremonyKind) bool {
	switch kind {
	case CeremonyLogin, CeremonyEnrollment:
		return true
	default:
		return false
	}
}

// Binding is a checked identity tuple, not an authorization or a DB revision.
// The store must recheck its own RevocationRevision and these identities in the
// final transaction. All component types are distinct and zero-invalid.
type Binding struct {
	account   contracts.AccountID
	challenge contracts.ChallengeID
	instance  contracts.InstanceID
}

func NewBinding(account contracts.AccountID, challenge contracts.ChallengeID, instance contracts.InstanceID) (Binding, error) {
	a, ae := account.Value()
	c, ce := challenge.Value()
	i, ie := instance.Value()
	if ae != nil || ce != nil || ie != nil || !validID(a) || !validID(c) || !validID(i) {
		return Binding{}, errorOf(ErrorInvalidInput)
	}
	return Binding{account: account, challenge: challenge, instance: instance}, nil
}

func (b Binding) valid() bool {
	_, err := NewBinding(b.account, b.challenge, b.instance)
	return err == nil
}

func (b Binding) Account() (contracts.AccountID, error) {
	if !b.valid() {
		return contracts.AccountID{}, errorOf(ErrorInvalidInput)
	}
	return b.account, nil
}

func (b Binding) Challenge() (contracts.ChallengeID, error) {
	if !b.valid() {
		return contracts.ChallengeID{}, errorOf(ErrorInvalidInput)
	}
	return b.challenge, nil
}

func (b Binding) Instance() (contracts.InstanceID, error) {
	if !b.valid() {
		return contracts.InstanceID{}, errorOf(ErrorInvalidInput)
	}
	return b.instance, nil
}

func validID(text string) bool {
	if text == "" || len(text) > 128 || !utf8.ValidString(text) {
		return false
	}
	for _, r := range text {
		if r == 0 || r == '\r' || r == '\n' {
			return false
		}
	}
	return true
}

// These variants are verification results, not a session-satisfaction enum.
// The latter remains owned by the future auth Protobuf addition. No wire or
// SQL satisfaction conversion is implemented in this isolated phase.
type verifiedVariant uint8

const (
	verifiedUnspecified verifiedVariant = iota
	verifiedFIDO2
	verifiedRecovery
)

type VerifiedLogin struct {
	variant        verifiedVariant
	binding        Binding
	ceremonyDigest [32]byte
	fido           *VerifiedFIDO2
	recovery       *VerifiedRecovery
}

type VerifiedFIDO2 struct {
	expected RecordVersion
	updated  CredentialRecord
}

type VerifiedRecovery struct {
	batch  RecoveryBatchID
	code   RecoveryCodeID
	digest RecoveryDigest
}

func (v VerifiedLogin) Binding() (Binding, error) {
	if !v.valid() {
		return Binding{}, errorOf(ErrorInvalidProof)
	}
	return v.binding, nil
}

func (v VerifiedLogin) valid() bool {
	if !v.binding.valid() || v.ceremonyDigest == [32]byte{} {
		return false
	}
	switch v.variant {
	case verifiedFIDO2:
		return v.fido != nil && v.recovery == nil && v.fido.expected.revision > 0 && v.fido.expected.revision < math.MaxInt64 && v.fido.updated.valid() && v.fido.updated.account == v.binding.account && v.fido.updated.party.instance == v.binding.instance && v.fido.updated.version.revision == v.fido.expected.revision+1
	case verifiedRecovery:
		return v.recovery != nil && v.fido == nil && v.recovery.batch.valid() && v.recovery.code.valid() && v.recovery.digest.validFor(v.binding.account, v.recovery.batch)
	default:
		return false
	}
}

func (v VerifiedLogin) FIDO2() (VerifiedFIDO2, bool) {
	if !v.valid() || v.variant != verifiedFIDO2 {
		return VerifiedFIDO2{}, false
	}
	return VerifiedFIDO2{expected: v.fido.expected, updated: v.fido.updated.clone()}, true
}

func (v VerifiedLogin) Recovery() (VerifiedRecovery, bool) {
	if !v.valid() || v.variant != verifiedRecovery {
		return VerifiedRecovery{}, false
	}
	return *v.recovery, true
}

func (v VerifiedFIDO2) Expected() (RecordVersion, error) {
	if v.expected.revision <= 0 || !v.updated.valid() {
		return RecordVersion{}, errorOf(ErrorInvalidProof)
	}
	return v.expected, nil
}

func (v VerifiedFIDO2) Updated() (CredentialRecord, error) {
	if v.expected.revision <= 0 || !v.updated.valid() {
		return CredentialRecord{}, errorOf(ErrorInvalidProof)
	}
	return v.updated.clone(), nil
}

func (v VerifiedRecovery) Batch() (RecoveryBatchID, error) {
	if !v.batch.valid() || !v.code.valid() {
		return RecoveryBatchID{}, errorOf(ErrorInvalidProof)
	}
	return v.batch, nil
}

func (v VerifiedRecovery) Code() (RecoveryCodeID, error) {
	if !v.batch.valid() || !v.code.valid() {
		return RecoveryCodeID{}, errorOf(ErrorInvalidProof)
	}
	return v.code, nil
}

func (v VerifiedRecovery) Digest() (RecoveryDigest, error) {
	if v.digest.recoveryDigestValue == nil || !v.digest.validFor(v.digest.account, v.batch) || !v.code.valid() {
		return RecoveryDigest{}, errorOf(ErrorInvalidProof)
	}
	return v.digest, nil
}

func (VerifiedLogin) String() string                 { return "[REDACTED MFA PROOF]" }
func (v VerifiedLogin) GoString() string             { return v.String() }
func (v VerifiedLogin) MarshalJSON() ([]byte, error) { return json.Marshal(v.String()) }

func decodeOneJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errorOf(ErrorIntegrity)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errorOf(ErrorIntegrity)
	}
	return nil
}

// IsRecentAuthentication is purely temporal. The caller must separately admit
// the authoritative session-satisfaction variant and current account state.
// Future times, zero values and equality at five minutes are never recent.
func IsRecentAuthentication(authenticatedAt, now time.Time) bool {
	return !authenticatedAt.IsZero() && !now.IsZero() && !now.Before(authenticatedAt) && now.Sub(authenticatedAt) < RecentAuthenticationLifetime
}

// PolicySource is the only admitted MFA policy source; source bytes are exact.
const PolicySchema = "homeward.mfa-policy/1"
const PolicySyntaxProfile = "homeward.mfa-policy-yaml/1"

type PolicySource struct {
	admitted bool
	required bool
}

func NewPolicySource(required bool) PolicySource {
	return PolicySource{admitted: true, required: required}
}
func AdmitPolicySource(raw []byte) (PolicySource, error) {
	for _, required := range []bool{false, true} {
		source := NewPolicySource(required)
		data, _ := source.Bytes()
		if bytes.Equal(raw, data) {
			return source, nil
		}
	}
	return PolicySource{}, errorOf(ErrorIntegrity)
}
func (s PolicySource) Required() (bool, error) {
	if !s.admitted {
		return false, errorOf(ErrorInvalidInput)
	}
	return s.required, nil
}
func (s PolicySource) Bytes() ([]byte, error) {
	if !s.admitted {
		return nil, errorOf(ErrorInvalidInput)
	}
	value := "false"
	if s.required {
		value = "true"
	}
	return []byte("schema: " + PolicySchema + "\nrequire_admin_factor: " + value + "\n"), nil
}
func (s PolicySource) SHA256() ([32]byte, error) {
	raw, err := s.Bytes()
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}

// ExpectedPolicyVersion names one exact committed revision and source hash.
type ExpectedPolicyVersion struct {
	revision int64
	hash     [32]byte
}

func NewExpectedPolicyVersion(revision uint64, hash []byte) (ExpectedPolicyVersion, error) {
	if revision == 0 || revision > math.MaxInt64 || len(hash) != sha256.Size {
		return ExpectedPolicyVersion{}, errorOf(ErrorInvalidInput)
	}
	v := ExpectedPolicyVersion{revision: int64(revision)}
	copy(v.hash[:], hash)
	return v, nil
}
func (v ExpectedPolicyVersion) Values() (uint64, [32]byte, error) {
	if v.revision <= 0 {
		return 0, [32]byte{}, errorOf(ErrorInvalidInput)
	}
	return uint64(v.revision), v.hash, nil
}

// NewError exposes only the closed, redacted error vocabulary to persistence.
func NewError(kind ErrorKind) error {
	switch kind {
	case ErrorInvalidInput, ErrorInvalidProof, ErrorExpired, ErrorConsumed, ErrorConflict, ErrorRateLimited, ErrorBusy, ErrorIntegrity, ErrorUnavailable, ErrorCommitUnknown:
		return errorOf(kind)
	default:
		return errorOf(ErrorUnavailable)
	}
}

func (v VerifiedLogin) CeremonyDigest() ([32]byte, error) {
	if !v.valid() {
		return [32]byte{}, errorOf(ErrorInvalidProof)
	}
	return v.ceremonyDigest, nil
}

func validContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	value := reflect.ValueOf(ctx)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !value.IsNil()
	default:
		return true
	}
}
