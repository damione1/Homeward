// Package domain owns device-independent business identities.
package domain

import (
	"errors"
)

var ErrInvalidEntityID = errors.New("entity ID must be nonempty UTF-8")

// EntityID is an opaque internal identity, not an HA alias or a device label.
// Parsing establishes representation validity, not host provenance or registry membership.
// Its zero value is uninitialized: Value rejects it before publication.
type EntityID struct{ value string }

// ParseEntityID preserves the exact identity; no format or normalization is imposed.
func ParseEntityID(value string) (EntityID, error) {
	parsed, err := parseIdentity(value, ErrInvalidEntityID)
	return EntityID{value: parsed}, err
}

func (id EntityID) Value() (string, error) {
	return parseIdentity(id.value, ErrInvalidEntityID)
}
