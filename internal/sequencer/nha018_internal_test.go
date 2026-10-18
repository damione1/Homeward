package sequencer

import (
	"math"
	"testing"
)

func TestNHA018ReadAtTerminalSequenceDoesNotWrap(t *testing.T) {
	s, err := New(RealClock{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	s.seq = math.MaxUint64
	s.count = 1
	cursor := Cursor{epoch: s.epoch, seq: math.MaxUint64}
	entries, next, err := s.ReadAfter(cursor, 1)
	if err != nil || len(entries) != 0 || next != cursor {
		t.Fatalf("terminal cursor wrapped: %d %+v %v", len(entries), next, err)
	}
}
