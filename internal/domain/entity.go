// Package domain owns device-independent business identities.
package domain

import (
	"errors"
	"unicode/utf8"
)

var ErrInvalidEntityID = errors.New("entity ID must be nonempty UTF-8")

// EntityID is an opaque internal identity, not an HA alias or a device label.
// Parsing establishes representation validity, not host provenance or registry membership.
// Its zero value is uninitialized: Value rejects it before publication.
type EntityID struct{ value string }

// ParseEntityID preserves the exact identity; no format or normalization is imposed.
func ParseEntityID(value string) (EntityID, error) {
	if value == "" || !utf8.ValidString(value) {
		return EntityID{}, ErrInvalidEntityID
	}
	return EntityID{value: value}, nil
}

func (id EntityID) Value() (string, error) {
	if id.value == "" {
		return "", ErrInvalidEntityID
	}
	return id.value, nil
}
