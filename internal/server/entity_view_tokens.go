package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/damione1/Homeward/internal/view"
)

// Tokens carry only bounded continuations into native captures. Each caller
// must obtain current authority and its exact capture before opening a token.
type entityViewToken struct {
	Page      string   `json:"p,omitempty"`
	Resume    []byte   `json:"r,omitempty"`
	Snapshots []string `json:"s,omitempty"`
	Names     []string `json:"n,omitempty"`
	Offset    int      `json:"o,omitempty"`
	Expires   int64    `json:"e"`
}

func (h *homeResourceHandler) sealEntityViewToken(value entityViewToken, binding []byte) (string, error) {
	plain, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, h.pages.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(h.pages.Seal(nonce, nonce, plain, binding))
	if len(token) > 8192 {
		return "", view.ErrResourceExhausted
	}
	return token, nil
}
func (h *homeResourceHandler) openEntityViewToken(token string, binding []byte) (entityViewToken, error) {
	var value entityViewToken
	if token == "" || len(token) > 8192 {
		return value, view.ErrPageToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) < h.pages.NonceSize()+h.pages.Overhead() {
		return value, view.ErrPageToken
	}
	plain, err := h.pages.Open(nil, raw[:h.pages.NonceSize()], raw[h.pages.NonceSize():], binding)
	if err != nil || json.Unmarshal(plain, &value) != nil || value.Expires <= time.Now().UnixNano() || len(value.Names) > view.MaxSnapshots || len(value.Snapshots) > view.MaxSnapshots || len(value.Resume) > 2048 || len(value.Page) > 2048 {
		return entityViewToken{}, view.ErrPageToken
	}
	return value, nil
}
