package sequencer

import (
	"context"
	"errors"
	"math"
	"testing"
)

func TestNHA165CataloguePositionUsesNonwrappingAdmissionSequence(t *testing.T) {
	s, e := New(RealClock{}, 8)
	if e != nil {
		t.Fatal(e)
	}
	first, e := s.CataloguePosition()
	if e != nil {
		t.Fatal(e)
	}
	if e = s.PublishRegistry(context.Background(), nil); e != nil {
		t.Fatal(e)
	}
	published, e := s.CataloguePosition()
	if e != nil || published == first {
		t.Fatal("registry admission left catalogue fence unchanged", e)
	}
	s.mu.Lock()
	s.seq = math.MaxUint64 - 1
	s.mu.Unlock()
	if e = s.PublishRegistry(context.Background(), nil); e != nil {
		t.Fatal(e)
	}
	last, e := s.CataloguePosition()
	if e != nil {
		t.Fatal(e)
	}
	_, sequence, e := last.Components()
	if e != nil || sequence != math.MaxUint64 {
		t.Fatal("catalogue maximum sequence", sequence, e)
	}
	if e = s.PublishRegistry(context.Background(), nil); !errors.Is(e, ErrSequenceExhausted) {
		t.Fatalf("sequence exhaustion wrapped metadata fence: %v", e)
	}
	same, e := s.CataloguePosition()
	if e != nil || same != last {
		t.Fatal("failed publication changed metadata fence", e)
	}
	s.InvalidateRegistry()
	if _, e = s.CataloguePosition(); !errors.Is(e, ErrRegistryUnavailable) {
		t.Fatal("invalid catalogue returned a fence", e)
	}
}
