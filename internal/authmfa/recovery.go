package authmfa

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"strings"

	"github.com/damione1/Homeward/contracts"
)

type RecoveryBatchID struct{ batch string }
type RecoveryCodeID struct{ code string }

func ParseRecoveryBatchID(text string) (RecoveryBatchID, error) {
	if !validID(text) {
		return RecoveryBatchID{}, errorOf(ErrorInvalidInput)
	}
	return RecoveryBatchID{batch: text}, nil
}

func ParseRecoveryCodeID(text string) (RecoveryCodeID, error) {
	if !validID(text) {
		return RecoveryCodeID{}, errorOf(ErrorInvalidInput)
	}
	return RecoveryCodeID{code: text}, nil
}

func (v RecoveryBatchID) valid() bool { return validID(v.batch) }
func (v RecoveryCodeID) valid() bool  { return validID(v.code) }

func (v RecoveryBatchID) Value() (string, error) {
	if !v.valid() {
		return "", errorOf(ErrorInvalidInput)
	}
	return v.batch, nil
}

func (v RecoveryCodeID) Value() (string, error) {
	if !v.valid() {
		return "", errorOf(ErrorInvalidInput)
	}
	return v.code, nil
}

func randomID() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil || raw == [32]byte{} {
		return "", errorOf(ErrorUnavailable)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// Keep sensitive storage behind an unexported immutable pointer. fmt ignores
// String/GoString on an unexported enclosing field; a direct raw array would
// then be printed recursively. Explicit getters alone reveal admitted copies.
type RecoveryCode struct{ *recoveryCodeValue }
type recoveryCodeValue struct{ raw [RecoveryCodeBytes]byte }

// RecoveryDigest is bound to both account and batch. A raw session/enrollment
// digest has a different shape and cannot be converted into this type.
type RecoveryDigest struct{ *recoveryDigestValue }
type recoveryDigestValue struct {
	account contracts.AccountID
	batch   RecoveryBatchID
	raw     [sha256.Size]byte
}

var recoveryEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func NewRecoveryCode() (RecoveryCode, error) {
	var raw [RecoveryCodeBytes]byte
	if _, err := rand.Read(raw[:]); err != nil || raw == [RecoveryCodeBytes]byte{} {
		return RecoveryCode{}, errorOf(ErrorUnavailable)
	}
	return RecoveryCode{recoveryCodeValue: &recoveryCodeValue{raw: raw}}, nil
}

func ParseRecoveryCode(text string) (RecoveryCode, error) {
	// Exactly 26 base32 characters or 5-5-5-5-6 groups. Unicode whitespace,
	// lookalikes, misplaced separators and noncanonical unused bits are refused.
	if len(text) != 26 && len(text) != 30 {
		return RecoveryCode{}, errorOf(ErrorInvalidInput)
	}
	var canonical strings.Builder
	canonical.Grow(26)
	for index := 0; index < len(text); index++ {
		value := text[index]
		if len(text) == 30 && (index == 5 || index == 11 || index == 17 || index == 23) {
			if value != '-' {
				return RecoveryCode{}, errorOf(ErrorInvalidInput)
			}
			continue
		}
		if value >= 'a' && value <= 'z' {
			value -= 'a' - 'A'
		}
		if !(value >= 'A' && value <= 'Z' || value >= '2' && value <= '7') {
			return RecoveryCode{}, errorOf(ErrorInvalidInput)
		}
		canonical.WriteByte(value)
	}
	plain := canonical.String()
	raw, err := recoveryEncoding.DecodeString(plain)
	if err != nil || len(raw) != RecoveryCodeBytes || recoveryEncoding.EncodeToString(raw) != plain {
		return RecoveryCode{}, errorOf(ErrorInvalidInput)
	}
	var decoded [RecoveryCodeBytes]byte
	copy(decoded[:], raw)
	if decoded == [RecoveryCodeBytes]byte{} {
		return RecoveryCode{}, errorOf(ErrorInvalidInput)
	}
	return RecoveryCode{recoveryCodeValue: &recoveryCodeValue{raw: decoded}}, nil
}

// Text is the deliberate once-only response path. Diagnostic paths redact.
func (v RecoveryCode) Text() (string, error) {
	if v.recoveryCodeValue == nil || v.raw == [RecoveryCodeBytes]byte{} {
		return "", errorOf(ErrorInvalidInput)
	}
	plain := recoveryEncoding.EncodeToString(v.raw[:])
	return plain[:5] + "-" + plain[5:10] + "-" + plain[10:15] + "-" + plain[15:20] + "-" + plain[20:], nil
}

func (RecoveryCode) String() string                   { return "[REDACTED RECOVERY CODE]" }
func (v RecoveryCode) GoString() string               { return v.String() }
func (v RecoveryCode) MarshalJSON() ([]byte, error)   { return json.Marshal(v.String()) }
func (RecoveryDigest) String() string                 { return "[REDACTED RECOVERY DIGEST]" }
func (v RecoveryDigest) GoString() string             { return v.String() }
func (v RecoveryDigest) MarshalJSON() ([]byte, error) { return json.Marshal(v.String()) }

func (v RecoveryCode) Digest(account contracts.AccountID, batch RecoveryBatchID) (RecoveryDigest, error) {
	accountText, err := account.Value()
	if err != nil || !validID(accountText) || !batch.valid() || v.recoveryCodeValue == nil || v.raw == [RecoveryCodeBytes]byte{} {
		return RecoveryDigest{}, errorOf(ErrorInvalidInput)
	}
	hash := sha256.New()
	for _, part := range [][]byte{[]byte("homeward/mfa-recovery-code/v1"), []byte(accountText), []byte(batch.batch), v.raw[:]} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(part)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write(part)
	}
	var raw [sha256.Size]byte
	copy(raw[:], hash.Sum(nil))
	return RecoveryDigest{recoveryDigestValue: &recoveryDigestValue{account: account, batch: batch, raw: raw}}, nil
}

