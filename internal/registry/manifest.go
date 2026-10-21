// Package registry persists explicitly configured protocol identities and HA aliases.
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/scope"
)

var ErrInvalidManifest = errors.New("invalid registry manifest")
var ErrInvalidHAAlias = domain.ErrInvalidHAAlias

// HAAlias is a Home Assistant domain.object_id label, never a Host EntityID.
// Its zero value is invalid and must not cross the registry boundary.
type HAAlias = domain.HAAlias

func ParseHAAlias(value string) (HAAlias, error) {
	return domain.ParseHAAlias(value)
}

// SourceName is open provenance from the frozen selection, not an integration kind.
// An alias-only Manifest has no source provenance to report.
type SourceName struct{ value string }

func (s SourceName) Value() (string, bool) {
	return s.value, s.value != "" && utf8.ValidString(s.value)
}

// Manifest is a parsed admission boundary. Its zero value grants no identities.
// A later scope amendment must be parsed and supplied explicitly by composition.
type Manifest struct {
	aliases   map[HAAlias]struct{}
	sources   map[HAAlias]SourceName
	digest    [sha256.Size]byte
	parsed    bool
	revision  scope.Revision
	lineage   map[HAAlias]struct{}
	amendment scope.EcobeeAmendment
}

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
	sources := make(map[HAAlias]SourceName, len(raw.Entities))
	for _, item := range raw.Entities {
		alias, err := ParseHAAlias(item.ID)
		if err != nil || item.Source == "" || !utf8.ValidString(item.Source) {
			return Manifest{}, ErrInvalidManifest
		}
		if _, exists := known[alias]; exists {
			return Manifest{}, ErrInvalidManifest
		}
		known[alias] = struct{}{}
		sources[alias] = SourceName{value: item.Source}
		ids = append(ids, item.ID)
	}
	sort.Strings(ids)
	digest := sha256.Sum256([]byte(strings.Join(ids, "\n") + "\n"))
	if hex.EncodeToString(digest[:]) != raw.Digest {
		return Manifest{}, ErrInvalidManifest
	}
	result := Manifest{aliases: known, sources: sources, digest: digest, parsed: true}
	fileDigest := sha256.Sum256(data)
	if hex.EncodeToString(fileDigest[:]) == scope.BaselineManifestSHA256 {
		result.revision = scope.BaselineV1()
	}
	return result, nil
}

// SourceFor reports only provenance supplied by ParseManifest. It does not infer
// a provider from an alias or make an alias-only Manifest appear sourced.
func (m Manifest) SourceFor(alias HAAlias) (SourceName, bool) {
	if _, err := alias.Value(); err != nil {
		return SourceName{}, false
	}
	source, ok := m.sources[alias]
	return source, ok
}

// Digest is absent on zero or alias-only manifests. The digest is of the
// selected ID set validated by ParseManifest, not of the JSON representation.
func (m Manifest) Digest() ([sha256.Size]byte, bool) { return m.digest, m.parsed }

func (m Manifest) permits(alias HAAlias) bool {
	_, ok := m.aliases[alias]
	return ok
}
