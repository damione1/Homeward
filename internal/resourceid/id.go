// Package resourceid provides one canonical, bounded, lossless encoding for
// existing opaque native identities in public resource name segments. Domain
// parsers still validate decoded values; this encoding grants no authority.
package resourceid

import (
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf8"
)

var ErrInvalid = errors.New("invalid opaque resource ID")

func Encode(raw string) (string, error) {
	if len(raw) == 0 || len(raw) > 255 || !utf8.ValidString(raw) || strings.ContainsAny(raw, "\x00\r\n") {
		return "", ErrInvalid
	}
	return "id-" + hex.EncodeToString([]byte(raw)), nil
}
func Decode(segment string) (string, error) {
	if len(segment) < 5 || len(segment) > 513 || !strings.HasPrefix(segment, "id-") {
		return "", ErrInvalid
	}
	raw, err := hex.DecodeString(segment[3:])
	if err != nil {
		return "", ErrInvalid
	}
	canonical, err := Encode(string(raw))
	if err != nil || canonical != segment {
		return "", ErrInvalid
	}
	return string(raw), nil
}
