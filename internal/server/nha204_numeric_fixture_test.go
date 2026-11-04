package server

import "testing"

// Signed API quantities must be admitted before comparison with native u64 facts.
func nha204UnsignedNonnegative(t *testing.T, value int64) uint64 {
	t.Helper()
	if value < 0 {
		t.Fatalf("negative API quantity: %d", value)
	}
	return uint64(value)
}
