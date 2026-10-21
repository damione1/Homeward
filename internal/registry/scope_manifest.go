package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/damione1/Homeward/scope"
)

// ManifestCounts describes the active documentary selection. Capacity fields,
// source bindings and Host IDs never contribute additional manifest identities.
type ManifestCounts struct {
	Entities    int
	Sources     int
	Diagnostics int
}

// LoadManifest is the composition boundary for the two published selections.
// Both share the same exact recognized lineage; admission, source lookup and
// digest always describe only the selected, active set.
func LoadManifest(revision scope.Revision) (Manifest, error) {
	if _, err := revision.Value(); err != nil {
		return Manifest{}, ErrInvalidManifest
	}
	baseline := scope.ManifestV1()
	amendment, err := scope.ParseEcobeeAmendment(scope.EcobeeMVPV1Bytes(), baseline)
	if err != nil {
		return Manifest{}, ErrInvalidManifest
	}
	m, err := ParseManifest(baseline)
	if err != nil {
		return Manifest{}, err
	}
	m.lineage = make(map[HAAlias]struct{}, len(m.aliases)+len(amendment.AddedIDs()))
	for alias := range m.aliases {
		m.lineage[alias] = struct{}{}
	}
	for _, id := range amendment.AddedIDs() {
		alias, err := ParseHAAlias(id)
		source, present := amendment.SourceFor(id)
		if err != nil || !present {
			return Manifest{}, ErrInvalidManifest
		}
		if _, collision := m.lineage[alias]; collision {
			return Manifest{}, ErrInvalidManifest
		}
		m.lineage[alias] = struct{}{}
		if revision == scope.EcobeeMVPV1() {
			m.aliases[alias] = struct{}{}
			m.sources[alias] = SourceName{value: source}
		}
	}
	m.revision, m.amendment = revision, amendment
	ids := make([]string, 0, len(m.aliases))
	for alias := range m.aliases {
		id, err := alias.Value()
		if err != nil {
			return Manifest{}, ErrInvalidManifest
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	m.digest = sha256.Sum256([]byte(strings.Join(ids, "\n") + "\n"))
	return m, nil
}

// Revision is absent for zero/alias-only or noncanonical v1 documents, even
// when the legacy v1 parser can parse their set. It is never a config revision.
func (m Manifest) Revision() (scope.Revision, bool) {
	_, err := m.revision.Value()
	return m.revision, m.parsed && err == nil
}

func (m Manifest) BaselineDigest() ([sha256.Size]byte, bool) {
	if _, ok := m.Revision(); !ok {
		return [sha256.Size]byte{}, false
	}
	data, err := hex.DecodeString(scope.BaselineIDsSHA256)
	if err != nil || len(data) != sha256.Size {
		return [sha256.Size]byte{}, false
	}
	var digest [sha256.Size]byte
	copy(digest[:], data)
	return digest, true
}

func (m Manifest) Counts() (ManifestCounts, bool) {
	if _, ok := m.Revision(); !ok {
		return ManifestCounts{}, false
	}
	sources := map[SourceName]struct{}{}
	for _, source := range m.sources {
		sources[source] = struct{}{}
	}
	return ManifestCounts{Entities: len(m.aliases), Sources: len(sources), Diagnostics: 19}, true
}

func (m Manifest) ScopeAmendment() (scope.EcobeeAmendment, bool) {
	return m.amendment, m.parsed && m.amendment.Valid()
}

// recognizes is a storage-integrity/reservation boundary only. It deliberately
// does not grant Resolve/ConfigureBinding permission for an inactive identity.
// The lineage can only be installed by LoadManifest from the admitted asset.
func (m Manifest) recognizes(alias HAAlias) bool {
	if _, err := alias.Value(); err != nil {
		return false
	}
	if m.lineage == nil {
		return m.permits(alias)
	}
	_, ok := m.lineage[alias]
	return ok
}
