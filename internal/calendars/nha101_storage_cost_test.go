//go:build !race

package calendars

import (
	"testing"

	"github.com/damione1/Homeward/internal/storage"
)

// Measure the actual SQL writer and the SQL->decode->Query->result-copy reader,
// separately. The old parser/codec costs are dependencies, not these proofs.
func TestNHA101CostStorageWriterAndReader(t *testing.T) {
	f, id, w, scope := nha101StoredSource(t)
	window := nha101StoredWindow(t, "2026-01-01T00:00:00Z", "2027-01-01T00:00:00Z")
	for _, name := range []string{"20-eight-zones-conversion.ics", "21-override-content-conversion.ics", "22-sparse-observance-allocation.ics"} {
		t.Run(name, func(t *testing.T) {
			raw := costRaw(t, name)
			current, err := w.Read(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			obs := nha101StoredObservation(t, scope, name)
			var mutation, read remoteDecodeObservation
			f.repo.afterRemoteMutation = func(o remoteDecodeObservation) { mutation = o }
			f.repo.afterRemoteRead = func(o remoteDecodeObservation) { read = o }
			defer func() { f.repo.afterRemoteMutation = nil; f.repo.afterRemoteRead = nil }()
			var receipt storage.RemoteCalendarCommit
			delta := nha101Measure(func() { receipt, err = w.Replace(f.ctx, current.Calendar().Revision(), obs, raw, nil) })
			t.Logf("SQL Replace TotalAlloc=%d reserved=%d image=%+v error=%v observation=%+v", delta, mutation.Admission.ReservedBytes, mutation.Image, err, mutation.Admission)
			if err != nil || !receipt.Valid() {
				t.Fatal("real Store writer refused cost corpus", err)
			}
			if delta > mutation.Admission.ReservedBytes || mutation.Admission.ReservedBytes > parseReservationLimit || mutation.Image.ReservedBytes > uint64(MaxImageBytes) {
				t.Fatal("SQL writer exceeded owned cumulative work/image credit")
			}
			var page Page
			delta = nha101Measure(func() { page, err = f.service.Query(f.ctx, id, window, MaxPageSize, Cursor{}) })
			t.Logf("SQL Query TotalAlloc=%d reserved=%d image=%+v error=%v observation=%+v", delta, read.Admission.ReservedBytes, read.Image, err, read.Admission)
			if err != nil {
				t.Fatal("SQL reader/decode/result copies refused cost corpus", err)
			}
			if _, ok := page.RemoteState(); !ok {
				t.Fatal("real SQL page lost remote state")
			}
			if delta > read.Admission.ReservedBytes || read.Admission.ReservedBytes > parseReservationLimit || read.Image.ReservedBytes > uint64(MaxImageBytes) {
				t.Fatal("SQL reader exceeded owned cumulative work/image credit")
			}
			images.mu.Lock()
			active, bytes := images.active, images.used
			images.mu.Unlock()
			parses.Lock()
			parserActive := parses.active
			parses.Unlock()
			if active != 0 || bytes != 0 || parserActive {
				t.Fatal("SQL operation retained a lease after return")
			}
		})
	}
}
