package mediacontent

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/png"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/mediasessions"
)

func nha202Binding(t *testing.T, id string, kind mediasessions.Kind) mediasessions.SourceBinding {
	t.Helper()
	entity, _ := domain.ParseEntityID(id)
	slot, _ := authorization.NewCapabilitySlot("synthetic-content.v2", "content")
	var codec [32]byte
	codec[0] = 1
	binding, err := mediasessions.NewSourceBinding(entity, slot, kind, 1, "synthetic-epoch", codec)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}
func TestNHA202ContentImageBoundsAndPrivateSourceTargetProof(t *testing.T) {
	binding := nha202Binding(t, "synthetic-camera", mediasessions.Camera)
	other := nha202Binding(t, "other-camera", mediasessions.Camera)
	var raw bytes.Buffer
	if err := png.Encode(&raw, image.NewNRGBA(image.Rect(0, 0, 96, 64))); err != nil {
		t.Fatal(err)
	}
	target := mediasessions.SnapshotTarget()
	admitted, err := NewImage(binding, target, nil, raw.Bytes(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !admitted.Matches(binding, target, nil) || admitted.Matches(other, target, nil) {
		t.Fatal("image lost source identity")
	}
	copy := admitted.PNG()
	copy[0] ^= 255
	if bytes.Equal(copy, admitted.PNG()) {
		t.Fatal("image ownership aliases consumer")
	}
	if _, err := NewImage(binding, target, nil, bytes.Repeat([]byte{0}, (512<<10)+1), time.Now()); err == nil {
		t.Fatal("oversize pixel bytes")
	}
	if _, err := NewImage(binding, target, nil, raw.Bytes(), time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("invalid protobuf capture time")
	}
	var oversized bytes.Buffer
	if err := png.Encode(&oversized, image.NewNRGBA(image.Rect(0, 0, 1025, 1))); err != nil {
		t.Fatal(err)
	}
	if _, err := NewImage(binding, target, nil, oversized.Bytes(), time.Now()); err == nil {
		t.Fatal("overbound allocation dimensions")
	}
}
func TestNHA202ContentEventsKeepSourceAndRejectDuplicateNativeIdentities(t *testing.T) {
	binding := nha202Binding(t, "synthetic-camera", mediasessions.Camera)
	uid, _ := mediasessions.ParseEventID("synthetic-event")
	event, err := NewEvent(uid, "Event", "Synthetic source", time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewEventCollection(binding, 1, []Event{event, event}); err == nil {
		t.Fatal("duplicate source event IDs")
	}
	collection, err := NewEventCollection(binding, 1, []Event{event})
	if err != nil || !collection.Valid() || !collection.Matches(binding) {
		t.Fatal("source-bound event capture", err)
	}
	rows := collection.Events()
	rows[0] = Event{}
	if !collection.Events()[0].Valid() {
		t.Fatal("mutable source capture ownership")
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	source, err := NewSource(binding, "Synthetic source", quality, Synthetic)
	if err != nil || source.Provenance() != Synthetic {
		t.Fatal("data provenance changed observation quality", err)
	}
	if _, err := NewSource(binding, "Synthetic source", quality, Provenance(255)); err == nil {
		t.Fatal("unknown actionable provenance")
	}
}
func TestNHA202ContentVideoRejectsOpenNALsAndForeignSourceProof(t *testing.T) {
	binding := nha202Binding(t, "synthetic-camera", mediasessions.Camera)
	other := nha202Binding(t, "other-camera", mediasessions.Camera)
	header, err := NewVideoHeader(binding, []byte{0x67, 66, 0, 31}, []byte{0x68, 0}, 1920, 1080, 30)
	if err != nil || !header.Matches(binding) || header.Matches(other) {
		t.Fatal("video route proof", err)
	}
	if _, err := NewVideoHeader(binding, []byte{0x67, 100, 0, 31}, []byte{0x68, 0}, 1920, 1080, 30); err == nil {
		t.Fatal("unknown baseline profile")
	}
	sample := func(nals ...[]byte) []byte {
		var raw []byte
		for _, nal := range nals {
			raw = binary.BigEndian.AppendUint32(raw, uint32(len(nal)))
			raw = append(raw, nal...)
		}
		return raw
	}
	valid, err := NewVideoSample(binding, sample([]byte{0x65, 1, 2}), 0)
	if err != nil || !valid.Keyframe() || valid.Matches(other) {
		t.Fatal("exact first keyframe", err)
	}
	for _, raw := range [][]byte{nil, {0, 0, 0}, sample(nil), sample([]byte{0xe5, 0}), sample([]byte{0x67, 66, 0, 31}), sample([]byte{0x65, 0}, []byte{0x41, 0}), sample([]byte{0x09, 0})} {
		if _, err := NewVideoSample(binding, raw, time.Microsecond); err == nil {
			t.Fatal("malformed/open native sample")
		}
	}
	if _, err := NewVideoSample(binding, valid.AVCC(), time.Nanosecond); err == nil {
		t.Fatal("submicrosecond sample")
	}
}
