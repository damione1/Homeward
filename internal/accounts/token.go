package accounts

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"

	"github.com/damione1/Homeward/contracts"
)

var ErrInvalidInvitationToken = errors.New("accounts: invalid invitation token")

// InvitationToken is emitted once. Only Digest crosses the repository boundary.
type InvitationToken struct {
	text   string
	digest [32]byte
}

func NewInvitationToken() (InvitationToken, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return InvitationToken{}, ErrInvalidInvitationToken
	}
	return InvitationToken{text: base64.RawURLEncoding.EncodeToString(raw[:]), digest: sha256.Sum256(raw[:])}, nil
}

func ParseInvitationToken(raw string) (InvitationToken, error) {
	if len(raw) != 43 {
		return InvitationToken{}, ErrInvalidInvitationToken
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != raw {
		return InvitationToken{}, ErrInvalidInvitationToken
	}
	return InvitationToken{text: raw, digest: sha256.Sum256(decoded)}, nil
}

func (t InvitationToken) Text() (string, error) {
	if t.text == "" {
		return "", ErrInvalidInvitationToken
	}
	return t.text, nil
}

func (t InvitationToken) Digest() ([32]byte, error) {
	if t.text == "" {
		return [32]byte{}, ErrInvalidInvitationToken
	}
	return t.digest, nil
}

func (t InvitationToken) String() string   { return "[REDACTED INVITATION TOKEN]" }
func (t InvitationToken) GoString() string { return t.String() }

func NewAccountID() (contracts.AccountID, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return contracts.AccountID{}, err
	}
	return contracts.ParseAccountID(hex.EncodeToString(raw[:]))
}

func NewInvitationID() (contracts.InvitationID, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return contracts.InvitationID{}, err
	}
	return contracts.ParseInvitationID(hex.EncodeToString(raw[:]))
}
