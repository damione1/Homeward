// Package apitypes contains admitted API values, without protobuf, transport or
// provider dependencies. Go still permits zero values: boundary constructors
// reject invalid zero names/intents before they enter executable requests.
package apitypes

import (
	"errors"
	"strings"
)

// ErrResourceName identifies invalid resource syntax, never object existence.
var ErrResourceName = errors.New("invalid resource name")

// Names are distinct types so an entity cannot be passed as a capability.
// Their fields are private; String returns the admitted relative name unchanged.
type HomeName struct{ value string }
type EntityName struct{ value string }
type CapabilityName struct{ value string }

func (v HomeName) String() string       { return v.value }
func (v EntityName) String() string     { return v.value }
func (v CapabilityName) String() string { return v.value }
func (v HomeName) Valid() bool          { return v.value != "" }
func (v EntityName) Valid() bool        { return v.value != "" }
func (v CapabilityName) Valid() bool    { return v.value != "" }

// Resource IDs are opaque system-assigned ASCII DNS labels, 1..63 bytes. No
// percent decoding, Unicode normalization, alias resolution or case folding.
// Names prove representation only; lookup/ownership and authz are separate.
func resourceName(value string, collections ...string) error {
	maximum := 65*len(collections) - 1
	for _, collection := range collections {
		maximum += len(collection)
	}
	if len(value) > maximum {
		return ErrResourceName
	}
	parts := strings.Split(value, "/")
	if len(parts) != 2*len(collections) {
		return ErrResourceName
	}
	for i, collection := range collections {
		id := parts[2*i+1]
		if parts[2*i] != collection || len(id) < 1 || len(id) > 63 || id[0] == '-' || id[len(id)-1] == '-' {
			return ErrResourceName
		}
		for _, b := range []byte(id) {
			if !(b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-') {
				return ErrResourceName
			}
		}
	}
	return nil
}

func ParseHomeName(value string) (HomeName, error) {
	if err := resourceName(value, "homes"); err != nil {
		return HomeName{}, err
	}
	return HomeName{value}, nil
}
func ParseEntityName(value string) (EntityName, error) {
	if err := resourceName(value, "homes", "entities"); err != nil {
		return EntityName{}, err
	}
	return EntityName{value}, nil
}
func ParseCapabilityName(value string) (CapabilityName, error) {
	if err := resourceName(value, "homes", "entities", "capabilities"); err != nil {
		return CapabilityName{}, err
	}
	return CapabilityName{value}, nil
}

// Entity returns this slot's admitted parent. A zero slot has a zero parent.
func (v CapabilityName) Entity() EntityName {
	if !v.Valid() {
		return EntityName{}
	}
	return EntityName{v.value[:strings.LastIndex(v.value, "/capabilities/")]}
}

// Home returns the entity's installation, preserving immutable identity.
func (v EntityName) Home() HomeName {
	if !v.Valid() {
		return HomeName{}
	}
	return HomeName{v.value[:strings.LastIndex(v.value, "/entities/")]}
}
