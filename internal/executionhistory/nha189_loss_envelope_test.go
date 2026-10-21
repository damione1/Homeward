package executionhistory

import (
	"runtime"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/storage"
)

func TestNHA189FreezeCoversContentionAfterCapturedBoundary(t *testing.T) {
	sink := nha186Sink(t)
	sink.mu.Lock()
	defer sink.mu.Unlock()

	// Model the worker being preempted after capturing its freeze argument.
	// Both instants come from the real clock; cross a SQL microsecond so the
	// missing coverage remains observable after storage canonicalization.
	t0 := time.Now().UTC()
	observation := nha186Observation()
	deadline := time.Now().Add(time.Second)
	for !observation.At.Truncate(time.Microsecond).After(t0.Truncate(time.Microsecond)) {
		if time.Now().After(deadline) {
			t.Fatal("real clock did not advance past the captured freeze boundary")
		}
		runtime.Gosched()
		observation.At = time.Now().UTC()
	}
	if observation.At.IsZero() || !observation.At.After(t0) || observation.At.After(time.Now().UTC()) {
		t.Fatal("characterization requires a valid, already elapsed emission after t0")
	}
	control := nha186Sink(t)
	control.ObserveCommand(observation)
	if len(control.queue) != 1 || control.hasActiveLoss() {
		t.Fatal("the contention probe must be an otherwise admissible observation")
	}

	sink.ObserveCommand(observation)
	if len(sink.queue) != 0 || sink.nextOrdinal != 0 {
		t.Fatal("observation entered the queue while the real transfer mutex was held")
	}
	for reason := storage.HistoryLossQueueFull; reason <= storage.HistoryLossInvalidMetadata; reason++ {
		var want uint64
		if reason == storage.HistoryLossContention {
			want = 1
		}
		if rejected, active := sink.rejected[reason].Load(), sink.activeLoss[reason].Load(); rejected != want || active != want {
			t.Fatalf("rejection inventory for reason %d: rejected=%d active=%d want=%d", reason, rejected, active, want)
		}
	}
	if !sink.freeze(t0) || !sink.hasPending || !sink.pending.Valid() || sink.pending.LastOrdinal() != 0 || sink.hasActiveLoss() {
		t.Fatal("the sole contention rejection was not frozen into a valid loss-only batch")
	}

	// HistoryBatch intentionally exposes no mutable loss slice. The old batch
	// can still be identified exactly by its public digest; the SQL extension
	// checks the resulting envelope through the public bounded reader.
	uncovered, err := storage.NewHistoryBatch(sink.epoch, 1, nil, []storage.HistoryLoss{{
		Reason: storage.HistoryLossContention, KnownCount: 1,
		From: sink.lastBoundary, Through: t0, UncertainEnvelope: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if sink.pending.Digest() == uncovered.Digest() {
		t.Fatalf("frozen loss ends at %s before its counted emission at %s", t0.Format(time.RFC3339Nano), observation.At.Format(time.RFC3339Nano))
	}
}
