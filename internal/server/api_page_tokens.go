package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
)

// Page payloads remain private. Callers bind exact current authority, parent and
// RPC purpose with homePageBinding; page size is intentionally not a binding.
func newAPIPageCipher() (cipher.AEAD, error) {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func sealAPIPage(pages cipher.AEAD, plain, binding []byte) (string, error) {
	if pages == nil || len(plain) == 0 || len(plain) > 1024 || len(binding) == 0 {
		return "", apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
	}
	nonce := make([]byte, pages.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(pages.Seal(nonce, nonce, plain, binding)), nil
}
func openAPIPage(pages cipher.AEAD, token string, binding []byte) ([]byte, error) {
	invalid := apierrors.New(api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED)
	if pages == nil || len(binding) == 0 || len(token) > 2048 {
		return nil, invalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != token || len(raw) < pages.NonceSize()+pages.Overhead() || len(raw) > 1024+pages.NonceSize()+pages.Overhead() {
		return nil, invalid
	}
	plain, err := pages.Open(nil, raw[:pages.NonceSize()], raw[pages.NonceSize():], binding)
	if err != nil {
		return nil, invalid
	}
	return plain, nil
}
