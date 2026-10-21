package storage

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/recordermaintenance"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNHA032FiveHundredSeriesHaveBoundedPrefixAndTailContinuation(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch := nha031Epoch(t, 326)
	nha031Open(t, s, epoch)
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	// One more than the admitted prefix makes an unbounded scan or a lost
	// keyset continuation observable. Every entity uses the production writer.
	rows := make([]RecorderRecord, 0, recordermaintenance.MaxPrefixRows)
	for i := 1; i <= recordermaintenance.MaxPrefixRows; i++ {
		rows = append(rows, nha032EntityRecord(t, fmt.Sprintf("nha032-series-%03d", i), uint64(i), at, at, float64(i), domain.SemanticMeasurement))
	}
	nha032Persist(t, s, epoch, 0, 500, rows, nil, nil)
	last := nha032EntityRecord(t, "nha032-series-501", 501, at, at, 501, domain.SemanticMeasurement)
	nha032Persist(t, s, epoch, 500, 501, []RecorderRecord{last}, nil, nil)
	b := nha032Boundary(t, epoch, 501, at)
	processed, continued := nha032DrainBoundedPrefixes(t, s, b)
	if processed != 501 || !continued {
		t.Fatalf("prefix lost continuation: processed=%d continued=%v", processed, continued)
	}
	b = nha032Boundary(t, epoch, 501, at.Add(time.Hour))
	processed, continued = nha032DrainBoundedPrefixes(t, s, b)
	if processed != 0 || !continued {
		t.Fatalf("suffix replayed inputs or lost keyset: %d %v", processed, continued)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM public.recorder_hourly_aggregates WHERE duration_ns=3600000000000 AND reasons=0`).Scan(&count); err != nil || count != 501 {
		t.Fatalf("lost or duplicated series: %d %v", count, err)
	}
	processed, continued = nha032DrainBoundedPrefixes(t, s, b)
	if processed != 0 || !continued {
		t.Fatalf("idempotent keyset scan: %d %v", processed, continued)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM public.recorder_hourly_aggregates WHERE duration_ns=3600000000000`).Scan(&count); err != nil || count != 501 {
		t.Fatalf("replayed suffix duration: %d %v", count, err)
	}

}

func nha032AssertEntityMean(t *testing.T, s *Store, entity string, at time.Time, want float64) {
	t.Helper()
	rows, err := s.ReadRecorderHourlyAggregates(context.Background(), entity, at, at.Add(time.Hour))
	if err != nil || len(rows) != 1 {
		t.Fatalf("mean rows %+v %v", rows, err)
	}
	if mean, present := rows[0].Values.Mean(); !present || mean != want || rows[0].Values.Input().Duration != time.Hour || rows[0].Values.Input().Reasons != 0 {
		t.Fatalf("mean changed or duplicated: %+v", rows[0])
	}
}

func TestNHA032ExtremePrefixAndPersistedTailDoNotStarveFollowingSeries(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch := nha031Epoch(t, 329)
	nha031Open(t, s, epoch)
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	rows := []RecorderRecord{
		nha032EntityRecord(t, "nha032-extreme", 1, at, at, 1e306, domain.SemanticMeasurement),
		nha032EntityRecord(t, "nha032-extreme", 2, at.Add(time.Hour), at.Add(time.Hour), 1e306, domain.SemanticMeasurement),
		nha032EntityRecord(t, "nha032-after-extreme", 3, at, at, 10, domain.SemanticMeasurement),
		nha032EntityRecord(t, "nha032-after-extreme", 4, at.Add(time.Hour), at.Add(time.Hour), 30, domain.SemanticMeasurement),
	}
	batch := nha032Persist(t, s, epoch, 0, 4, rows, nil, nil)
	if n, more, err := s.AggregateRecorderPrefix(ctx, nha032Boundary(t, epoch, 4, at.Add(time.Hour))); err != nil || n != 4 || more {
		t.Fatalf("extreme poisoned prefix: %d %v %v", n, more, err)
	}
	nha032AssertEntityMean(t, s, "nha032-after-extreme", at, 10)
	if n, more, err := s.PurgeRecorderRaw(ctx, at.Add(2*time.Hour)); err != nil || n != 4 || more {
		t.Fatalf("extreme raw purge: %d %v %v", n, more, err)
	}
	boundary := nha032Boundary(t, epoch, 4, at.Add(2*time.Hour))
	for range 2 {
		if n, more, err := s.AggregateRecorderPrefix(ctx, boundary); err != nil || n != 0 || more {
			t.Fatalf("persisted extreme poisoned tail: %d %v %v", n, more, err)
		}
		nha032AssertEntityMean(t, s, "nha032-after-extreme", at.Add(time.Hour), 30)
	}
	refused, err := s.ReadRecorderHourlyAggregates(ctx, "nha032-extreme", at, at.Add(2*time.Hour))
	if err != nil || len(refused) != 2 {
		t.Fatalf("extreme refusal lost: %+v %v", refused, err)
	}
	for i, row := range refused {
		// The first refusal was actually computed before purge. The later
		// suffix is already unproven through retention and skips arithmetic.
		want := recordermaintenance.UnsupportedNumeric
		if i == 1 {
			want = recordermaintenance.RetentionPruned
		}
		if row.Values.Input().Reasons&want == 0 || row.Values.Input().HasMeasurement || row.Values.Input().HasContribution {
			t.Fatalf("extreme fabricated numeric value or lost actual cause: %+v", row)
		}
	}
	var cursor string
	if err = s.pool.QueryRow(ctx, `SELECT analytic_sequence::text FROM public.recorder_maintenance_head WHERE singleton=true`).Scan(&cursor); err != nil || cursor != "4" {
		t.Fatalf("cursor failed to advance atomically: %s %v", cursor, err)
	}
	if checkpoint, err := s.RecorderCheckpoint(ctx, epoch); err != nil || !checkpoint.Confirms(batch) {
		t.Fatalf("source checkpoint changed: %+v %v", checkpoint, err)
	}
}

