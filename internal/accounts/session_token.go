package accounts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/damione1/Homeward/contracts"
)

var ErrInvalidSessionToken = errors.New("accounts: invalid session credential")

// VerifyUnknownPassword spends the same bounded Argon2 profile as a known
// login without creating a real account or persisting an artificial verifier.
func VerifyUnknownPassword(ctx context.Context, raw string) error {
	verifier := Verifier{profile: passwordProfileArgon2idV1,
		salt: [saltSize]byte{0x17, 0x8a, 0xd9, 0x43, 0x72, 0x5c, 0xe1, 0x6b, 0x94, 0x2f, 0xc8, 0x51, 0x3d, 0xa0, 0x77, 0xb5},
		digest: [digestSize]byte{0x44, 0x0e, 0x89, 0x5b, 0x21, 0xca, 0xa4, 0x66, 0xfe, 0x98, 0x1b, 0x34, 0x79, 0xd2, 0x5f, 0xe0,
			0x13, 0x67, 0xbc, 0x0a, 0x56, 0x9f, 0x32, 0xcd, 0xe8, 0x74, 0x1d, 0x90, 0x5a, 0xb3, 0x6c, 0x28}}
	_, err := VerifyPassword(ctx, raw, verifier)
	return err
}

const sessionTokenBytes = 32
const sessionTokenTextBytes = 43

type AccessSecret struct {
	raw   [sessionTokenBytes]byte
	valid bool
}

type RefreshSecret struct {
	raw   [sessionTokenBytes]byte
	valid bool
}

type EnrollmentSecret struct {
	raw   [sessionTokenBytes]byte
	valid bool
}

// The three digest shapes deliberately differ. A refresh or enrollment
// digest cannot be passed to an access admission, even by conversion.
type accessDigestPurpose struct{}
type refreshDigestPurpose struct{}
type enrollmentDigestPurpose struct{}

type AccessDigest struct {
	raw     [32]byte
	valid   bool
	purpose [0]accessDigestPurpose
}
type RefreshDigest struct {
	raw     [32]byte
	valid   bool
	purpose [0]refreshDigestPurpose
}
type EnrollmentDigest struct {
	raw     [32]byte
	valid   bool
	purpose [0]enrollmentDigestPurpose
}

func (d AccessDigest) Bytes() ([32]byte, error) {
	if !d.valid || d.raw == [32]byte{} {
		return [32]byte{}, ErrInvalidSessionToken
	}
	return d.raw, nil
}
func (d RefreshDigest) Bytes() ([32]byte, error) {
	if !d.valid || d.raw == [32]byte{} {
		return [32]byte{}, ErrInvalidSessionToken
	}
	return d.raw, nil
}
func (d EnrollmentDigest) Bytes() ([32]byte, error) {
	if !d.valid || d.raw == [32]byte{} {
		return [32]byte{}, ErrInvalidSessionToken
	}
	return d.raw, nil
}

