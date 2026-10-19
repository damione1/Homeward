package storage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type nha031GapQueryBarrier struct {
	enter, release chan struct{}
	once           sync.Once
}

func (b *nha031GapQueryBarrier) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "FROM public.history_gaps") {
		b.once.Do(func() {
			close(b.enter)
			select {
			case <-b.release:
			case <-ctx.Done():
			}
		})
	}
	return ctx
}
func (*nha031GapQueryBarrier) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func nha031Epoch(t *testing.T, n uint64) RecorderEpoch {
	t.Helper()
	epoch, err := ParseRecorderEpoch(fmt.Sprintf("%032x", n+1))
	if err != nil {
		t.Fatal(err)
	}
	return epoch
}
func nha031Open(t *testing.T, store *Store, epoch RecorderEpoch) {
	t.Helper()
	if _, outcome, err := store.OpenRecorderEpoch(context.Background(), epoch, time.Now().UTC()); outcome != TransactionCommitted || err != nil {
		t.Fatalf("open: %v %v", outcome, err)
	}
}
func nha031Request(t *testing.T, from RecorderEpoch, start uint64, end RecorderEpoch, through *uint64) RecorderCoverageRequest {
	t.Helper()
	r, err := NewRecorderCoverageRequest(from, start, end, through)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func nha031Persist(t *testing.T, store *Store, batch RecorderBatch) {
	t.Helper()
	if outcome, err := store.PersistRecorderBatch(context.Background(), batch); outcome != TransactionCommitted || err != nil {
		t.Fatalf("persist: %v %v", outcome, err)
	}
}

func TestNHA031CoverageReadSnapshotAndUint64(t *testing.T) {
	store := nha030RecorderSQLFixture(t)
	epoch := nha031Epoch(t, 1)
	nha031Open(t, store, epoch)
	const high = uint64(math.MaxInt64) + 7
	one := uint64(1)
	batch, err := NewRecorderBatch(epoch, 0, high, []RecorderRecord{nha030Record(high, "state-high")}, []RecorderGap{{From: 1, Through: high, KnownLost: &one, Reason: RecorderGapOverflow, UncertainEnvelope: true}}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.ReadRecorderCoverage(context.Background(), nha031Request(t, epoch, 0, epoch, nil))
	if err != nil || before.Kind() != RecorderCoverageReadComplete || len(before.Gaps()) != 0 {
		t.Fatalf("precommit read: %#v %v", before, err)
	}
	nha031Persist(t, store, batch)
	after, err := store.ReadRecorderCoverage(context.Background(), nha031Request(t, epoch, 0, epoch, nil))
	if err != nil || after.Kind() != RecorderCoverageReadComplete {
		t.Fatalf("postcommit read: %#v %v", after, err)
	}
	_, _, _, through := after.Bounds()
	gaps := after.Gaps()
	if through != high || len(gaps) != 1 {
		t.Fatalf("gap and checkpoint separated: %d %+v", through, gaps)
	}
	if from, end := gaps[0].Bounds(); from != 1 || end != high {
		t.Fatalf("lost uint64 gap: %d %d", from, end)
	}
	if count, ok := gaps[0].KnownLost(); !ok || count != 1 {
		t.Fatalf("gap width became loss count: %d %v", count, ok)
	}
	gaps[0] = RecorderCoverageGap{}
	if len(after.Gaps()) != 1 || after.Gaps()[0].Reason() != RecorderGapOverflow {
		t.Fatal("returned gap alias mutates snapshot")
	}
	_, err = store.ReadRecorderCoverage(context.Background(), nha031Request(t, epoch, high, epoch, nil))
	if err != nil {
		t.Fatalf("empty suffix should be valid: %v", err)
	}
	tooFar := high + 1
	tail, err := store.ReadRecorderCoverage(context.Background(), nha031Request(t, epoch, high, epoch, &tooFar))
	if err != nil || tail.Kind() != RecorderCoverageReadComplete {
		t.Fatalf("uncommitted tail should be projected: %#v %v", tail, err)
	}
}

func TestNHA031RepeatableReadKeepsEpochAndGapsInOneSnapshot(t *testing.T) {
	store := nha030RecorderSQLFixture(t)
	epoch := nha031Epoch(t, 300)
	nha031Open(t, store, epoch)
	image, err := NewRecorderBatch(epoch, 0, 0, nil, nil, 0, &RecorderImage{Boundary: 0})
	if err != nil {
		t.Fatal(err)
	}
	nha031Persist(t, store, image)
	one := uint64(1)
	gap, err := NewRecorderBatch(epoch, 0, 1, nil, []RecorderGap{{From: 1, Through: 1, KnownLost: &one, Reason: RecorderGapOverflow}}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	barrier := &nha031GapQueryBarrier{enter: make(chan struct{}), release: make(chan struct{})}
	readerConfig := store.pool.Config()
	readerConfig.ConnConfig.Tracer = barrier
	readerPool, err := pgxpool.NewWithConfig(context.Background(), readerConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer readerPool.Close()
	releaseOnce := sync.Once{}
	defer releaseOnce.Do(func() { close(barrier.release) })
	reader := &Store{pool: readerPool} // isolated traced pool; writer keeps its own pool
	request := nha031Request(t, epoch, 0, epoch, &one)
	type readResult struct {
		snapshot RecorderCoverageSnapshot
		err      error
	}
	finished := make(chan readResult, 1)
	go func() {
		snapshot, err := reader.ReadRecorderCoverage(context.Background(), request)
		finished <- readResult{snapshot, err}
	}()
	select {
	case <-barrier.enter:
	case <-time.After(3 * time.Second):
		t.Fatal("coverage reader never reached gap query")
	}
	// The first SELECT established the RR snapshot. Commit a gap+watermark
	// before the second SELECT of this exact ReadRecorderCoverage invocation.
	nha031Persist(t, store, gap)
	releaseOnce.Do(func() { close(barrier.release) })
	select {
	case first := <-finished:
		if first.err != nil || first.snapshot.Kind() != RecorderCoverageReadComplete || len(first.snapshot.Gaps()) != 0 {
			t.Fatalf("mixed RR snapshot: %#v %v", first.snapshot, first.err)
		}
		if got := first.snapshot.Epochs()[0].Checkpoint(); got != 0 {
			t.Fatalf("first SELECT checkpoint was spliced with later COMMIT: %d", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("coverage reader did not resume")
	}
	second, err := reader.ReadRecorderCoverage(context.Background(), request)
	if err != nil || second.Kind() != RecorderCoverageReadComplete || len(second.Gaps()) != 1 {
		t.Fatalf("next RR snapshot missed committed pair: %#v %v", second, err)
	}
	if got := second.Epochs()[0].Checkpoint(); got != 1 {
		t.Fatalf("next checkpoint: %d", got)
	}
}

func TestNHA031CrashEdgePolicyOnlyAndEmptyEpoch(t *testing.T) {
	store := nha030RecorderSQLFixture(t)
	first, second, third := nha031Epoch(t, 2), nha031Epoch(t, 3), nha031Epoch(t, 4)
	nha031Open(t, store, first)
	policy, err := NewRecorderBatch(first, 0, 2, nil, nil, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	nha031Persist(t, store, policy)
	// No CloseRecorderEpoch: only durable checkpoint and previous_epoch survive.
	nha031Open(t, store, second)
	image, err := NewRecorderBatch(second, 0, 0, nil, nil, 0, &RecorderImage{Boundary: 0})
	if err != nil {
		t.Fatal(err)
	}
	nha031Persist(t, store, image)
	nha031Open(t, store, third)
	snapshot, err := store.ReadRecorderCoverage(context.Background(), nha031Request(t, first, 2, third, nil))
	if err != nil || snapshot.Kind() != RecorderCoverageReadComplete {
		t.Fatalf("read crash chain: %#v %v", snapshot, err)
	}
	runs := snapshot.Epochs()
	if len(runs) != 3 || runs[0].PolicyExcluded() != 2 || runs[0].Checkpoint() != 2 || runs[1].Checkpoint() != 0 || runs[2].Checkpoint() != 0 {
		t.Fatalf("empty/policy runs lost: %+v", runs)
	}
	prior, checkpoint, clean, ok := runs[1].Previous()
	if !ok || prior != first || checkpoint != 2 || clean {
		t.Fatalf("unclean durable edge lost: %v %d %v %v", prior, checkpoint, clean, ok)
	}
	if _, ok := runs[1].ClosedAt(); ok {
		t.Fatal("crash invented close time")
	}
}

func TestNHA031CoverageBudgetsTruncateBeforeCopies(t *testing.T) {
	store := nha030RecorderSQLFixture(t)
	epoch := nha031Epoch(t, 10)
	nha031Open(t, store, epoch)
	one := uint64(1)
	batch, err := NewRecorderBatch(epoch, 0, 1, nil, []RecorderGap{{From: 1, Through: 1, KnownLost: &one, Reason: RecorderGapOverflow}}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	nha031Persist(t, store, batch)
	request := nha031Request(t, epoch, 0, epoch, nil)
	limits := recorderCoverageProductionLimits
	limits.bytes = 1
	result, err := store.readRecorderCoverageWithLimits(context.Background(), request, limits)
	if err != nil || result.Kind() != RecorderCoverageReadTruncated || result.Truncation() != RecorderCoverageByteLimit || len(result.Epochs()) != 0 {
		t.Fatalf("byte cap: %#v %v", result, err)
	}
	limits = recorderCoverageProductionLimits
	limits.gaps = 1
	_, err = store.pool.Exec(context.Background(), `INSERT INTO public.history_gaps(epoch,from_sequence,through_sequence,reason,known_lost_count,uncertain_envelope) VALUES ($1,1,1,2,1,false)`, epoch.data())
	if err != nil {
		t.Fatal(err)
	}
	result, err = store.readRecorderCoverageWithLimits(context.Background(), request, limits)
	if err != nil || result.Truncation() != RecorderCoverageGapLimit || len(result.Gaps()) != 0 {
		t.Fatalf("gap cap: %#v %v", result, err)
	}
	limits = recorderCoverageProductionLimits
	limits.timeout = time.Nanosecond
	result, err = store.readRecorderCoverageWithLimits(context.Background(), request, limits)
	if err != nil || result.Truncation() != RecorderCoverageDurationLimit {
		t.Fatalf("duration cap: %#v %v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.ReadRecorderCoverage(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation collapsed: %v", err)
	}
	// Epoch limit uses a real sequence of linked runs, independent of gaps.
	var last RecorderEpoch
	for i := uint64(0); i < RecorderCoverageMaxEpochs; i++ {
		last = nha031Epoch(t, 100+i)
		nha031Open(t, store, last)
	}
	limits = recorderCoverageProductionLimits
	limits.epochs = 1
	result, err = store.readRecorderCoverageWithLimits(context.Background(), nha031Request(t, epoch, 0, last, nil), limits)
	if err != nil || result.Truncation() != RecorderCoverageEpochLimit {
		t.Fatalf("epoch cap: %#v %v", result, err)
	}
	result, err = store.ReadRecorderCoverage(context.Background(), nha031Request(t, epoch, 0, last, nil))
	if err != nil || result.Truncation() != RecorderCoverageEpochLimit {
		t.Fatalf("production 65th epoch cap: %#v %v", result, err)
	}
	gapEpoch := nha031Epoch(t, 500)
	nha031Open(t, store, gapEpoch)
	if _, err := store.pool.Exec(context.Background(), `UPDATE public.recorder_epochs SET checkpoint=513 WHERE epoch=$1`, gapEpoch.data()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(context.Background(), `INSERT INTO public.history_gaps(epoch,from_sequence,through_sequence,reason,known_lost_count,uncertain_envelope) SELECT $1,g,g,1,1,false FROM generate_series(1,513) AS g`, gapEpoch.data()); err != nil {
		t.Fatal(err)
	}
	result, err = store.ReadRecorderCoverage(context.Background(), nha031Request(t, gapEpoch, 0, gapEpoch, nil))
	if err != nil || result.Truncation() != RecorderCoverageGapLimit || len(result.Gaps()) != 0 {
		t.Fatalf("production 513th gap cap: %#v %v", result, err)
	}
}

func TestNHA031CoverageRejectsCorruptRowsAndUnavailableStore(t *testing.T) {
	store := nha030RecorderSQLFixture(t)
	epoch := nha031Epoch(t, 200)
	nha031Open(t, store, epoch)
	request := nha031Request(t, epoch, 0, epoch, nil)
	if _, err := (&Store{}).ReadRecorderCoverage(context.Background(), request); !errors.Is(err, ErrRecorderCoverageInvalid) {
		t.Fatalf("nil pool: %v", err)
	}
	if _, err := store.ReadRecorderCoverage(context.Background(), RecorderCoverageRequest{}); !errors.Is(err, ErrRecorderCoverageInvalid) {
		t.Fatalf("zero request: %v", err)
	}
	_, err := store.pool.Exec(context.Background(), `INSERT INTO public.history_gaps(epoch,from_sequence,through_sequence,reason,known_lost_count,uncertain_envelope) VALUES ($1,1,1,1,1,false)`, epoch.data())
	if err != nil {
		t.Fatal(err)
	}
	one := uint64(1)
	_, err = store.ReadRecorderCoverage(context.Background(), nha031Request(t, epoch, 0, epoch, &one))
	if !errors.Is(err, ErrRecorderCoverageIntegrity) {
		t.Fatalf("gap past checkpoint accepted: %v", err)
	}
	// A transaction held open before commit exposes neither a checkpoint nor a gap.
	if _, err := store.pool.Exec(context.Background(), `DELETE FROM public.history_gaps WHERE epoch=$1`, epoch.data()); err != nil {
		t.Fatal(err)
	}
	tx, err := store.pool.BeginTx(context.Background(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(context.Background(), `UPDATE public.recorder_epochs SET checkpoint=1 WHERE epoch=$1`, epoch.data()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `INSERT INTO public.history_gaps(epoch,from_sequence,through_sequence,reason,known_lost_count,uncertain_envelope) VALUES ($1,1,1,1,1,false)`, epoch.data()); err != nil {
		t.Fatal(err)
	}
	before, err := store.ReadRecorderCoverage(context.Background(), request)
	if err != nil || len(before.Gaps()) != 0 {
		t.Fatalf("uncommitted rows visible: %#v %v", before, err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := store.ReadRecorderCoverage(context.Background(), request)
	if err != nil || len(after.Gaps()) != 1 {
		t.Fatalf("committed gap missing: %#v %v", after, err)
	}
	store.Close()
	unavailable, err := store.ReadRecorderCoverage(context.Background(), request)
	if !errors.Is(err, ErrRecorderCoverageUnavailable) || unavailable.Kind() != RecorderCoverageReadUnspecified || len(unavailable.Gaps()) != 0 {
		t.Fatalf("closed PG pool became empty complete coverage: %#v %v", unavailable, err)
	}
}
