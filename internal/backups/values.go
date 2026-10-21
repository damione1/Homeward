package backups

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

type InstanceID struct{ value string }
type ArtifactID struct{ value string }

func ParseInstanceID(raw string) (InstanceID, error) {
	if len(raw) < 1 || len(raw) > 48 || raw[0] < 'a' || raw[0] > 'z' {
		return InstanceID{}, NewError(InvalidInput)
	}
	for _, r := range raw {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return InstanceID{}, NewError(InvalidInput)
		}
	}
	return InstanceID{value: raw}, nil
}
func (id InstanceID) Valid() bool    { return id.value != "" }
func (id InstanceID) String() string { return id.value }

func ParseArtifactID(raw string) (ArtifactID, error) {
	if len(raw) != 32 {
		return ArtifactID{}, NewError(InvalidInput)
	}
	for _, r := range raw {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return ArtifactID{}, NewError(InvalidInput)
		}
	}
	return ArtifactID{value: raw}, nil
}
func NewArtifactID() (ArtifactID, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return ArtifactID{}, NewError(Unavailable)
	}
	return ParseArtifactID(hex.EncodeToString(bytes[:]))
}
func (id ArtifactID) Valid() bool    { return id.value != "" }
func (id ArtifactID) String() string { return id.value }

// Metadata is an envelope claim. Only a completed Read or Verify checks bytes.
// An empty payload is valid and has SHA256(nil), independently of zero metadata.
type Metadata struct {
	id        ArtifactID
	size      int64
	digest    [sha256.Size]byte
	published time.Time
	valid     bool
}

func NewMetadata(id ArtifactID, size int64, digest [sha256.Size]byte, published time.Time) (Metadata, error) {
	if !id.Valid() || size < 0 || size > MaxArtifactBytes || published.IsZero() || published.UnixNano() <= 0 || !time.Unix(0, published.UnixNano()).Equal(published) {
		return Metadata{}, NewError(InvalidInput)
	}
	return Metadata{id: id, size: size, digest: digest, published: published.UTC(), valid: true}, nil
}
func (m Metadata) Valid() bool               { return m.valid }
func (m Metadata) ID() ArtifactID            { return m.id }
func (m Metadata) Size() int64               { return m.size }
func (m Metadata) SHA256() [sha256.Size]byte { return m.digest }
func (m Metadata) PublishedAt() time.Time    { return m.published }

type VerifiedArtifact struct{ metadata Metadata }

func NewVerifiedArtifact(metadata Metadata) (VerifiedArtifact, error) {
	if !metadata.Valid() {
		return VerifiedArtifact{}, NewError(InvalidInput)
	}
	return VerifiedArtifact{metadata: metadata}, nil
}
func (v VerifiedArtifact) Valid() bool        { return v.metadata.Valid() }
func (v VerifiedArtifact) Metadata() Metadata { return v.metadata }
