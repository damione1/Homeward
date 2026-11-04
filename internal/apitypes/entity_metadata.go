package apitypes

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/damione1/Homeward/internal/domain"
)

var ErrDisplayName = errors.New("invalid entity display name")
var ErrMetadata = errors.New("invalid entity metadata")
var ErrETag = errors.New("invalid entity etag")

// DisplayName is a label, never an alias, identity or command discriminator.
// Its legitimate zero value is the empty override. Public construction admits
// at most 63 Unicode code points and rejects control/edge whitespace characters.
type DisplayName struct{ value string }

func NewDisplayName(text string) (DisplayName, error) {
	if !utf8.ValidString(text) || utf8.RuneCountInString(text) > 63 || strings.TrimSpace(text) != text {
		return DisplayName{}, ErrDisplayName
	}
	for _, character := range text {
		if unicode.IsControl(character) {
			return DisplayName{}, ErrDisplayName
		}
	}
	return DisplayName{strings.Clone(text)}, nil
}
func (n DisplayName) String() string { return n.value }

// EntityNameForIdentity uses the registry's unchanged native ID, including its
// underscores. The SQL registry index uses the same hash; cross-boundary tests
// check the exact bytes. Neither aliases nor display labels enter this mapping.
func EntityNameForIdentity(home HomeName, id domain.EntityID) (EntityName, error) {
	text, err := id.Value()
	if err != nil || !home.Valid() {
		return EntityName{}, ErrResourceName
	}
	digest := sha256.Sum256([]byte(text))
	return ParseEntityName(home.String() + "/entities/entity-" + hex.EncodeToString(digest[:24]))
}

type EntityETag struct{ value string }

func ParseEntityETag(text string) (EntityETag, error) {
	// Tokens are opaque to clients. Representation is bounded; any admitted
	// token that differs from the current value fails the live CAS as ABORTED.
	if !utf8.ValidString(text) || len(text) < 1 || len(text) > 256 {
		return EntityETag{}, ErrETag
	}
	for _, c := range text {
		if unicode.IsControl(c) {
			return EntityETag{}, ErrETag
		}
	}
	return EntityETag{text}, nil
}
func (e EntityETag) String() string { return e.value }
func (e EntityETag) Valid() bool    { return e.value != "" }

// EntityMetadata is the immutable registry-owned metadata image. Unknown
// historical timestamps remain absent; migration time is not identity creation.
type EntityMetadata struct {
	entity                 domain.EntityID
	label                  DisplayName
	revision               int64
	created, updated       time.Time
	hasCreated, hasUpdated bool
}

func NewEntityMetadata(entity domain.EntityID, label DisplayName, revision int64, created, updated *time.Time) (EntityMetadata, error) {
	if _, err := entity.Value(); err != nil || revision <= 0 {
		return EntityMetadata{}, ErrMetadata
	}
	m := EntityMetadata{entity: entity, label: label, revision: revision}
	for _, timestamp := range []*time.Time{created, updated} {
		if timestamp != nil && (timestamp.Year() < 1 || timestamp.Year() > 9999) {
			return EntityMetadata{}, ErrMetadata
		}
	}
	if created != nil {
		m.created, m.hasCreated = created.UTC(), true
	}
	if updated != nil {
		m.updated, m.hasUpdated = updated.UTC(), true
	}
	if m.hasCreated && m.hasUpdated && m.updated.Before(m.created) {
		return EntityMetadata{}, ErrMetadata
	}
	return m, nil
}
func (m EntityMetadata) Entity() domain.EntityID       { return m.entity }
func (m EntityMetadata) Label() DisplayName            { return m.label }
func (m EntityMetadata) Revision() int64               { return m.revision }
func (m EntityMetadata) CreateTime() (time.Time, bool) { return m.created, m.hasCreated }
func (m EntityMetadata) UpdateTime() (time.Time, bool) { return m.updated, m.hasUpdated }
func (m EntityMetadata) Valid() bool {
	_, err := m.entity.Value()
	return err == nil && m.revision > 0
}
func (m EntityMetadata) ETag() EntityETag {
	if !m.Valid() {
		return EntityETag{}
	}
	id, _ := m.entity.Value()
	h := sha256.New()
	h.Write([]byte("homeward.entity-metadata/v2\x00"))
	h.Write([]byte(id))
	var revision [8]byte
	binary.BigEndian.PutUint64(revision[:], uint64(m.revision))
	h.Write(revision[:])
	return EntityETag{`"` + hex.EncodeToString(h.Sum(nil)) + `"`}
}

type metadataPatchKind uint8

const (
	metadataPatchInvalid metadataPatchKind = iota
	metadataPatchKeep
	metadataPatchLabel
)

// MetadataPatch cannot express an observed-state or immutable identity update.
// A present empty mask is a real no-op; a selected empty label clears it.
type MetadataPatch struct {
	kind  metadataPatchKind
	label DisplayName
}

func KeepEntityMetadata() MetadataPatch { return MetadataPatch{kind: metadataPatchKeep} }
func SetEntityDisplayName(label DisplayName) MetadataPatch {
	return MetadataPatch{kind: metadataPatchLabel, label: label}
}
func (p MetadataPatch) Valid() bool {
	return p.kind == metadataPatchKeep || p.kind == metadataPatchLabel
}
func (p MetadataPatch) DisplayName() (DisplayName, bool) {
	return p.label, p.kind == metadataPatchLabel
}

type EntityUpdate struct {
	target    EntityName
	etag      EntityETag
	patch     MetadataPatch
	requestID RequestID
}

func NewEntityUpdate(target EntityName, etag EntityETag, patch MetadataPatch, requestID *RequestID) (EntityUpdate, error) {
	if !target.Valid() || !etag.Valid() || !patch.Valid() || requestID != nil && !requestID.Valid() {
		return EntityUpdate{}, ErrMetadata
	}
	u := EntityUpdate{target: target, etag: etag, patch: patch}
	if requestID != nil {
		u.requestID = *requestID
	}
	return u, nil
}
func (u EntityUpdate) Target() EntityName           { return u.target }
func (u EntityUpdate) ETag() EntityETag             { return u.etag }
func (u EntityUpdate) Patch() MetadataPatch         { return u.patch }
func (u EntityUpdate) RequestID() (RequestID, bool) { return u.requestID, u.requestID.Valid() }
func (u EntityUpdate) Valid() bool {
	return u.target.Valid() && u.etag.Valid() && u.patch.Valid()
}