// AdmitRecoveryDigest is only shape admission of a row read by trusted storage.
// It does not prove that the corresponding code is available or unconsumed.
func AdmitRecoveryDigest(account contracts.AccountID, batch RecoveryBatchID, raw []byte) (RecoveryDigest, error) {
	text, err := account.Value()
	if err != nil || !validID(text) || !batch.valid() || len(raw) != sha256.Size {
		return RecoveryDigest{}, errorOf(ErrorIntegrity)
	}
	result := RecoveryDigest{recoveryDigestValue: &recoveryDigestValue{account: account, batch: batch}}
	copy(result.raw[:], raw)
	if !result.validFor(account, batch) {
		return RecoveryDigest{}, errorOf(ErrorIntegrity)
	}
	return result, nil
}

func (v RecoveryDigest) validFor(account contracts.AccountID, batch RecoveryBatchID) bool {
	text, err := account.Value()
	return err == nil && validID(text) && batch.valid() && v.recoveryDigestValue != nil && v.account == account && v.batch == batch && v.raw != [sha256.Size]byte{}
}

func (v RecoveryDigest) Bytes() ([sha256.Size]byte, error) {
	if v.recoveryDigestValue == nil || !v.validFor(v.account, v.batch) {
		return [sha256.Size]byte{}, errorOf(ErrorInvalidInput)
	}
	return v.raw, nil
}

type RecoveryCandidate struct{ *recoveryCandidateValue }
type recoveryCandidateValue struct {
	id     RecoveryCodeID
	digest RecoveryDigest
}

func NewRecoveryCandidate(id RecoveryCodeID, digest RecoveryDigest) (RecoveryCandidate, error) {
	if !id.valid() || digest.recoveryDigestValue == nil || !digest.validFor(digest.account, digest.batch) {
		return RecoveryCandidate{}, errorOf(ErrorIntegrity)
	}
	return RecoveryCandidate{recoveryCandidateValue: &recoveryCandidateValue{id: id, digest: digest}}, nil
}

func (c RecoveryCandidate) ID() (RecoveryCodeID, error) {
	if c.recoveryCandidateValue == nil || !c.id.valid() || c.digest.recoveryDigestValue == nil || !c.digest.validFor(c.digest.account, c.digest.batch) {
		return RecoveryCodeID{}, errorOf(ErrorIntegrity)
	}
	return c.id, nil
}

func (c RecoveryCandidate) Digest() (RecoveryDigest, error) {
	if _, err := c.ID(); err != nil {
		return RecoveryDigest{}, err
	}
	return c.digest, nil
}

