// Package registry persists explicitly configured protocol identities and HA aliases.
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
)

var ErrInvalidManifest = errors.New("invalid registry manifest")
var ErrInvalidHAAlias = errors.New("invalid Home Assistant alias")

// HAAlias is a Home Assistant domain.object_id label, never a Host EntityID.
// Its zero value is invalid and must not cross the registry boundary.
type HAAlias struct{ value string }

var haAliasPattern = regexp.MustCompile(`^[a-z0-9_]+\.[a-z0-9_]+$`)

func ParseHAAlias(value string) (HAAlias, error) {
	if len(value) == 0 || len(value) > 255 || !haAliasPattern.MatchString(value) {
		return HAAlias{}, ErrInvalidHAAlias
	}
	return HAAlias{value: value}, nil
}

func (a HAAlias) Value() (string, error) {
	if _, err := ParseHAAlias(a.value); err != nil {
		return "", err
	}
	return a.value, nil
}

// Manifest is a parsed admission boundary. Its zero value grants no identities.
// A later scope amendment must be parsed and supplied explicitly by composition.
type Manifest struct{ aliases map[HAAlias]struct{} }

func ParseManifest(data []byte) (Manifest, error) {
	var raw struct {
		Version  int    `json:"version"`
		Digest   string `json:"ids_sha256"`
		Entities []struct {
			ID     string `json:"entity_id"`
			Source string `json:"source"`
		} `json:"entities"`
	}
	if err := json.Unmarshal(data, &raw); err != nil || raw.Version != 1 || len(raw.Entities) != 850 {
		return Manifest{}, ErrInvalidManifest
	}
	ids := make([]string, 0, len(raw.Entities))
	known := make(map[HAAlias]struct{}, len(raw.Entities))
	for _, item := range raw.Entities {
		alias, err := ParseHAAlias(item.ID)
		if err != nil || item.Source == "" {
			return Manifest{}, ErrInvalidManifest
		}
		if _, exists := known[alias]; exists {
			return Manifest{}, ErrInvalidManifest
		}
		known[alias] = struct{}{}
		ids = append(ids, item.ID)
	}
	sort.Strings(ids)
	digest := sha256.Sum256([]byte(strings.Join(ids, "\n") + "\n"))
	if hex.EncodeToString(digest[:]) != raw.Digest {
		return Manifest{}, ErrInvalidManifest
	}
	return Manifest{aliases: known}, nil
}

func (m Manifest) permits(alias HAAlias) bool {
	_, ok := m.aliases[alias]
	return ok
}
