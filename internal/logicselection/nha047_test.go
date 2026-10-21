package logicselection_test

import (
	"testing"

	"github.com/damione1/Homeward/internal/logicselection"
)

func TestNHA047SelectionEntriesAreAnImmutablePartition(t *testing.T) {
	selection, err := logicselection.LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	entries := selection.Entries()
	if len(entries) != 89 {
		t.Fatalf("want 89 entries, got %d", len(entries))
	}
	last := ""
	retained, deferred := 0, 0
	for _, entry := range entries {
		id, err := entry.ID().Value()
		if err != nil || id <= last {
			t.Fatalf("invalid or unordered identity %q", id)
		}
		last = id
		switch entry.Disposition() {
		case logicselection.DispositionRetained:
			retained++
		case logicselection.DispositionDeferred:
			deferred++
		default:
			t.Fatal("unspecified disposition escaped admission")
		}
		if got, ok := selection.DispositionFor(entry.ID()); !ok || got != entry.Disposition() {
			t.Fatal("enumeration disagrees with admitted selection")
		}
	}
	if retained != 17 || deferred != 72 {
		t.Fatalf("partition changed: %d/%d", retained, deferred)
	}
	entries[0] = logicselection.Entry{}
	if _, err := selection.Entries()[0].ID().Value(); err != nil {
		t.Fatal("caller mutated the selection")
	}
	if (logicselection.Selection{}).Entries() != nil {
		t.Fatal("zero selection exposes entries")
	}
}
