package storage

import (
	"context"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/historyquery"
)

func TestNHA164RawNanosecondsAndBoundedPhysicalProgress(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch := nha031Epoch(t, 169)
	nha031Open(t, s, epoch)
	at := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Minute)
	entity := "nha164-nanos"
	// The first hundred rows are conservative SQL matches only. The next row
	// regresses the received clock but remains in sequence order and in scope.
	rows := make([]RecorderRecord, 0, 103)
	for seq := uint64(1); seq <= 100; seq++ {
		rows = append(rows, nha032EntityRecord(t, entity, seq, at, at.Add(900*time.Nanosecond), float64(seq), domain.SemanticMeasurement))
	}
	for _, point := range []struct {
		seq   uint64
		nanos time.Duration
	}{{101, 100}, {102, 499}, {103, 500}} {
		rows = append(rows, nha032EntityRecord(t, entity, point.seq, at, at.Add(point.nanos*time.Nanosecond), float64(point.seq), domain.SemanticMeasurement))
	}
	nha032Persist(t, s, epoch, 0, 103, rows, nil, nil)
	q := nha164Query(t, historyquery.Observations, entity, at, at.Add(500*time.Nanosecond), 1)
	first, err := s.ReadHistoricalRecorderPage(ctx, q, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Observations) != 0 || first.Next == nil || first.Next.afterSequence != 100 {
		first.Release()
		t.Fatalf("bounded scan claimed completion or skipped positions: %+v", first)
	}
	raw, err := first.Next.MarshalBinary()
	first.Release()
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := ParseRecorderQueryCursor(raw)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ReadHistoricalRecorderPage(ctx, q, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Observations) != 1 || second.Observations[0].Sequence != 101 || second.Next == nil || second.Next.afterSequence != 101 {
		second.Release()
		t.Fatalf("lower bound or examined cursor changed: %+v", second)
	}
	cursor = second.Next
	second.Release()
	third, err := s.ReadHistoricalRecorderPage(ctx, q, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Observations) != 1 || third.Observations[0].Sequence != 102 || third.Next == nil {
		third.Release()
		t.Fatalf("exclusive upper boundary lost an admissible point: %+v", third)
	}
	cursor = third.Next
	third.Release()
	last, err := s.ReadHistoricalRecorderPage(ctx, q, cursor)
	if err != nil {
		t.Fatal(err)
	}
	last.Release()
	if len(last.Observations) != 0 || last.Next != nil {
		t.Fatal("exclusive upper bound was admitted or did not terminate")
	}
	// A fractional lower bound excludes the earlier exact payload rather than
	// treating its rounded SQL timestamp as record corruption.
	q = nha164Query(t, historyquery.Observations, entity, at.Add(500*time.Nanosecond), at.Add(time.Microsecond), 100)
	page, err := s.ReadHistoricalRecorderPage(ctx, q, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer page.Release()
	if len(page.Observations) != 100 || page.Next == nil {
		t.Fatalf("fractional lower bound failed: %d %v", len(page.Observations), page.Next)
	}
}

