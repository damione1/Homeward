// Package mediasessions owns the current caller-bound MediaSession metadata.
// Image/video bytes stay with source owners and never enter this repository.
package mediasessions

import (
	"crypto/rand"
	"encoding/base32"
	"github.com/damione1/Homeward/internal/apitypes"
	"strings"
	"unicode"
	"unicode/utf8"
)

type ID struct{ value string }

func NewID() (ID, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return ID{}, failure(Unavailable)
	}
	return ID{encodeID(random[:])}, nil
}
func encodeID(raw []byte) string {
	return "s-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
}
func ParseID(raw string) (ID, error) {
	if len(raw) != 54 || !strings.HasPrefix(raw, "s-") {
		return ID{}, failure(Invalid)
	}
	for _, c := range raw[2:] {
		if !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			return ID{}, failure(Invalid)
		}
	}
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(raw[2:]))
	if err != nil || len(decoded) != 32 || encodeID(decoded) != raw {
		return ID{}, failure(Invalid)
	}
	return ID{raw}, nil
}
func (id ID) String() string { return id.value }
func (id ID) Valid() bool    { _, err := ParseID(id.value); return err == nil }

type Name struct {
	home apitypes.HomeName
	id   ID
}

func ResourceName(home apitypes.HomeName, id ID) (Name, error) {
	if !home.Valid() || !id.Valid() {
		return Name{}, failure(Invalid)
	}
	return Name{home, id}, nil
}
func ParseName(raw string) (Name, error) {
	if len(raw) > 256 {
		return Name{}, failure(Invalid)
	}
	parts := strings.Split(raw, "/")
	if len(parts) != 4 || parts[2] != "mediaSessions" {
		return Name{}, failure(Invalid)
	}
	home, err := apitypes.ParseHomeName(strings.Join(parts[:2], "/"))
	if err != nil {
		return Name{}, failure(Invalid)
	}
	id, err := ParseID(parts[3])
	if err != nil {
		return Name{}, err
	}
	return ResourceName(home, id)
}
func (n Name) String() string {
	if !n.home.Valid() || !n.id.Valid() {
		return ""
	}
	return n.home.String() + "/mediaSessions/" + n.id.String()
}
func (n Name) Home() apitypes.HomeName { return n.home }
func (n Name) ID() ID                  { return n.id }

type EventID struct{ value string }

func ParseEventID(raw string) (EventID, error) {
	if len(raw) < 1 || len(raw) > 256 || !utf8.ValidString(raw) {
		return EventID{}, failure(Invalid)
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return EventID{}, failure(Invalid)
		}
	}
	return EventID{strings.Clone(raw)}, nil
}
func (id EventID) String() string { return id.value }
func (id EventID) Valid() bool    { _, err := ParseEventID(id.value); return err == nil }

type ImageTargetKind uint8

const (
	ImageTargetInvalid ImageTargetKind = iota
	SnapshotImage
	EventImage
)

type ImageTarget struct {
	kind  ImageTargetKind
	event EventID
}

func SnapshotTarget() ImageTarget { return ImageTarget{kind: SnapshotImage} }
func EventTarget(id EventID) (ImageTarget, error) {
	if !id.Valid() {
		return ImageTarget{}, failure(Invalid)
	}
	return ImageTarget{kind: EventImage, event: id}, nil
}
func (t ImageTarget) Kind() ImageTargetKind  { return t.kind }
func (t ImageTarget) Event() (EventID, bool) { return t.event, t.kind == EventImage && t.event.Valid() }
