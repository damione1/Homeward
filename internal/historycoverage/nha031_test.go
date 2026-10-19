package historycoverage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

func nha031CoverageStore(t *testing.T) *storage.Store {
	t.Helper()
	cluster := testpg.Start(t)
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(string) (string, bool) { return testpg.Password(), true })
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}
func nha031CoverageEpoch(t *testing.T, value string) storage.RecorderEpoch {
	t.Helper()
	epoch, err := storage.ParseRecorderEpoch(value)
	if err != nil {
		t.Fatal(err)
	}
	return epoch
}
func nha031CoverageOpen(t *testing.T, store *storage.Store, epoch storage.RecorderEpoch) {
	t.Helper()
	if _, outcome, err := store.OpenRecorderEpoch(context.Background(), epoch, time.Now().UTC()); outcome != storage.TransactionCommitted || err != nil {
		t.Fatalf("open: %v %v", outcome, err)
	}
}
func nha031CoverageRead(t *testing.T, store *storage.Store, from storage.RecorderEpoch, start uint64, end storage.RecorderEpoch, through *uint64) storage.RecorderCoverageSnapshot {
	t.Helper()
	request, err := storage.NewRecorderCoverageRequest(from, start, end, through)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.ReadRecorderCoverage(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestNHA031HistoryAndStatisticsCarryIncompleteCoverage(t *testing.T) {
	store := nha031CoverageStore(t)
	first := nha031CoverageEpoch(t, "11111111111111111111111111111111")
	second := nha031CoverageEpoch(t, "22222222222222222222222222222222")
	nha031CoverageOpen(t, store, first)
	one := uint64(1)
	batch, err := storage.NewRecorderBatch(first, 0, 2, nil, []storage.RecorderGap{{From: 1, Through: 2, KnownLost: &one, Reason: storage.RecorderGapOverflow, UncertainEnvelope: true}}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := store.PersistRecorderBatch(context.Background(), batch); outcome != storage.TransactionCommitted || err != nil {
		t.Fatalf("persist: %v %v", outcome, err)
	}
	// A new epoch follows an unclean predecessor; it cannot heal that edge.
	nha031CoverageOpen(t, store, second)
	image, err := storage.NewRecorderBatch(second, 0, 0, nil, nil, 0, &storage.RecorderImage{Boundary: 0})
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := store.PersistRecorderBatch(context.Background(), image); outcome != storage.TransactionCommitted || err != nil {
		t.Fatalf("image: %v %v", outcome, err)
	}
	window, err := NewUTCWindow(time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC), time.Date(2026, 10, 18, 13, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := FromRecorderSnapshot(nha031CoverageRead(t, store, first, 0, second, nil), &window)
	if err != nil || coverage.Verdict() != Incomplete {
		t.Fatalf("coverage: %+v %v", coverage, err)
	}
	issues := coverage.Issues()
	if len(issues) != 2 || issues[0].Kind() != EpochBridge && issues[1].Kind() != EpochBridge {
		t.Fatalf("gap/bridge absent: %+v", issues)
	}
	for _, issue := range issues {
		if issue.TemporalPlacement() != TemporalUnknown {
			t.Fatal("UTC gap placement fabricated")
		}
	}
	var gap Issue
	for _, issue := range issues {
		if issue.Kind() == StoredGap {
			gap = issue
		}
	}
	if count, ok := gap.KnownLost(); !ok || count != 1 {
		t.Fatalf("known loss confused with width: %d %v", count, ok)
	}
	history, err := NewHistoryReadModel(coverage)
	if err != nil {
		t.Fatal(err)
	}
	statistics, err := NewStatisticsReadModel(coverage)
	if err != nil {
		t.Fatal(err)
	}
	issues[0] = Issue{}
	if history.Coverage().Verdict() != Incomplete || statistics.Coverage().Verdict() != Incomplete || len(history.Coverage().Issues()) != 2 || len(statistics.Coverage().Issues()) != 2 {
		t.Fatal("model lost or aliased incomplete coverage")
	}
	// A gap-free suffix entirely inside the later epoch is not poisoned by the
	// previous edge; its UTC placement is still not inferred from received_at.
	clean, err := FromRecorderSnapshot(nha031CoverageRead(t, store, second, 0, second, nil), &window)
	if err != nil || clean.Verdict() != CompleteUnderPolicy || len(clean.Issues()) != 0 {
		t.Fatalf("later epoch suffix: %+v %v", clean, err)
	}
}

func TestNHA031UnknownUTCPlacementAndTailStayConservative(t *testing.T) {
	store := nha031CoverageStore(t)
	epoch := nha031CoverageEpoch(t, "33333333333333333333333333333333")
	nha031CoverageOpen(t, store, epoch)
	pending, err := FromRecorderSnapshot(nha031CoverageRead(t, store, epoch, 0, epoch, nil), nil)
	if err != nil || pending.Verdict() != Incomplete || len(pending.Issues()) != 1 || pending.Issues()[0].Kind() != InitialSnapshotPending {
		t.Fatalf("open epoch without initial image became complete: %+v %v", pending, err)
	}
	one := uint64(1)
	batch, err := storage.NewRecorderBatch(epoch, 0, 2, nil, []storage.RecorderGap{{From: 1, Through: 2, KnownLost: &one, Reason: storage.RecorderGapOverflow, UncertainEnvelope: true}}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := store.PersistRecorderBatch(context.Background(), batch); outcome != storage.TransactionCommitted || err != nil {
		t.Fatal(err)
	}
	through := uint64(3)
	window, err := NewUTCWindow(time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC), time.Date(2026, 10, 18, 13, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := FromRecorderSnapshot(nha031CoverageRead(t, store, epoch, 1, epoch, &through), &window)
	if err != nil || coverage.Verdict() != Incomplete {
		t.Fatalf("clipped gap/tail: %+v %v", coverage, err)
	}
	issues := coverage.Issues()
	if len(issues) != 2 {
		t.Fatalf("missing clipped gap or tail: %+v", issues)
	}
	for _, issue := range issues {
		if issue.TemporalPlacement() != TemporalUnknown {
			t.Fatal("fabricated UTC localization")
		}
		if issue.Kind() == StoredGap {
			if _, ok := issue.KnownLost(); ok {
				t.Fatal("whole-gap loss count apportioned to clipped range")
			}
		}
	}
	if _, err := NewUTCWindow(window.start, window.start); !errors.Is(err, ErrInvalidCoverage) {
		t.Fatalf("empty UTC window accepted: %v", err)
	}
	if _, err := FromRecorderSnapshot(storage.RecorderCoverageSnapshot{}, nil); !errors.Is(err, ErrInvalidCoverage) {
		t.Fatalf("zero snapshot accepted: %v", err)
	}
	request, err := storage.NewRecorderCoverageRequest(epoch, 0, epoch, nil)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	unavailable, err := store.ReadRecorderCoverage(context.Background(), request)
	if !errors.Is(err, storage.ErrRecorderCoverageUnavailable) {
		t.Fatalf("closed pool was not unavailable: %v", err)
	}
	if _, err := FromRecorderSnapshot(unavailable, &window); !errors.Is(err, ErrInvalidCoverage) {
		t.Fatalf("unavailable read became complete coverage: %v", err)
	}
}