func nha032LongUnitRecord(t *testing.T, seq uint64, admitted, received time.Time) RecorderRecord {
	t.Helper()
	catalog := domain.NewUnitCatalog()
	symbol := strings.Repeat("u", 257)
	if err := catalog.Register(symbol, domain.DimensionPower, 1, 0); err != nil {
		t.Fatal(err)
	}
	unit, err := catalog.Parse(symbol)
	if err != nil {
		t.Fatal(err)
	}
	measurement, err := domain.NewMeasurement(12, unit, 2, domain.SemanticMeasurement, domain.AggregationCurrent)
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.NewMeasurementValue(measurement)
	if err != nil {
		t.Fatal(err)
	}
	entity, _ := domain.ParseEntityID("nha032-long-unit")
	instance, _ := domain.ParseIntegrationInstanceID("nha032-fake")
	source, _ := domain.NewSourceStamp("nha032-source", nil)
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	state, err := domain.NewObservedStateSnapshot(entity, quality, &value, domain.StateTimes{ReceivedAt: received, LastChanged: received, LastUpdated: received, LastReported: received}, source, seq, nil)
	if err != nil {
		t.Fatal(err)
	}
	row, err := NewRecorderRecord(seq, instance, state, domain.Causality{}, admitted, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func TestNHA032AdmittedLongMetadataCannotStarveAggregationOrRawPurge(t *testing.T) {
	for _, aggregateFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("aggregate_first_%v", aggregateFirst), func(t *testing.T) {
			s := nha032SQLFixture(t)
			ctx := context.Background()
			epoch := nha031Epoch(t, 330)
			nha031Open(t, s, epoch)
			at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
			longID := strings.Repeat("é", 512) + "a"
			if len(longID) != 1025 {
				t.Fatal("fixture lost UTF-8 byte boundary")
			}
			old := at.Add(-8 * 24 * time.Hour)
			batch := nha032Persist(t, s, epoch, 0, 4, []RecorderRecord{
				nha032EntityRecord(t, longID, 1, at, old, 10, domain.SemanticMeasurement),
				nha032LongUnitRecord(t, 2, at, old),
				nha032EntityRecord(t, "nha032-after-metadata", 3, at, at, 10, domain.SemanticMeasurement),
				nha032EntityRecord(t, "nha032-after-metadata", 4, at.Add(time.Hour), at.Add(time.Hour), 30, domain.SemanticMeasurement),
			}, nil, nil)
			var entityBytes, unitBytes int
			if err := s.pool.QueryRow(ctx, `SELECT (SELECT octet_length(entity_id) FROM public.state_history WHERE epoch=$1 AND sequence=1),(SELECT octet_length(measurement_unit) FROM public.state_history WHERE epoch=$1 AND sequence=2)`, epoch.data()).Scan(&entityBytes, &unitBytes); err != nil || entityBytes != 1025 || unitBytes != 257 {
				t.Fatalf("writer changed admitted metadata: %d %d %v", entityBytes, unitBytes, err)
			}
			for _, sequence := range []string{"1", "2"} {
				compact, err := scanNumericRow(s.pool.QueryRow(ctx, `SELECT `+recorderNumericColumns+` FROM public.state_history h JOIN public.recorder_epochs e ON e.epoch=h.epoch WHERE h.epoch=$1 AND h.sequence=$2::numeric`, epoch.data(), sequence))
				if err != nil || !compact.refused || compact.entity != "" || compact.point.Input().Numeric != nil {
					t.Fatalf("SQL projection copied or rejected oversized historical metadata: %+v %v", compact, err)
				}
			}
			boundary := nha032Boundary(t, epoch, 4, at.Add(time.Hour))
			if aggregateFirst {
				if n, more, err := s.AggregateRecorderPrefix(ctx, boundary); err != nil || n != 4 || more {
					t.Fatalf("metadata poisoned prefix: %d %v %v", n, more, err)
				}
				var refused int
				if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM public.recorder_numeric_continuations WHERE analytical_refusal AND entity_id IS NULL AND octet_length(point::text)<=4096 AND (point->>'Sequence')::numeric IN (1,2)`).Scan(&refused); err != nil || refused != 2 {
					t.Fatalf("compact refusal continuation/positions missing: %d %v", refused, err)
				}
			}
			if n, more, err := s.PurgeRecorderRaw(ctx, at.Add(-7*24*time.Hour)); err != nil || n != 2 || more {
				t.Fatalf("admitted metadata made raw unpurgeable: %d %v %v", n, more, err)
			}
			want := int64(2)
			if aggregateFirst {
				want = 0
			}
			if n, more, err := s.AggregateRecorderPrefix(ctx, boundary); err != nil || n != want || more {
				t.Fatalf("following series lost prefix: %d %v %v", n, more, err)
			}
			nha032AssertEntityMean(t, s, "nha032-after-metadata", at, 10)
			through := uint64(4)
			proof, err := s.ReadRecorderCoverage(ctx, nha031Request(t, epoch, 0, epoch, &through))
			if err != nil || len(proof.Retention()) == 0 {
				t.Fatalf("raw payload deleted without logical retention proof: %+v %v", proof, err)
			}
			for _, entity := range []string{longID, "nha032-long-unit"} {
				breaks, err := s.ReadRecorderAnalyticalBreaks(ctx, entity, at, at.Add(time.Hour))
				if err != nil || len(breaks) == 0 {
					t.Fatalf("metadata refusal lost digest/coverage: %+v %v", breaks, err)
				}
				for _, b := range breaks {
					if b.Reasons()&recordermaintenance.UnsupportedNumeric == 0 {
						t.Fatalf("refusal reason lost: %+v", b)
					}
				}
			}
			for range 2 {
				if n, more, err := s.AggregateRecorderPrefix(ctx, nha032Boundary(t, epoch, 4, at.Add(2*time.Hour))); err != nil || n != 0 || more {
					t.Fatalf("metadata continuation poisoned restart: %d %v %v", n, more, err)
				}
				nha032AssertEntityMean(t, s, "nha032-after-metadata", at.Add(time.Hour), 30)
			}
			if checkpoint, err := s.RecorderCheckpoint(ctx, epoch); err != nil || !checkpoint.Confirms(batch) {
				t.Fatalf("metadata checkpoint/digest lost: %+v %v", checkpoint, err)
			}
			var raw int
			if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM public.state_history`).Scan(&raw); err != nil || raw != 2 {
				t.Fatalf("wrong raw class deleted: %d %v", raw, err)
			}
			// A later representable descriptor replaces the explicit refusal
			// baseline without bridging the refused segment or poisoning clean hours.
			recovered := nha032Persist(t, s, epoch, 4, 7, []RecorderRecord{
				nha032EntityRecord(t, "nha032-long-unit", 5, at.Add(2*time.Hour), at.Add(2*time.Hour), 20, domain.SemanticMeasurement),
				nha032EntityRecord(t, "nha032-long-unit", 6, at.Add(3*time.Hour), at.Add(3*time.Hour), 30, domain.SemanticMeasurement),
				nha032EntityRecord(t, "nha032-long-unit", 7, at.Add(4*time.Hour), at.Add(4*time.Hour), 40, domain.SemanticMeasurement),
			}, nil, nil)
			for attempt := 0; attempt < 2; attempt++ {
				want := int64(3)
				if attempt == 1 {
					want = 0
				}
				if n, more, err := s.AggregateRecorderPrefix(ctx, nha032Boundary(t, epoch, 7, at.Add(4*time.Hour))); err != nil || n != want || more {
					t.Fatalf("representable descriptor did not recover: %d %v %v", n, more, err)
				}
				nha032AssertEntityMean(t, s, "nha032-long-unit", at.Add(3*time.Hour), 30)
			}
			if checkpoint, err := s.RecorderCheckpoint(ctx, epoch); err != nil || !checkpoint.Confirms(recovered) {
				t.Fatalf("recovered checkpoint/digest lost: %+v %v", checkpoint, err)
			}
		})
	}
}

