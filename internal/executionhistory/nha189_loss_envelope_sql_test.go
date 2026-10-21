package executionhistory

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

func TestNHA189ContentionEnvelopeSurvivesCommitReadAndRetry(t *testing.T) {
	cluster := testpg.Start(t)
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(string) (string, bool) { return testpg.Password(), true })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	sink, err := New(ctx, store, "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}

	var emittedAt, frozenAt time.Time
	sink.mu.Lock()
	func() {
		defer sink.mu.Unlock()
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
			t.Fatal("SQL characterization requires a valid, already elapsed emission after t0")
		}
		control := nha186Sink(t)
		control.ObserveCommand(observation)
		if len(control.queue) != 1 || control.hasActiveLoss() {
			t.Fatal("the SQL contention probe must be otherwise admissible")
		}
		emittedAt = observation.At.Truncate(time.Microsecond)
		sink.ObserveCommand(observation)
		if !sink.freeze(t0) || !sink.hasPending || !sink.pending.Valid() || sink.pending.LastOrdinal() != 0 || sink.hasActiveLoss() {
			t.Fatal("the contention rejection did not produce a valid loss-only batch")
		}
		frozenAt = time.Now().UTC().Truncate(time.Microsecond)
		pending := sink.pending

		// A later rejection belongs to a future batch. An unresolved pending
		// batch must retain the same identity and digest for resolution/retry.
		observation.At = time.Now().UTC()
		sink.ObserveCommand(observation)
		if sink.freeze(time.Now().UTC()) || sink.pending.Epoch() != pending.Epoch() || sink.pending.Number() != pending.Number() ||
			sink.pending.Digest() != pending.Digest() || len(sink.queue) != 0 || sink.nextOrdinal != 0 {
			t.Fatal("pending batch changed or admitted a contended observation")
		}
		for reason := storage.HistoryLossQueueFull; reason <= storage.HistoryLossInvalidMetadata; reason++ {
			var wantRejected, wantActive uint64
			if reason == storage.HistoryLossContention {
				wantRejected, wantActive = 2, 1
			}
			if rejected, active := sink.rejected[reason].Load(), sink.activeLoss[reason].Load(); rejected != wantRejected || active != wantActive {
				t.Fatalf("rejection inventory for reason %d: rejected=%d active=%d want=%d/%d", reason, rejected, active, wantRejected, wantActive)
			}
		}
	}()
	batch := sink.pending
	if result, err := store.CommitExecutionHistoryBatch(ctx, batch); err != nil || result != storage.HistoryCommitted {
		t.Fatalf("commit frozen batch: result=%v err=%v", result, err)
	}
	if result, err := store.ResolveExecutionHistoryBatch(ctx, batch); err != nil || result != storage.HistoryResolutionCommitted {
		t.Fatalf("resolve frozen batch: result=%v err=%v", result, err)
	}
	if result, err := store.CommitExecutionHistoryBatch(ctx, batch); err != nil || result != storage.HistoryAlreadyCommitted {
		t.Fatalf("retry frozen batch: result=%v err=%v", result, err)
	}
	page, err := store.ReadExecutionHistoryPage(ctx, storage.HistoryPageQuery{Epoch: sink.epoch, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 0 || page.Next != nil || page.Checkpoint != 0 || len(page.Losses) != 1 {
		t.Fatalf("loss-only history invented records or lost its sole proof: %+v", page)
	}
	loss := page.Losses[0]
	if loss.Reason != storage.HistoryLossContention || loss.KnownCount != 1 || !loss.UncertainEnvelope {
		t.Fatalf("counted rejection changed reason/count/envelope across commit and retry: %+v", loss)
	}
	if loss.From.IsZero() || loss.Through.IsZero() || loss.Through.Before(loss.From) ||
		!loss.From.Equal(sink.lastBoundary.Truncate(time.Microsecond)) || loss.From.After(emittedAt) ||
		loss.Through.Before(emittedAt) || loss.Through.After(frozenAt) {
		t.Fatalf("persisted envelope does not cover the real emission %s before freeze returned at %s: %+v", emittedAt.Format(time.RFC3339Nano), frozenAt.Format(time.RFC3339Nano), loss)
	}
	reloaded, err := storage.NewHistoryBatch(batch.Epoch(), batch.Number(), nil, []storage.HistoryLoss{{
		Reason: loss.Reason, KnownCount: loss.KnownCount, From: loss.From,
		Through: loss.Through, UncertainEnvelope: loss.UncertainEnvelope,
	}})
	if err != nil || reloaded.Digest() != batch.Digest() {
		t.Fatalf("persisted envelope differs from the frozen retry digest: err=%v", err)
	}
	if sink.pending.Digest() != batch.Digest() || sink.pending.Number() != batch.Number() || sink.pending.Epoch() != batch.Epoch() {
		t.Fatal("commit/read/retry mutated the pending batch identity")
	}
	t.Logf("durable contention count=1, later active count=1; emission=%s covered by [%s,%s]", emittedAt.Format(time.RFC3339Nano), loss.From.Format(time.RFC3339Nano), loss.Through.Format(time.RFC3339Nano))
}
