// Package mcpkeys owns MCP-only credentials. It never issues interactive tokens.
package mcpkeys

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/damione1/Homeward/contracts"
)

var ErrInvalid = errors.New("mcp keys: invalid input")

const Prefix = "hwmcp1_"

type Secret struct {
	raw   [32]byte
	valid bool
}
type digestPurpose struct{}
type Digest struct {
	raw     [32]byte
	valid   bool
	purpose [0]digestPurpose
}
type ClientName struct{ value string }

func ParseClientName(text string) (ClientName, error) {
	if text == "" || len(text) > 128 || !utf8.ValidString(text) || strings.TrimSpace(text) != text || strings.ContainsAny(text, "\x00\r\n") {
		return ClientName{}, ErrInvalid
	}
	return ClientName{text}, nil
}
func (n ClientName) Value() (string, error) { v, e := ParseClientName(n.value); return v.value, e }
func NewSecret() (Secret, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil || raw == [32]byte{} {
		return Secret{}, ErrInvalid
	}
	return Secret{raw, true}, nil
}
func ParseSecret(text string) (Secret, error) {
	if !strings.HasPrefix(text, Prefix) || len(text) != len(Prefix)+43 {
		return Secret{}, ErrInvalid
	}
	value, err := base64.RawURLEncoding.DecodeString(text[len(Prefix):])
	if err != nil || len(value) != 32 || base64.RawURLEncoding.EncodeToString(value) != text[len(Prefix):] {
		return Secret{}, ErrInvalid
	}
	var raw [32]byte
	copy(raw[:], value)
	clear(value)
	if raw == [32]byte{} {
		return Secret{}, ErrInvalid
	}
	return Secret{raw, true}, nil
}
func (s Secret) Text() (string, error) {
	if !s.valid {
		return "", ErrInvalid
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(s.raw[:]), nil
}
func (s Secret) Digest() (Digest, error) {
	if !s.valid {
		return Digest{}, ErrInvalid
	}
	buffer := append([]byte("homeward/mcp-key/v1\x00"), s.raw[:]...)
	raw := sha256.Sum256(buffer)
	clear(buffer)
	return Digest{raw: raw, valid: true}, nil
}
func (d Digest) Bytes() ([32]byte, error) {
	if !d.valid || d.raw == [32]byte{} {
		return [32]byte{}, ErrInvalid
	}
	return d.raw, nil
}
func (Secret) String() string                 { return "[REDACTED MCP CREDENTIAL]" }
func (s Secret) GoString() string             { return s.String() }
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }
func (Digest) String() string                 { return "[REDACTED MCP DIGEST]" }
func (d Digest) GoString() string             { return d.String() }
func (d Digest) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }
func NewID() (contracts.McpKeyID, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil || raw == [16]byte{} {
		return contracts.McpKeyID{}, ErrInvalid
	}
	return contracts.ParseMcpKeyID(hex.EncodeToString(raw[:]))
}