func TestNHA032MetadataByteBudgetPrecedesHashAndCommitsProgress(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch := nha031Epoch(t, 331)
	nha031Open(t, s, epoch)
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for sequence := uint64(1); sequence <= 5; sequence++ {
		// Each row fits the real writer4MiB budget; their historical prefix
		// exceeds it before500 rows. Do not send big identities to the owner.
		name := strings.Repeat("x", 1<<20) + fmt.Sprintf("-%d", sequence)
		row := nha032EntityRecord(t, name, sequence, at, at.Add(-8*24*time.Hour), 10, domain.SemanticMeasurement)
		nha032Persist(t, s, epoch, sequence-1, sequence, []RecorderRecord{row}, nil, nil)
	}
	nha032Persist(t, s, epoch, 5, 7, []RecorderRecord{
		nha032EntityRecord(t, "nha032-after-byte-budget", 6, at, at, 10, domain.SemanticMeasurement),
		nha032EntityRecord(t, "nha032-after-byte-budget", 7, at.Add(time.Hour), at.Add(time.Hour), 30, domain.SemanticMeasurement),
	}, nil, nil)
	boundary := nha032Boundary(t, epoch, 7, at.Add(time.Hour))
	n, more, err := s.AggregateRecorderPrefix(ctx, boundary)
	if err != nil || n < 1 || n > 3 || !more {
		t.Fatalf("large metadata hashed beyond the input prefix or made no progress: %d %v %v", n, more, err)
	}
	rest, _ := nha032DrainBoundedPrefixes(t, s, boundary)
	if n+rest != 7 {
		t.Fatalf("byte-limited prefix lost positions: %d+%d", n, rest)
	}
	nha032AssertEntityMean(t, s, "nha032-after-byte-budget", at, 10)
	deleted, more, err := s.PurgeRecorderRaw(ctx, at.Add(-7*24*time.Hour))
	if err != nil || deleted < 1 || deleted > 3 || !more {
		t.Fatalf("large raw purge ignored byte budget or did not advance: %d %v %v", deleted, more, err)
	}
	for attempt := 0; more && attempt < 5; attempt++ {
		n, next, err := s.PurgeRecorderRaw(ctx, at.Add(-7*24*time.Hour))
		if err != nil || n < 1 || n > 3 {
			t.Fatalf("raw byte continuation: %d %v %v", n, next, err)
		}
		deleted += n
		more = next
	}
	if more || deleted != 5 {
		t.Fatalf("raw byte continuation lost rows: %d %v", deleted, more)
	}
	if n, more, err := s.AggregateRecorderPrefix(ctx, boundary); err != nil || n != 0 || more {
		t.Fatalf("byte-prefix retry duplicated positions: %d %v %v", n, more, err)
	}
	nha032AssertEntityMean(t, s, "nha032-after-byte-budget", at, 10)
}