func (d AccessDigest) String() string                   { return "[REDACTED ACCESS DIGEST]" }
func (d AccessDigest) GoString() string                 { return d.String() }
func (d AccessDigest) MarshalJSON() ([]byte, error)     { return json.Marshal(d.String()) }
func (d RefreshDigest) String() string                  { return "[REDACTED REFRESH DIGEST]" }
func (d RefreshDigest) GoString() string                { return d.String() }
func (d RefreshDigest) MarshalJSON() ([]byte, error)    { return json.Marshal(d.String()) }
func (d EnrollmentDigest) String() string               { return "[REDACTED ENROLLMENT DIGEST]" }
func (d EnrollmentDigest) GoString() string             { return d.String() }
func (d EnrollmentDigest) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func newSessionRaw() ([sessionTokenBytes]byte, error) {
	var raw [sessionTokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil || raw == [sessionTokenBytes]byte{} {
		return raw, ErrInvalidSessionToken
	}
	return raw, nil
}

func parseSessionRaw(text string) ([sessionTokenBytes]byte, error) {
	var raw [sessionTokenBytes]byte
	if len(text) != sessionTokenTextBytes {
		return raw, ErrInvalidSessionToken
	}
	decoded, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil || len(decoded) != len(raw) || base64.RawURLEncoding.EncodeToString(decoded) != text {
		return raw, ErrInvalidSessionToken
	}
	copy(raw[:], decoded)
	if raw == [sessionTokenBytes]byte{} {
		return [sessionTokenBytes]byte{}, ErrInvalidSessionToken
	}
	return raw, nil
}

func sessionDigest(label string, raw [sessionTokenBytes]byte) ([32]byte, error) {
	if raw == [sessionTokenBytes]byte{} {
		return [32]byte{}, ErrInvalidSessionToken
	}
	buffer := make([]byte, 0, len(label)+len(raw))
	buffer = append(buffer, label...)
	buffer = append(buffer, raw[:]...)
	result := sha256.Sum256(buffer)
	clear(buffer)
	return result, nil
}

func NewAccessSecret() (AccessSecret, error) {
	raw, err := newSessionRaw()
	return AccessSecret{raw: raw, valid: err == nil}, err
}
func ParseAccessSecret(text string) (AccessSecret, error) {
	raw, err := parseSessionRaw(text)
	return AccessSecret{raw: raw, valid: err == nil}, err
}
func (s AccessSecret) Text() (string, error) {
	if !s.valid {
		return "", ErrInvalidSessionToken
	}
	return base64.RawURLEncoding.EncodeToString(s.raw[:]), nil
}
func (s AccessSecret) Digest() (AccessDigest, error) {
	if !s.valid {
		return AccessDigest{}, ErrInvalidSessionToken
	}
	raw, err := sessionDigest("homeward/access/v1", s.raw)
	return AccessDigest{raw: raw, valid: err == nil}, err
}
func (s AccessSecret) String() string               { return "[REDACTED ACCESS CREDENTIAL]" }
func (s AccessSecret) GoString() string             { return s.String() }
func (s AccessSecret) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

func NewRefreshSecret() (RefreshSecret, error) {
	raw, err := newSessionRaw()
	return RefreshSecret{raw: raw, valid: err == nil}, err
}
func ParseRefreshSecret(text string) (RefreshSecret, error) {
	raw, err := parseSessionRaw(text)
	return RefreshSecret{raw: raw, valid: err == nil}, err
}
func (s RefreshSecret) Text() (string, error) {
	if !s.valid {
		return "", ErrInvalidSessionToken
	}
	return base64.RawURLEncoding.EncodeToString(s.raw[:]), nil
}
func (s RefreshSecret) Digest() (RefreshDigest, error) {
	if !s.valid {
		return RefreshDigest{}, ErrInvalidSessionToken
	}
	raw, err := sessionDigest("homeward/refresh/v1", s.raw)
	return RefreshDigest{raw: raw, valid: err == nil}, err
}
func (s RefreshSecret) String() string               { return "[REDACTED REFRESH CREDENTIAL]" }
func (s RefreshSecret) GoString() string             { return s.String() }
func (s RefreshSecret) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

func NewEnrollmentSecret() (EnrollmentSecret, error) {
	raw, err := newSessionRaw()
	return EnrollmentSecret{raw: raw, valid: err == nil}, err
}
func ParseEnrollmentSecret(text string) (EnrollmentSecret, error) {
	raw, err := parseSessionRaw(text)
	return EnrollmentSecret{raw: raw, valid: err == nil}, err
}
func (s EnrollmentSecret) Text() (string, error) {
	if !s.valid {
		return "", ErrInvalidSessionToken
	}
	return base64.RawURLEncoding.EncodeToString(s.raw[:]), nil
}
func (s EnrollmentSecret) Digest() (EnrollmentDigest, error) {
	if !s.valid {
		return EnrollmentDigest{}, ErrInvalidSessionToken
	}
	raw, err := sessionDigest("homeward/enrollment/v1", s.raw)
	return EnrollmentDigest{raw: raw, valid: err == nil}, err
}
func (s EnrollmentSecret) String() string               { return "[REDACTED ENROLLMENT CREDENTIAL]" }
func (s EnrollmentSecret) GoString() string             { return s.String() }
func (s EnrollmentSecret) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

func NewSessionID() (contracts.SessionID, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil || raw == [16]byte{} {
		return contracts.SessionID{}, ErrInvalidSessionToken
	}
	return contracts.ParseSessionID(hex.EncodeToString(raw[:]))
}

func NewChallengeID() (contracts.ChallengeID, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil || raw == [16]byte{} {
		return contracts.ChallengeID{}, ErrInvalidSessionToken
	}
	return contracts.ParseChallengeID(hex.EncodeToString(raw[:]))
}
