package observability

import (
	"testing"

	"github.com/damione1/Homeward/contracts"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
)

func TestNHA056JournalBoundsEvictionAndReadWithoutEmission(t *testing.T) {
	epoch, cells, codec := nha056Fixture(t)
	journal, err := NewJournal(epoch, cells)
	if err != nil {
		t.Fatal(err)
	}
	binding := nha056Binding(t, 1)
	event := nha056Event(t)
	if err := journal.Emit(event); err != nil {
		t.Fatal(err)
	}
	old, _ := codec.BeginLogs(binding, 1)
	old.first = false
	old.next = 1
	for range 1025 {
		if err := journal.Emit(event); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, _ := journal.Snapshot()
	if snapshot.RetainedRows() != 1024 || snapshot.ChargedBytes() > 1<<20 || snapshot.EvictedTotal().GetExact() != 2 {
		t.Fatal("ring bounds or eviction count wrong")
	}
	size, _ := NewPageSize(100)
	request, _ := NewLogPageRequest(size, old)
	page, err := journal.Read(request)
	if err != nil {
		t.Fatal(err)
	}
	if page.CursorEvicted() == nil {
		t.Fatal("evicted cursor returned a successful page")
	}
	cursor, _ := codec.BeginLogs(binding, snapshot.Through())
	request, _ = NewLogPageRequest(size, cursor)
	page, err = journal.Read(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows()) != 100 {
		t.Fatalf("page rows=%d", len(page.Rows()))
	}
	next, more := page.Next()
	if !more {
		t.Fatal("missing continuation")
	}
	token, _ := codec.EncodeLogs(next)
	wire, err := page.Wire(token)
	if err != nil || contracts.AdmitObservabilityResponse(wire) != nil {
		t.Fatal("invalid wire page", err)
	}
	after, _ := journal.Snapshot()
	if after.Through() != snapshot.Through() {
		t.Fatal("read emitted a log")
	}
	if event.Wire().GetCode() != obs.LogCode_LOG_CODE_PROCESS_READY {
		t.Fatal("code lost")
	}
}