func TestNHA032LongEdgeRefusesUnboundedBucketsAndResumesCleanScope(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch := nha031Epoch(t, 327)
	nha031Open(t, s, epoch)
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	right := at.Add(501*time.Hour + time.Minute)
	a := nha032Record(t, 1, at, at, 10, domain.SemanticMeasurement)
	b := nha032Record(t, 2, right, right, 20, domain.SemanticMeasurement)
	nha032Persist(t, s, epoch, 0, 2, []RecorderRecord{a, b}, nil, nil)
	n, more, err := s.AggregateRecorderPrefix(ctx, nha032Boundary(t, epoch, 2, right))
	if err != nil || n != 2 || more {
		t.Fatalf("long edge %d %v %v", n, more, err)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM public.recorder_hourly_aggregates`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("long edge allocated buckets: %d %v", count, err)
	}
	refused, err := s.ReadRecorderHourlyAggregates(ctx, "nha032-measurement", right.Truncate(time.Hour), right.Truncate(time.Hour).Add(time.Hour))
	if err != nil || len(refused) != 1 || refused[0].Values.Input().HasMeasurement || refused[0].Values.Input().Reasons&recordermaintenance.BudgetLimit == 0 {
		t.Fatalf("silent long-edge loss %+v %v", refused, err)
	}
	breaks, err := s.ReadRecorderAnalyticalBreaks(ctx, "nha032-measurement", at.Add(time.Hour), at.Add(2*time.Hour))
	if err != nil || len(breaks) != 1 || breaks[0].Reasons()&recordermaintenance.BudgetLimit == 0 {
		t.Fatalf("empty middle window lost refusal %+v %v", breaks, err)
	}
	cAt := right.Add(2 * time.Hour)
	c := nha032Record(t, 3, cAt, cAt, 30, domain.SemanticMeasurement)
	nha032Persist(t, s, epoch, 2, 3, []RecorderRecord{c}, nil, nil)
	if _, _, err = s.AggregateRecorderPrefix(ctx, nha032Boundary(t, epoch, 3, cAt)); err != nil {
		t.Fatal(err)
	}
	cleanStart := right.Truncate(time.Hour).Add(time.Hour)
	clean, err := s.ReadRecorderHourlyAggregates(ctx, "nha032-measurement", cleanStart, cleanStart.Add(time.Hour))
	if err != nil || len(clean) != 1 {
		t.Fatalf("clean continuation missing %+v %v", clean, err)
	}
	if mean, ok := clean[0].Values.Mean(); !ok || mean != 20 || clean[0].Values.Input().Duration != time.Hour || clean[0].Values.Input().Reasons != 0 {
		t.Fatalf("old budget issue poisoned clean scope %+v", clean[0])
	}
}

// A slow PostgreSQL may close the prefix before 500 rows to preserve the same
// two-second COMMIT budget. Observe progress and the actual row bound rather
// than presuming that 500 sequential SQL operations always fit that budget.
func nha032DrainBoundedPrefixes(t *testing.T, s *Store, b recordermaintenance.Boundary) (int64, bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var processed int64
	continued := false
	for attempt := 0; attempt <= 501 && time.Now().Before(deadline); attempt++ {
		var before, after int
		if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM public.recorder_hourly_aggregates`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		n, more, err := s.AggregateRecorderPrefix(context.Background(), b)
		if err != nil || n < 0 || n > recordermaintenance.MaxPrefixRows {
			t.Fatalf("bounded prefix %d %v %v", n, more, err)
		}
		if err = s.pool.QueryRow(context.Background(), `SELECT count(*) FROM public.recorder_hourly_aggregates`).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after-before > recordermaintenance.MaxPrefixRows {
			t.Fatalf("unbounded bucket materialization %d", after-before)
		}
		processed += n
		continued = continued || more
		if !more {
			return processed, continued
		}
	}
	t.Fatal("bounded prefix did not make resumable progress within the test deadline")
	return processed, continued
}
func TestNHA032RawPurgeMakesBoundedProgressAcross501Rows(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch := nha031Epoch(t, 328)
	nha031Open(t, s, epoch)
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	received := at.Add(-8 * 24 * time.Hour)
	rows := make([]RecorderRecord, 0, 500)
	for i := 1; i <= 500; i++ {
		rows = append(rows, nha032EntityRecord(t, fmt.Sprintf("nha032-expired-%03d", i), uint64(i), at, received, float64(i), domain.SemanticMeasurement))
	}
	nha032Persist(t, s, epoch, 0, 500, rows, nil, nil)
	last := nha032EntityRecord(t, "nha032-expired-501", 501, at, received, 501, domain.SemanticMeasurement)
	nha032Persist(t, s, epoch, 500, 501, []RecorderRecord{last}, nil, nil)
	var deleted int64
	continued := false
	deadline := time.Now().Add(30 * time.Second)
	for attempt := 0; attempt <= 501 && time.Now().Before(deadline); attempt++ {
		n, more, err := s.PurgeRecorderRaw(ctx, at.Add(-7*24*time.Hour))
		if err != nil || n <= 0 || n > 500 {
			t.Fatalf("raw prefix %d %v %v", n, more, err)
		}
		deleted += n
		continued = continued || more
		if !more {
			break
		}
	}
	if deleted != 501 || !continued {
		t.Fatalf("raw continuation lost rows: %d %v", deleted, continued)
	}
	n, more, err := s.PurgeRecorderRaw(ctx, at.Add(-7*24*time.Hour))
	if err != nil || n != 0 || more {
		t.Fatalf("raw replay changed data: %d %v %v", n, more, err)
	}
	checkpoint, err := s.RecorderCheckpoint(ctx, epoch)
	if err != nil || checkpoint.Sequence != 501 {
		t.Fatalf("purge changed recorder checkpoint: %+v %v", checkpoint, err)
	}
}

