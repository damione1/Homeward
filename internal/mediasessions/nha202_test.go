package mediasessions

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
)

func TestNHA202SessionIdentityCanonicalBitsAndResourceRoundtrip(t *testing.T) {
	home, err := apitypes.ParseHomeName("homes/synthetic-nha202")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{make([]byte, 32), bytes.Repeat([]byte{255}, 32)} {
		encoded := encodeID(raw)
		id, err := ParseID(encoded)
		if err != nil {
			t.Fatal(err)
		}
		name, err := ResourceName(home, id)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseName(name.String())
		if err != nil || parsed != name {
			t.Fatal("canonical roundtrip", err)
		}
		last := encoded[len(encoded)-1]
		if last != 'a' && last != 'q' {
			t.Fatal("noncanonical padding bits")
		}
		for _, other := range []string{strings.ToUpper(encoded), encoded[:len(encoded)-1] + "b", encoded + "a", " " + encoded} {
			if _, err := ParseID(other); err == nil {
				t.Fatal("accepted noncanonical ID")
			}
		}
	}
	for i := 0; i < 16; i++ {
		id, err := NewID()
		if err != nil || !id.Valid() {
			t.Fatal("random identity", err)
		}
	}
}
func TestNHA202SessionInputPreservesOwnedOpaqueReferenceAndExactSource(t *testing.T) {
	entity, _ := domain.ParseEntityID("synthetic-camera-1")
	slot, _ := authorization.NewCapabilitySlot("home.cameras.v1.Camera", "camera")
	var codec [32]byte
	codec[0] = 1
	camera, err := NewSourceBinding(entity, slot, Camera, 1, "source-epoch", codec)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := NewSourceBinding(entity, slot, Camera, 2, "source-epoch", codec)
	if err != nil || changed == camera {
		t.Fatal("configuration absent from binding")
	}
	input, err := NewInput(camera, nil, MaxLifetime, 4)
	if err != nil || !input.valid {
		t.Fatal(err)
	}
	for _, duration := range []time.Duration{0, -1, MaxLifetime + time.Microsecond, time.Nanosecond} {
		if _, err := NewInput(camera, nil, duration, 1); err == nil {
			t.Fatal("unbounded lease admitted")
		}
	}
	if _, err := NewInput(camera, []byte("opaque"), time.Second, 1); err == nil {
		t.Fatal("camera admitted artwork ref")
	}
	art, err := NewSourceBinding(entity, slot, Artwork, 1, "source-epoch", codec)
	if err != nil {
		t.Fatal(err)
	}
	reference := []byte{0, 255, 12}
	artInput, err := NewInput(art, reference, time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	reference[0] = 42
	if artInput.reference[0] != 0 {
		t.Fatal("reference aliases caller bytes")
	}
	if _, err := NewInput(art, nil, time.Second, 0); err == nil {
		t.Fatal("missing artwork ref")
	}
	actor, _ := domain.ParseActorID("synthetic-actor")
	id, _ := NewID()
	now := time.Now().UTC().Truncate(time.Microsecond)
	record := Record{id: id, actor: actor, credential: codec, binding: art, reference: artInput.reference, created: now, expires: now.Add(time.Second), discard: now.Add(time.Second + TombstoneRetention)}
	if !record.valid() || !record.Active(now) || record.Active(now.Add(-time.Microsecond)) || record.Active(record.expires) {
		t.Fatal("invalid lifetime fence")
	}
	record.retired = now
	if record.Active(now) {
		t.Fatal("retirement exposed active content")
	}
}

func TestNHA202MediaSourceNamesSeparateSlotsWithoutReversingEntityNames(t *testing.T) {
	home, _ := apitypes.ParseHomeName("homes/synthetic-nha202")
	entity, _ := domain.ParseEntityID("same-native-entity")
	first, _ := authorization.NewCapabilitySlot("synthetic-content.v2", "first")
	second, _ := authorization.NewCapabilitySlot("synthetic-content.v2", "second")
	firstSchema, firstEndpoint, _ := first.Values()
	a, err := apitypes.MediaSourceNameForIdentity(home, entity, firstSchema, firstEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	secondSchema, secondEndpoint, _ := second.Values()
	b, err := apitypes.MediaSourceNameForIdentity(home, entity, secondSchema, secondEndpoint)
	if err != nil || a == b {
		t.Fatal("independent source slots merged", err)
	}
	parsed, err := apitypes.ParseMediaSourceName(a.String())
	if err != nil || parsed != a {
		t.Fatal("canonical source name", err)
	}
	if _, err := apitypes.ParseMediaSourceName(strings.ToUpper(a.String())); err == nil {
		t.Fatal("source alias normalization")
	}
}
