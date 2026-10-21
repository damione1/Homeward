package historyquery

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"time"
)

// Binding is the digest of the existing authorization binding and catalogue
// capture. It conveys no permission on its own.
type Binding struct{ digest [32]byte }

func NewBinding(policy, catalogue [32]byte) (Binding, error) {
	if policy == ([32]byte{}) || catalogue == ([32]byte{}) {
		return Binding{}, ErrInvalid
	}
	h := sha256.New()
	h.Write([]byte("homeward-history-binding-v1"))
	h.Write(policy[:])
	h.Write(catalogue[:])
	var b Binding
	copy(b.digest[:], h.Sum(nil))
	return b, nil
}
func (b Binding) Equal(other Binding) bool { return b.digest != ([32]byte{}) && b == other }

type CursorCodec struct {
	aead    cipher.AEAD
	elapsed func() time.Duration
}

func (c CursorCodec) String() string               { return "history cursor codec" }
func (c CursorCodec) GoString() string             { return c.String() }
func (c CursorCodec) Format(s fmt.State, _ rune)   { _, _ = s.Write([]byte(c.String())) }
func (c CursorCodec) MarshalJSON() ([]byte, error) { return []byte(`"history cursor codec"`), nil }
func NewCursorCodec() (*CursorCodec, error) {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, ErrUnavailable
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, ErrUnavailable
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrUnavailable
	}
	origin := time.Now()
	return &CursorCodec{aead: aead, elapsed: func() time.Duration { return time.Since(origin) }}, nil
}
func (c *CursorCodec) aad(procedure string, q Query, b Binding) ([]byte, error) {
	if c == nil || c.aead == nil || c.elapsed == nil || procedure == "" || b.digest == ([32]byte{}) {
		return nil, ErrStale
	}
	digest, err := q.Digest()
	if err != nil {
		return nil, ErrStale
	}
	aad := append([]byte("homeward-history-cursor-v1/"+procedure), digest[:]...)
	return append(aad, b.digest[:]...), nil
}
func (c *CursorCodec) Seal(procedure string, q Query, b Binding, state []byte) (string, error) {
	aad, err := c.aad(procedure, q, b)
	if err != nil || len(state) == 0 || len(state) > 256 {
		return "", ErrStale
	}
	now := c.elapsed()
	if now < 0 || now > time.Duration(math.MaxInt64)-CursorTTL {
		return "", ErrStale
	}
	payload := make([]byte, 8+len(state))
	binary.BigEndian.PutUint64(payload, uint64(now+CursorTTL))
	copy(payload[8:], state)
	nonce := make([]byte, c.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", ErrUnavailable
	}
	sealed := c.aead.Seal(nonce, nonce, payload, aad)
	if base64.RawURLEncoding.EncodedLen(len(sealed)) > MaxTokenBytes {
		return "", ErrStale
	}
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}
func (c *CursorCodec) Open(procedure string, q Query, b Binding, token string) ([]byte, error) {
	if len(token) == 0 || len(token) > MaxTokenBytes {
		return nil, ErrStale
	}
	aad, err := c.aad(procedure, q, b)
	if err != nil {
		return nil, ErrStale
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) < c.aead.NonceSize()+c.aead.Overhead()+9 || len(raw) > c.aead.NonceSize()+c.aead.Overhead()+264 {
		return nil, ErrStale
	}
	plain, err := c.aead.Open(nil, raw[:c.aead.NonceSize()], raw[c.aead.NonceSize():], aad)
	if err != nil {
		return nil, ErrStale
	}
	now := c.elapsed()
	expiry := binary.BigEndian.Uint64(plain[:8])
	if now < 0 || expiry > math.MaxInt64 || uint64(now) >= expiry || expiry-uint64(now) > uint64(CursorTTL) {
		return nil, ErrStale
	}
	return plain[8:], nil
}