// GenerateRecoveryBatch returns raw codes separately from persistable candidates.
// The caller must discard raw codes on every rollback/unknown outcome; there is
// no reversible representation in a RecoveryCandidate.
func GenerateRecoveryBatch(account contracts.AccountID) (RecoveryBatchID, []RecoveryCandidate, []RecoveryCode, error) {
	text, err := account.Value()
	if err != nil || !validID(text) {
		return RecoveryBatchID{}, nil, nil, errorOf(ErrorInvalidInput)
	}
	batchText, err := randomID()
	if err != nil {
		return RecoveryBatchID{}, nil, nil, err
	}
	batch := RecoveryBatchID{batch: batchText}
	candidates := make([]RecoveryCandidate, 0, RecoveryCodeCount)
	codes := make([]RecoveryCode, 0, RecoveryCodeCount)
	seenID := make(map[string]struct{}, RecoveryCodeCount)
	seenDigest := make(map[[sha256.Size]byte]struct{}, RecoveryCodeCount)
	for index := 0; index < RecoveryCodeCount; index++ {
		code, err := NewRecoveryCode()
		if err != nil {
			return RecoveryBatchID{}, nil, nil, err
		}
		id, err := randomID()
		if err != nil {
			return RecoveryBatchID{}, nil, nil, err
		}
		digest, err := code.Digest(account, batch)
		if err != nil {
			return RecoveryBatchID{}, nil, nil, err
		}
		if _, duplicate := seenID[id]; duplicate {
			return RecoveryBatchID{}, nil, nil, errorOf(ErrorUnavailable)
		}
		if _, duplicate := seenDigest[digest.raw]; duplicate {
			return RecoveryBatchID{}, nil, nil, errorOf(ErrorUnavailable)
		}
		seenID[id], seenDigest[digest.raw] = struct{}{}, struct{}{}
		candidates = append(candidates, RecoveryCandidate{recoveryCandidateValue: &recoveryCandidateValue{id: RecoveryCodeID{code: id}, digest: digest}})
		codes = append(codes, code)
	}
	return batch, candidates, codes, nil
}

// VerifyRecoveryCode does no writes. The store must consume this exact active
// batch/code atomically with challenge, session and audit after revalidation.
// A candidate loaded before a reset/replacement cannot bypass that final CAS.
func VerifyRecoveryCode(binding Binding, ceremony PreparedCeremony, batch RecoveryBatchID, code RecoveryCode, candidates []RecoveryCandidate) (VerifiedLogin, error) {
	if ceremony.preparedCeremonyValue == nil || ceremony.kind != CeremonyLogin || ceremony.binding != binding || !binding.valid() || !batch.valid() || len(candidates) == 0 || len(candidates) > RecoveryCodeCount {
		return VerifiedLogin{}, errorOf(ErrorInvalidProof)
	}
	wanted, err := code.Digest(binding.account, batch)
	if err != nil {
		return VerifiedLogin{}, errorOf(ErrorInvalidProof)
	}
	match := -1
	seen := make(map[string]struct{}, len(candidates))
	for index, candidate := range candidates {
		if candidate.recoveryCandidateValue == nil || !candidate.id.valid() || !candidate.digest.validFor(binding.account, batch) {
			return VerifiedLogin{}, errorOf(ErrorIntegrity)
		}
		if _, duplicate := seen[candidate.id.code]; duplicate {
			return VerifiedLogin{}, errorOf(ErrorIntegrity)
		}
		seen[candidate.id.code] = struct{}{}
		if subtle.ConstantTimeCompare(wanted.raw[:], candidate.digest.raw[:]) == 1 {
			if match != -1 {
				return VerifiedLogin{}, errorOf(ErrorIntegrity)
			}
			match = index
		}
	}
	if match < 0 {
		return VerifiedLogin{}, errorOf(ErrorInvalidProof)
	}
	selected := candidates[match]
	return VerifiedLogin{variant: verifiedRecovery, binding: binding, ceremonyDigest: sha256.Sum256(ceremony.encoded), recovery: &VerifiedRecovery{batch: batch, code: selected.id, digest: selected.digest}}, nil
}
func (v RecoveryDigest) MatchesBinding(account contracts.AccountID, batch RecoveryBatchID) bool {
	return v.validFor(account, batch)
}