// The RED cumulative-allocation oracle is preserved in qualification/check10.
// This oracle bounds simultaneously reachable owner memory: at checkpoints,
// exact allocation profiles measure retained allocations from owner stacks.
// Between checkpoints every newly allocated process byte is charged, so peaks
// between observations cannot escape the bound. Shared Store/pool allocations
// preceding owner construction are outside this owner. No maintenance batch is
// retained by Store or Worker between cycles. Fixtures are built beforehand.
type nha032AllocationSource struct{ boundary recordermaintenance.Boundary }

func (s nha032AllocationSource) Boundary() (recordermaintenance.Boundary, error) {
	return s.boundary, nil
}

type nha032AllocationClock struct {
	at    time.Time
	ticks chan time.Time
}

func (c nha032AllocationClock) Now() time.Time                                     { return c.at }
func (c nha032AllocationClock) NewTicker(time.Duration) recordermaintenance.Ticker { return c }
func (c nha032AllocationClock) C() <-chan time.Time                                { return c.ticks }
func (c nha032AllocationClock) Stop()                                              {}

type nha032OwnerMemory struct {
	lastAllocated, retained, peak, retainedPeak uint64
	checkpoints                                 int
}

func (m *nha032OwnerMemory) observe() {
	var current runtime.MemStats
	runtime.ReadMemStats(&current)
	// GC is disabled between explicit checkpoints. The active owner stack
	// cannot shrink there; charging all process stacks is conservative.
	upper := m.retained + current.TotalAlloc - m.lastAllocated + current.StackInuse
	if upper > m.peak {
		m.peak = upper
	}
}
func (m *nha032OwnerMemory) checkpoint() {
	m.observe()
	// MemProfile can lag by two collections. The synchronous observer stops
	// the owner while collecting; collection changes garbage, not its roots.
	runtime.GC()
	runtime.GC()
	n, _ := runtime.MemProfile(nil, false)
	records := make([]runtime.MemProfileRecord, n+32)
	for {
		var ok bool
		n, ok = runtime.MemProfile(records, false)
		if ok {
			break
		}
		records = make([]runtime.MemProfileRecord, n+32)
	}
	var retained uint64
	for _, record := range records[:n] {
		owned, observer := false, false
		stack := record.Stack()
		for _, pc := range stack {
			if fn := runtime.FuncForPC(pc); fn != nil {
				name := fn.Name()
				owned = owned || strings.Contains(name, "nha032MeasuredOwnerCycle") || strings.Contains(name, "recordermaintenance.(*Worker).run")
				observer = observer || strings.Contains(name, "nha032OwnerMemory")
			}
		}
		// Include an unclassified truncated stack rather than silently lose
		// allocations whose owner frame might lie beyond the profile depth.
		if !observer && (owned || len(stack) == len(record.Stack0)) && record.InUseBytes() > 0 {
			retained += uint64(record.InUseBytes())
		}
	}
	m.retained = retained
	if retained > m.retainedPeak {
		m.retainedPeak = retained
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	m.lastAllocated = after.TotalAlloc
	m.checkpoints++
}

type nha032MemoryTracer struct {
	memory  *nha032OwnerMemory
	queries int
}

func (*nha032MemoryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (t *nha032MemoryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
	if t.memory != nil {
		t.queries++
		if t.queries%128 == 0 {
			t.memory.checkpoint()
		}
	}
}
func nha032InstallMemoryTracer(t *testing.T, s *Store) *nha032MemoryTracer {
	t.Helper()
	if tracer, ok := s.pool.Config().ConnConfig.Tracer.(*nha032MemoryTracer); ok {
		return tracer
	}
	config := s.pool.Config()
	tracer := &nha032MemoryTracer{}
	config.ConnConfig.Tracer = tracer
	s.pool.Close()
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	s.pool = pool
	return tracer
}

func nha032MeasuredOwnerCycle(t *testing.T, s *Store, epoch RecorderEpoch, high uint64, at time.Time) recordermaintenance.CycleResult {
	t.Helper()
	tracer := nha032InstallMemoryTracer(t, s)
	oldRate := runtime.MemProfileRate
	runtime.MemProfileRate = 1
	defer func() { runtime.MemProfileRate = oldRate }()
	oldGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(oldGC)
	memory := &nha032OwnerMemory{}
	// No owner exists yet. Discard prior owners and begin counting only
	// after profiler bookkeeping; construct boundary/clock/Worker below.
	runtime.GC()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	memory.lastAllocated = before.TotalAlloc
	tracer.memory = memory
	tracer.queries = 0
	boundary := nha032Boundary(t, epoch, high, at)
	worker, err := recordermaintenance.NewWithClock(s, nha032AllocationSource{boundary}, nha032AllocationClock{at, make(chan time.Time)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { worker.BeginClose(); <-worker.Done(); tracer.memory = nil }()
	if err = worker.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	var status recordermaintenance.Status
	for time.Now().Before(deadline) {
		status = worker.Status()
		if status.Cycles == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	memory.observe()
	t.Logf("fresh_owner retained_peak=%d interval_peak_upper_bytes=%d checkpoints=%d queries=%d limit=%d processed=%d deleted_raw=%d more=%v", memory.retainedPeak, memory.peak, memory.checkpoints, tracer.queries, recordermaintenance.MaxOwnerBytes, status.Result.Processed, status.Result.DeletedRaw, status.Result.More)
	if status.Cycles != 1 || status.Active || status.LastError != nil {
		t.Fatalf("real owner cycle incomplete: %+v", status)
	}
	if memory.peak > recordermaintenance.MaxOwnerBytes {
		t.Fatalf("live maintenance owner upper bound %d exceeds %d", memory.peak, recordermaintenance.MaxOwnerBytes)
	}
	if status.Result.Processed < 0 || status.Result.Processed > 500 || status.Result.DeletedRaw > 500 {
		t.Fatalf("owner exceeded row bounds: %+v", status.Result)
	}
	return status.Result
}

func nha032DrainMeasuredOwner(t *testing.T, s *Store, epoch RecorderEpoch, high uint64, at time.Time) recordermaintenance.CycleResult {
	t.Helper()
	var total recordermaintenance.CycleResult
	for attempt := 0; attempt <= 501; attempt++ {
		r := nha032MeasuredOwnerCycle(t, s, epoch, high, at)
		total.Processed += r.Processed
		total.DeletedRaw += r.DeletedRaw
		if !r.More {
			return total
		}
	}
	t.Fatal("measured owner failed to drain bounded continuation")
	return total
}

func TestNHA032FreshMaintenanceOwnerAllocationStaysBelowEightMiB(t *testing.T) {
	if recordermaintenance.MaxPrefixRows != 500 || recordermaintenance.MaxInputBytes != 4<<20 || recordermaintenance.MaxOwnerBytes != 8<<20 || recordermaintenance.AttemptTimeout != 2*time.Second {
		t.Fatal("ratified maintenance limits changed")
	}
	t.Run("five_hundred_max_identity_series_and_tails", func(t *testing.T) {
		s := nha032SQLFixture(t)
		epoch := nha031Epoch(t, 332)
		nha031Open(t, s, epoch)
		at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
		rows := make([]RecorderRecord, 0, 500)
		for i := 1; i <= 500; i++ {
			name := strings.Repeat("e", 1020) + fmt.Sprintf("%04d", i)
			rows = append(rows, nha032EntityRecord(t, name, uint64(i), at, at, float64(i), domain.SemanticMeasurement))
		}
		nha032Persist(t, s, epoch, 0, 500, rows, nil, nil)
		last := nha032EntityRecord(t, strings.Repeat("e", 1020)+"0501", 501, at, at, 501, domain.SemanticMeasurement)
		nha032Persist(t, s, epoch, 500, 501, []RecorderRecord{last}, nil, nil)
		if total := nha032DrainMeasuredOwner(t, s, epoch, 501, at); total.Processed != 501 {
			t.Fatalf("measured prefix lost positions: %+v", total)
		}
		if total := nha032DrainMeasuredOwner(t, s, epoch, 501, at.Add(time.Hour)); total.Processed != 0 {
			t.Fatalf("measured tails replayed: %+v", total)
		}
		var count int
		if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM public.recorder_hourly_aggregates WHERE duration_ns=3600000000000 AND reasons=0`).Scan(&count); err != nil || count != 501 {
			t.Fatalf("measured suffix lost series: %d %v", count, err)
		}
	})
	t.Run("near_four_mib_metadata_and_raw_purge", func(t *testing.T) {
		s := nha032SQLFixture(t)
		epoch := nha031Epoch(t, 333)
		nha031Open(t, s, epoch)
		at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
		for seq := uint64(1); seq <= 5; seq++ {
			row := nha032EntityRecord(t, strings.Repeat("x", 1<<20)+fmt.Sprintf("-%d", seq), seq, at, at.Add(-8*24*time.Hour), 10, domain.SemanticMeasurement)
			nha032Persist(t, s, epoch, seq-1, seq, []RecorderRecord{row}, nil, nil)
		}
		nha032Persist(t, s, epoch, 5, 7, []RecorderRecord{nha032EntityRecord(t, "nha032-memory-after", 6, at, at, 10, domain.SemanticMeasurement), nha032EntityRecord(t, "nha032-memory-after", 7, at.Add(time.Hour), at.Add(time.Hour), 30, domain.SemanticMeasurement)}, nil, nil)
		if total := nha032DrainMeasuredOwner(t, s, epoch, 7, at.Add(time.Hour)); total.Processed != 7 || total.DeletedRaw != 5 {
			t.Fatalf("measured byte prefix/purge lost progress: %+v", total)
		}
		nha032AssertEntityMean(t, s, "nha032-memory-after", at, 10)
	})
}

// Nondegenerate analytical intervals are half-open; a point refusal belongs
// exactly to the window whose start includes that point, never its predecessor.
func TestNHA032RefusalIntervalsExcludeAdjacentBucketsAndKeepBoundaryPoints(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch := nha031Epoch(t, 334)
	nha031Open(t, s, epoch)
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	entity := "nha032-r3-adjacent"
	nha032Persist(t, s, epoch, 0, 3, []RecorderRecord{
		nha032EntityRecord(t, entity, 1, at, at, 1e306, domain.SemanticMeasurement),
		nha032EntityRecord(t, entity, 2, at.Add(time.Hour), at.Add(time.Hour), 10, domain.SemanticMeasurement),
		nha032EntityRecord(t, entity, 3, at.Add(2*time.Hour), at.Add(2*time.Hour), 20, domain.SemanticMeasurement),
	}, nil, nil)
	if n, more, err := s.AggregateRecorderPrefix(ctx, nha032Boundary(t, epoch, 3, at.Add(2*time.Hour))); err != nil || n != 3 || more {
		t.Fatalf("adjacent prefix: %d %v %v", n, more, err)
	}
	nha032AssertEntityMean(t, s, entity, at.Add(time.Hour), 10)
	if rows, err := s.ReadRecorderAnalyticalBreaks(ctx, entity, at.Add(time.Hour), at.Add(2*time.Hour)); err != nil || len(rows) != 0 {
		t.Fatalf("earlier refused interval contaminated following window: %+v %v", rows, err)
	}
	first, err := s.ReadRecorderHourlyAggregates(ctx, entity, at, at.Add(time.Hour))
	if err != nil || len(first) != 1 || first[0].Values.Input().HasMeasurement || first[0].Values.Input().Reasons&recordermaintenance.UnsupportedNumeric == 0 {
		t.Fatalf("original refusal lost: %+v %v", first, err)
	}
	if err = s.InTransaction(ctx, func(tx pgx.Tx) error {
		if err := recordAnalyticalBreak(ctx, tx, hashEntity(entity), at.Add(time.Hour), at.Add(time.Hour), recordermaintenance.Gap); err != nil {
			return err
		}
		return recordAnalyticalBreak(ctx, tx, hashEntity(entity), at.Add(2*time.Hour), at.Add(2*time.Hour), recordermaintenance.ClockRegression)
	}); err != nil {
		t.Fatal(err)
	}
	for index, want := range []recordermaintenance.Reason{recordermaintenance.UnsupportedNumeric, recordermaintenance.Gap, recordermaintenance.ClockRegression} {
		start := at.Add(time.Duration(index) * time.Hour)
		breaks, err := s.ReadRecorderAnalyticalBreaks(ctx, entity, start, start.Add(time.Hour))
		if err != nil || len(breaks) != 1 || breaks[0].Reasons() != want {
			t.Fatalf("half-open interval/point window%d: %+v %v", index, breaks, err)
		}
		left, right := breaks[0].Bounds()
		if !left.Equal(start) || index == 0 && !right.Equal(start.Add(time.Hour)) || index != 0 && !right.Equal(start) {
			t.Fatalf("point/refusal bounds changed: %v %v", left, right)
		}
	}
	following, err := s.ReadRecorderHourlyAggregates(ctx, entity, at.Add(time.Hour), at.Add(2*time.Hour))
	if err != nil || len(following) != 1 || following[0].Values.Input().Reasons != recordermaintenance.Gap {
		t.Fatalf("point lost or preceding refusal leaked into aggregate: %+v %v", following, err)
	}
	if mean, present := following[0].Values.Mean(); !present || mean != 10 || following[0].Values.Input().Duration != time.Hour {
		t.Fatalf("point altered known measurement: %+v", following)
	}
	var raw int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM public.state_history`).Scan(&raw); err != nil || raw != 3 {
		t.Fatalf("oracle lost raw source: %d %v", raw, err)
	}
}

func TestNHA032ExtremeTailWithoutPurgePersistsRefusalAndRecovers(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch := nha031Epoch(t, 335)
	nha031Open(t, s, epoch)
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	entity := "nha032-extreme-tail-direct"
	nha032Persist(t, s, epoch, 0, 1, []RecorderRecord{nha032EntityRecord(t, entity, 1, at, at, 1e306, domain.SemanticMeasurement)}, nil, nil)
	if n, more, err := s.AggregateRecorderPrefix(ctx, nha032Boundary(t, epoch, 1, at)); err != nil || n != 1 || more {
		t.Fatalf("tail baseline: %d %v %v", n, more, err)
	}
	boundary := nha032Boundary(t, epoch, 1, at.Add(time.Hour))
	for range 2 {
		if n, more, err := s.AggregateRecorderPrefix(ctx, boundary); err != nil || n != 0 || more {
			t.Fatalf("direct refused tail: %d %v %v", n, more, err)
		}
		rows, err := s.ReadRecorderHourlyAggregates(ctx, entity, at, at.Add(time.Hour))
		if err != nil || len(rows) != 1 || rows[0].Values.Input().Reasons != recordermaintenance.UnsupportedNumeric || rows[0].Values.Input().HasMeasurement || rows[0].Values.Input().HasContribution {
			t.Fatalf("direct tail lost refusal/created zero: %+v %v", rows, err)
		}
		breaks, err := s.ReadRecorderAnalyticalBreaks(ctx, entity, at, at.Add(time.Hour))
		if err != nil || len(breaks) != 1 || breaks[0].Reasons() != recordermaintenance.UnsupportedNumeric {
			t.Fatalf("direct tail failed to persist envelope: %+v %v", breaks, err)
		}
		left, right := breaks[0].Bounds()
		if !left.Equal(at) || !right.Equal(at.Add(time.Hour)) {
			t.Fatalf("tail refused range: %v %v", left, right)
		}
	}
	nha032Persist(t, s, epoch, 1, 3, []RecorderRecord{
		nha032EntityRecord(t, entity, 2, at.Add(time.Hour), at.Add(time.Hour), 10, domain.SemanticMeasurement),
		nha032EntityRecord(t, entity, 3, at.Add(2*time.Hour), at.Add(2*time.Hour), 20, domain.SemanticMeasurement),
	}, nil, nil)
	if n, more, err := s.AggregateRecorderPrefix(ctx, nha032Boundary(t, epoch, 3, at.Add(2*time.Hour))); err != nil || n != 2 || more {
		t.Fatalf("tail recovery: %d %v %v", n, more, err)
	}
	nha032AssertEntityMean(t, s, entity, at.Add(time.Hour), 10)
	var raw int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM public.state_history`).Scan(&raw); err != nil || raw != 3 {
		t.Fatalf("direct tail was actually tested after retention: %d %v", raw, err)
	}
}

func TestNHA032AggregatePurgeKeepsPointAtCutoffAndItsIncompleteBucket(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch := nha031Epoch(t, 336)
	nha031Open(t, s, epoch)
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	cutoff := at.Add(time.Hour)
	entity := "nha032-point-cutoff"
	nha032Persist(t, s, epoch, 0, 3, []RecorderRecord{
		nha032EntityRecord(t, entity, 1, at, at, 5, domain.SemanticMeasurement),
		nha032EntityRecord(t, entity, 2, cutoff, cutoff, 10, domain.SemanticMeasurement),
		nha032EntityRecord(t, entity, 3, cutoff.Add(time.Hour), cutoff.Add(time.Hour), 20, domain.SemanticMeasurement),
	}, nil, nil)
	if n, more, err := s.AggregateRecorderPrefix(ctx, nha032Boundary(t, epoch, 3, cutoff.Add(time.Hour))); err != nil || n != 3 || more {
		t.Fatalf("cutoff prefix: %d %v %v", n, more, err)
	}
	if err := s.InTransaction(ctx, func(tx pgx.Tx) error {
		if err := recordAnalyticalBreak(ctx, tx, hashEntity(entity), at, cutoff, recordermaintenance.UnsupportedNumeric); err != nil {
			return err
		}
		if err := recordAnalyticalBreak(ctx, tx, hashEntity(entity), at.Add(30*time.Minute), at.Add(30*time.Minute), recordermaintenance.ClockRegression); err != nil {
			return err
		}
		return recordAnalyticalBreak(ctx, tx, hashEntity(entity), cutoff, cutoff, recordermaintenance.Gap)
	}); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		want := int64(3)
		if attempt != 0 {
			want = 0
		}
		if n, more, err := s.PurgeRecorderAggregates(ctx, cutoff); err != nil || n != want || more {
			t.Fatalf("point cutoff purge attempt%d: %d %v %v", attempt, n, more, err)
		}
		breaks, err := s.ReadRecorderAnalyticalBreaks(ctx, entity, cutoff, cutoff.Add(time.Hour))
		if err != nil || len(breaks) != 1 || breaks[0].Reasons() != recordermaintenance.Gap {
			t.Fatalf("point at cutoff lost or expired interval survived: %+v %v", breaks, err)
		}
		left, right := breaks[0].Bounds()
		if !left.Equal(cutoff) || !right.Equal(cutoff) {
			t.Fatalf("retained point bounds: %v %v", left, right)
		}
		rows, err := s.ReadRecorderHourlyAggregates(ctx, entity, cutoff, cutoff.Add(time.Hour))
		if err != nil || len(rows) != 1 || rows[0].Values.Input().Reasons != recordermaintenance.Gap {
			t.Fatalf("purge silently completed retained bucket: %+v %v", rows, err)
		}
		if mean, present := rows[0].Values.Mean(); !present || mean != 10 || rows[0].Values.Input().Duration != time.Hour {
			t.Fatalf("purge changed retained numeric value: %+v", rows)
		}
		var buckets, proofs int
		if err = s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM public.recorder_hourly_aggregates),(SELECT count(*) FROM public.recorder_analytical_breaks)`).Scan(&buckets, &proofs); err != nil || buckets != 1 || proofs != 1 {
			t.Fatalf("purge selection/More inconsistent: %d %d %v", buckets, proofs, err)
		}
	}
	// Moving the boundary beyond the point must expire it; the equality
	// exception cannot retain a point forever or leave More stuck true.
	if n, more, err := s.PurgeRecorderAggregates(ctx, cutoff.Add(time.Microsecond)); err != nil || n != 1 || more {
		t.Fatalf("advanced cutoff did not expire point: %d %v %v", n, more, err)
	}
	if breaks, err := s.ReadRecorderAnalyticalBreaks(ctx, entity, cutoff, cutoff.Add(time.Hour)); err != nil || len(breaks) != 0 {
		t.Fatalf("expired point survived advanced cutoff: %+v %v", breaks, err)
	}
	var buckets, proofs int
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM public.recorder_hourly_aggregates),(SELECT count(*) FROM public.recorder_analytical_breaks)`).Scan(&buckets, &proofs); err != nil || buckets != 1 || proofs != 0 {
		t.Fatalf("advanced cutoff altered bucket or retained point: %d %d %v", buckets, proofs, err)
	}
}
