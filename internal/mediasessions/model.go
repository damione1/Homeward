package mediasessions

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxLifetime = 60 * time.Second
const MaxActivePerActor = 8
const MaxActiveGlobal = 64
const MaxMetadataPerActor = 256
const MaxMetadataGlobal = 4096
const MaxArtworkReference = 512
const MaxConcurrentCalls = 64
const TombstoneRetention = 24 * time.Hour

type Code uint8

const (
	Invalid Code = iota + 1
	Denied
	NotFound
	SourceChanged
	ActiveCapacity
	RetentionCapacity
	Unavailable
	CommitUnknown
	CommittedResponseWithheld
	Closed
)

type Error struct{ code Code }

func (e *Error) Error() string { return "media session operation failed" }
func (e *Error) Code() Code {
	if e == nil {
		return Invalid
	}
	return e.code
}
func failure(code Code) error { return &Error{code} }
func IsCode(err error, code Code) bool {
	var own *Error
	return errors.As(err, &own) && own != nil && own.code == code
}

type Kind uint8

const (
	KindInvalid Kind = iota
	Artwork
	Camera
)

func (k Kind) Valid() bool { return k == Artwork || k == Camera }

// SourceBinding ties an exact native source slot to current configuration,
// admission/source epoch and compiled codec/generation fingerprint. This is a
// representation precondition, never a permission or provider credential.
type SourceBinding struct {
	entity domain.EntityID
	slot   authorization.CapabilitySlot
	kind   Kind
	digest [32]byte
}

func NewSourceBinding(entity domain.EntityID, slot authorization.CapabilitySlot, kind Kind, configuration int64, epoch string, codec [32]byte) (SourceBinding, error) {
	id, err := entity.Value()
	if err != nil || len(id) > 255 || !kind.Valid() || configuration < 1 || len(epoch) < 1 || len(epoch) > 128 || !utf8.ValidString(epoch) || strings.ContainsAny(epoch, "\x00\r\n") || codec == ([32]byte{}) {
		return SourceBinding{}, failure(Invalid)
	}
	schema, endpoint, err := slot.Values()
	if err != nil || len(schema) > 255 || len(endpoint) > 256 {
		return SourceBinding{}, failure(Invalid)
	}
	hash := sha256.New()
	hash.Write([]byte("homeward-media-source/2"))
	var size [8]byte
	for _, text := range []string{id, schema, endpoint, epoch} {
		binary.BigEndian.PutUint64(size[:], uint64(len(text)))
		hash.Write(size[:])
		hash.Write([]byte(text))
	}
	binary.BigEndian.PutUint64(size[:], uint64(configuration))
	hash.Write(size[:])
	hash.Write([]byte{byte(kind)})
	hash.Write(codec[:])
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	return SourceBinding{entity, slot, kind, digest}, nil
}
func (b SourceBinding) Valid() bool {
	_, err := b.entity.Value()
	_, _, slotErr := b.slot.Values()
	return err == nil && slotErr == nil && b.kind.Valid() && b.digest != ([32]byte{})
}
func (b SourceBinding) Entity() domain.EntityID            { return b.entity }
func (b SourceBinding) Slot() authorization.CapabilitySlot { return b.slot }
func (b SourceBinding) Kind() Kind                         { return b.kind }
func (b SourceBinding) Digest() [32]byte                   { return b.digest }

type Input struct {
	binding       SourceBinding
	reference     []byte
	lifetime      time.Duration
	eventRevision int64
	valid         bool
}

func NewInput(binding SourceBinding, reference []byte, lifetime time.Duration, eventRevision int64) (Input, error) {
	if !binding.Valid() || lifetime <= 0 || lifetime > MaxLifetime || lifetime%time.Microsecond != 0 || eventRevision < 0 {
		return Input{}, failure(Invalid)
	}
	switch binding.kind {
	case Artwork:
		if len(reference) < 1 || len(reference) > MaxArtworkReference || eventRevision != 0 {
			return Input{}, failure(Invalid)
		}
	case Camera:
		if len(reference) != 0 {
			return Input{}, failure(Invalid)
		}
	default:
		return Input{}, failure(Invalid)
	}
	return Input{binding, append([]byte(nil), reference...), lifetime, eventRevision, true}, nil
}

// Record is immutable session metadata. Retained tombstones serve source
// reauthorization only: Active is always checked before content can be read.
type Record struct {
	id                                 ID
	actor                              domain.ActorID
	credential                         [32]byte
	binding                            SourceBinding
	reference                          []byte
	eventRevision                      int64
	created, expires, retired, discard time.Time
}

func (r Record) ID() ID                      { return r.id }
func (r Record) Actor() domain.ActorID       { return r.actor }
func (r Record) CredentialBinding() [32]byte { return r.credential }
func (r Record) Source() SourceBinding       { return r.binding }
func (r Record) ArtworkReference() []byte    { return append([]byte(nil), r.reference...) }
func (r Record) EventRevision() int64        { return r.eventRevision }
func (r Record) CreateTime() time.Time       { return r.created }
func (r Record) ExpireTime() time.Time       { return r.expires }
func (r Record) Active(now time.Time) bool {
	return r.id.Valid() && r.retired.IsZero() && !now.Before(r.created) && now.Before(r.expires)
}
func (r Record) MatchesSource(current SourceBinding) bool {
	return r.binding.Valid() && current.Valid() && r.binding == current
}
func (r Record) valid() bool {
	actor, err := r.actor.Value()
	return err == nil && len(actor) <= 255 && r.id.Valid() && r.binding.Valid() && r.credential != ([32]byte{}) && !r.created.IsZero() && r.expires.After(r.created) && r.expires.Sub(r.created) <= MaxLifetime && r.discard.Equal(r.expires.Add(TombstoneRetention)) && r.eventRevision >= 0 && (r.binding.kind != Artwork || len(r.reference) >= 1 && len(r.reference) <= MaxArtworkReference) && (r.binding.kind != Camera || len(r.reference) == 0)
}