func TestNHA164TraceMicrosecondBoundsAndIntersectingLoss(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch := nha186Epoch(t, "00000000000000000000000000000169")
	if _, err := s.OpenExecutionHistoryEpoch(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	batch, err := NewHistoryBatch(epoch, 1, []HistoryRecord{nha186Record(t, epoch, 1, at, 2), nha186Record(t, epoch, 2, at.Add(time.Microsecond), 2)}, []HistoryLoss{{Reason: HistoryLossQueueFull, KnownCount: 7, From: at, Through: at.Add(10 * time.Minute)}, {Reason: HistoryLossContention, KnownCount: 11, From: at.Add(6 * time.Minute), Through: at.Add(8 * time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CommitExecutionHistoryBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		start, end time.Duration
		ordinal    uint64
	}{{0, 500 * time.Nanosecond, 1}, {500 * time.Nanosecond, 1500 * time.Nanosecond, 2}} {
		page, err := s.ReadExecutionHistoryPage(ctx, HistoryPageQuery{Epoch: epoch, Since: at.Add(tc.start), Until: at.Add(tc.end), ClipRetention: true, Limit: 31})
		if err != nil || len(page.Records) != 1 || page.Records[0].Record.ID().Ordinal() != tc.ordinal {
			t.Fatalf("microsecond lattice interval changed: %+v %v", page, err)
		}
	}
	page, err := s.ReadExecutionHistoryPage(ctx, HistoryPageQuery{Epoch: epoch, Since: at.Add(2 * time.Minute), Until: at.Add(5 * time.Minute), ClipRetention: true, Limit: 31})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Losses) != 1 {
		t.Fatalf("overlap lost or later envelope included: %+v", page.Losses)
	}
	loss := page.Losses[0]
	if loss.KnownCount != 7 || !loss.UncertainEnvelope || !loss.From.Equal(at) || !loss.Through.Equal(at.Add(10*time.Minute)) {
		t.Fatalf("clipped envelope/count was invented: %+v", loss)
	}
}

func TestNHA164LiveTailDoesNotAdvancePaginationHighWater(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch := nha031Epoch(t, 170)
	nha031Open(t, s, epoch)
	at := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Minute)
	entity := "nha164-tail-watermark"
	nha032Persist(t, s, epoch, 0, 2, []RecorderRecord{nha032EntityRecord(t, entity, 1, at, at, 1, domain.SemanticMeasurement), nha032EntityRecord(t, entity, 2, at, at, 2, domain.SemanticMeasurement)}, nil, nil)
	live := RecorderLiveHigh{epoch: epoch, sequence: 3}
	q := nha164Query(t, historyquery.Observations, entity, at.Add(-time.Hour), at.Add(time.Hour), 1)
	first, err := s.ReadHistoricalRecorderPageAt(ctx, q, nil, &live)
	if err != nil {
		t.Fatal(err)
	}
	cursor := first.Next
	first.Release()
	if cursor == nil || cursor.highSequence != 2 || cursor.liveSequence != 3 {
		t.Fatal("live queue enlarged the durable point watermark")
	}
	nha032Persist(t, s, epoch, 2, 3, []RecorderRecord{nha032EntityRecord(t, entity, 3, at, at, 3, domain.SemanticMeasurement)}, nil, nil)
	second, err := s.ReadHistoricalRecorderPageAt(ctx, q, cursor, &live)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	_, _, _, through := second.Coverage.Bounds()
	epochs := second.Coverage.Epochs()
	if len(second.Observations) != 1 || second.Observations[0].Sequence != 2 || second.Next != nil || through != 3 || len(epochs) != 1 || epochs[0].Checkpoint() != 2 {
		t.Fatalf("commit changed a frozen page's proven prefix/tail: %+v", second)
	}
	fresh, err := s.ReadHistoricalRecorderPageAt(ctx, nha164Query(t, historyquery.Observations, entity, at.Add(-time.Hour), at.Add(time.Hour), 100), nil, &live)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Release()
	if len(fresh.Observations) != 3 || fresh.Coverage.Epochs()[0].Checkpoint() != 3 {
		t.Fatal("new query did not observe the committed prefix")
	}
}

func TestNHA164FrozenTailPreservesPolicyExcludedPrefix(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch := nha031Epoch(t, 173)
	nha031Open(t, s, epoch)
	at := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Minute)
	entity := "nha164-excluded-prefix"
	nha032Persist(t, s, epoch, 0, 2, []RecorderRecord{nha032EntityRecord(t, entity, 1, at, at, 1, domain.SemanticMeasurement), nha032EntityRecord(t, entity, 2, at, at, 2, domain.SemanticMeasurement)}, nil, nil)
	exclude := func(from, through, count uint64) {
		batch, err := NewRecorderBatch(epoch, from, through, nil, nil, count, nil)
		if err != nil {
			t.Fatal(err)
		}
		if outcome, err := s.PersistRecorderBatch(ctx, batch); err != nil || outcome != TransactionCommitted {
			t.Fatalf("actual policy exclusion commit: %v %v", outcome, err)
		}
	}
	exclude(2, 3, 1)
	live := RecorderLiveHigh{epoch: epoch, sequence: 10}
	q := nha164Query(t, historyquery.Observations, entity, at.Add(-time.Hour), at.Add(time.Hour), 1)
	first, err := s.ReadHistoricalRecorderPageAt(ctx, q, nil, &live)
	if err != nil {
		t.Fatal(err)
	}
	cursor := first.Next
	first.Release()
	if cursor == nil {
		t.Fatal("oracle requires high3/live10 continuation")
	}
	encoded, err := cursor.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	cursor, err = ParseRecorderQueryCursor(encoded)
	if err != nil || cursor.highPolicyExcluded != 1 {
		t.Fatal("prefix exclusion lost by token roundtrip", err)
	}
	exclude(3, 10, 7)
	second, err := s.ReadHistoricalRecorderPageAt(ctx, q, cursor, &live)
	if err != nil {
		t.Fatal(err)
	}
	epochs := second.Coverage.Epochs()
	_, _, _, through := second.Coverage.Bounds()
	second.Release()
	if len(second.Observations) != 1 || second.Observations[0].Sequence != 2 || second.Next != nil || len(epochs) != 1 || epochs[0].Checkpoint() != 3 || epochs[0].PolicyExcluded() != 1 || through != 10 {
		t.Fatalf("current exclusion count escaped frozen prefix: %+v", epochs)
	}
	fresh, err := s.ReadHistoricalRecorderPageAt(ctx, q, nil, &live)
	if err != nil {
		t.Fatal(err)
	}
	epochs = fresh.Coverage.Epochs()
	fresh.Release()
	if len(epochs) != 1 || epochs[0].Checkpoint() != 10 || epochs[0].PolicyExcluded() != 8 {
		t.Fatalf("new query lost real current epoch total: %+v", epochs)
	}
}
