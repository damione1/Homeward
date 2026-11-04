// Package mediacontent admits bounded native content values. It owns no leases,
// credentials, URLs, decoders, provider I/O or public protobuf compatibility.
package mediacontent

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"image/png"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/mediasessions"
)

var ErrInvalid = errors.New("invalid media source content")
var ErrNotFound = errors.New("media source content does not exist")
var ErrUnavailable = errors.New("media source unavailable")
var ErrUnsupported = errors.New("native media content unsupported")
var ErrSourceChanged = errors.New("media source generation changed")

const MaxSources = 256
const MaxEvents = 10000

type Provenance uint8

const (
	ProvenanceInvalid Provenance = iota
	Observed
	Synthetic
)

func (p Provenance) Valid() bool { return p == Observed || p == Synthetic }
func text(raw string, max int, empty bool) bool {
	if (!empty && raw == "") || len(raw) > max || !utf8.ValidString(raw) {
		return false
	}
	for _, c := range raw {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}

// Source is one configured content slot and kind. Its binding includes the
// native configuration/epoch/codec generation. Quality and provenance remain
// independent; no writable media action is implied by content readability.
type Source struct {
	binding    mediasessions.SourceBinding
	label      string
	quality    domain.Quality
	provenance Provenance
	valid      bool
}

func NewSource(binding mediasessions.SourceBinding, label string, quality domain.Quality, provenance Provenance) (Source, error) {
	_, _, _, err := quality.Components()
	if !binding.Valid() || !text(label, 1024, false) || err != nil || !provenance.Valid() {
		return Source{}, ErrInvalid
	}
	return Source{binding, strings.Clone(label), quality, provenance, true}, nil
}
func (s Source) Valid() bool                          { return s.valid && s.binding.Valid() && s.provenance.Valid() }
func (s Source) Binding() mediasessions.SourceBinding { return s.binding }
func (s Source) Label() string                        { return s.label }
func (s Source) Quality() domain.Quality              { return s.quality }
func (s Source) Provenance() Provenance               { return s.provenance }

type Image struct {
	png       []byte
	captured  time.Time
	binding   mediasessions.SourceBinding
	target    mediasessions.ImageTarget
	reference [32]byte
}

// NewImage bounds allocation dimensions from the PNG header before any full
// decoder can allocate pixels. The source owns original bytes; copies prevent
// a producer from changing a response after admission.
func NewImage(binding mediasessions.SourceBinding, target mediasessions.ImageTarget, reference []byte, raw []byte, captured time.Time) (Image, error) {
	if !binding.Valid() {
		return Image{}, ErrInvalid
	}
	kind := binding.Kind()
	if target.Kind() != mediasessions.SnapshotImage && target.Kind() != mediasessions.EventImage {
		return Image{}, ErrInvalid
	}
	if kind == mediasessions.Artwork && (target.Kind() != mediasessions.SnapshotImage || len(reference) < 1 || len(reference) > mediasessions.MaxArtworkReference) {
		return Image{}, ErrInvalid
	}
	if kind == mediasessions.Camera && len(reference) != 0 {
		return Image{}, ErrInvalid
	}
	maxBytes, maxSide := 512<<10, 1024
	if kind == mediasessions.Artwork {
		maxBytes, maxSide = 128<<10, 512
	} else if kind != mediasessions.Camera {
		return Image{}, ErrInvalid
	}
	if len(raw) < 8 || len(raw) > maxBytes || !validTime(captured) {
		return Image{}, ErrInvalid
	}
	config, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > maxSide || config.Height > maxSide {
		return Image{}, ErrInvalid
	}
	return Image{bytes.Clone(raw), captured.UTC(), binding, target, sha256.Sum256(reference)}, nil
}
func (i Image) Valid() bool              { return i.binding.Valid() && len(i.png) > 0 && !i.captured.IsZero() }
func (i Image) PNG() []byte              { return bytes.Clone(i.png) }
func (i Image) CaptureTime() time.Time   { return i.captured }
func (i Image) Kind() mediasessions.Kind { return i.binding.Kind() }

type Event struct {
	id            mediasessions.EventID
	label, source string
	captured      time.Time
	image         bool
}

func NewEvent(id mediasessions.EventID, label, source string, captured time.Time, image bool) (Event, error) {
	if !id.Valid() || !text(label, 1024, false) || !text(source, 1024, false) || !validTime(captured) {
		return Event{}, ErrInvalid
	}
	return Event{id, strings.Clone(label), strings.Clone(source), captured.UTC(), image}, nil
}
func (e Event) ID() mediasessions.EventID { return e.id }
func (e Event) Label() string             { return e.label }
func (e Event) Source() string            { return e.source }
func (e Event) CaptureTime() time.Time    { return e.captured }
func (e Event) HasImage() bool            { return e.image }
func (e Event) Valid() bool {
	return e.id.Valid() && validTime(e.captured) && text(e.label, 1024, false) && text(e.source, 1024, false)
}

type EventCollection struct {
	binding  mediasessions.SourceBinding
	revision int64
	events   []Event
}

func NewEventCollection(binding mediasessions.SourceBinding, revision int64, events []Event) (EventCollection, error) {
	if !binding.Valid() || binding.Kind() != mediasessions.Camera || revision < 1 || len(events) > MaxEvents {
		return EventCollection{}, ErrInvalid
	}
	seen := make(map[mediasessions.EventID]bool, len(events))
	charge := 0
	for _, event := range events {
		if !event.Valid() || seen[event.id] {
			return EventCollection{}, ErrInvalid
		}
		seen[event.id] = true
		charge += len(event.id.String()) + len(event.label) + len(event.source) + 128
		if charge > 8<<20 {
			return EventCollection{}, ErrInvalid
		}
	}
	return EventCollection{binding, revision, append([]Event(nil), events...)}, nil
}
func (c EventCollection) Revision() int64 { return c.revision }
func (c EventCollection) Events() []Event { return append([]Event(nil), c.events...) }
func (c EventCollection) Valid() bool {
	return c.binding.Valid() && c.revision > 0 && len(c.events) <= MaxEvents
}

func validTime(value time.Time) bool {
	utc := value.UTC()
	return !value.IsZero() && utc.Year() >= 1 && utc.Year() <= 9999
}
func (i Image) Matches(binding mediasessions.SourceBinding, target mediasessions.ImageTarget, reference []byte) bool {
	return i.binding == binding && i.target == target && i.reference == sha256.Sum256(reference)
}
func (c EventCollection) Matches(binding mediasessions.SourceBinding) bool {
	return c.binding == binding
}
