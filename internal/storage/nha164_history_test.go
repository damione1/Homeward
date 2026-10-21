package storage

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/historyquery"
)

func nha164Query(t *testing.T, kind historyquery.Kind, entity string, start, end time.Time, limit int) historyquery.Query {
	t.Helper()
	q, err := historyquery.NewQuery(kind, entity, start, end, limit, "")
	if err != nil {
		t.Fatal(err)
	}
	return q
}
func TestNHA164RawReceivedAxisLogicalOrderAndHighWater(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch := nha031Epoch(t, 164)
	nha031Open(t, s, epoch)
	now := time.Now().UTC().Truncate(time.Microsecond)
	entity := "nha164-raw"
	a := nha032EntityRecord(t, entity, 1, now.Add(-time.Minute), now.Add(-time.Second), 0, domain.SemanticMeasurement)
	b := nha032EntityRecord(t, entity, 2, now, now.Add(-2*time.Second), 2, domain.SemanticMeasurement)
	// Source capture is older than seven days, while local reception is fresh.
	// Rebuild payload/digests through the real admission instead of mutating the
	// record used by the writer: a separate stale-source fixture below does that.
	a = nha164SourceTimeRecord(t, a, now.Add(-8*24*time.Hour))
	nha032Persist(t, s, epoch, 0, 2, []RecorderRecord{a, b}, nil, nil)
	q := nha164Query(t, historyquery.Observations, entity, now.Add(-time.Hour), now.Add(time.Hour), 1)
	first, err := s.ReadHistoricalRecorderPage(ctx, q, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Observations) != 1 || first.Observations[0].Sequence != 1 || first.Next == nil || first.Observations[0].State.GetMeasurement().Value != 0 {
		first.Release()
		t.Fatalf("lost real zero/first key: %+v", first)
	}
	if !first.Observations[0].State.ObservedAt.AsTime().Equal(now.Add(-8 * 24 * time.Hour)) {
		first.Release()
		t.Fatal("source capture replaced by received time")
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
	c := nha032EntityRecord(t, entity, 3, now.Add(time.Second), now, 3, domain.SemanticMeasurement)
	nha032Persist(t, s, epoch, 2, 3, []RecorderRecord{c}, nil, nil)
	second, err := s.ReadHistoricalRecorderPage(ctx, q, cursor)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if len(second.Observations) != 1 || second.Observations[0].Sequence != 2 || second.Next != nil {
		t.Fatalf("append escaped highwater or clock regression lost point: %+v", second)
	}
}
func nha164SourceTimeRecord(t *testing.T, row RecorderRecord, source time.Time) RecorderRecord {
	return nha164SourceRecord(t, row, source, "nha164-source")
}
func nha164SourceRecord(t *testing.T, row RecorderRecord, source time.Time, session string) RecorderRecord {
	t.Helper()
	// Decode existing wire, alter only the optional source timestamp, and re-admit
	// through the same immutable domain snapshot and production Recorder encoder.
	entity, err := domain.ParseEntityID(row.observed.entity)
	if err != nil {
		t.Fatal(err)
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	units := domain.NewUnitCatalog()
	unit, err := units.Parse("W")
	if err != nil {
		t.Fatal(err)
	}
	measurement, err := domain.NewMeasurement(*row.observed.measurementValue, unit, 2, domain.SemanticMeasurement, domain.AggregationMean)
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.NewMeasurementValue(measurement)
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := domain.NewSourceStamp(session, nil)
	if err != nil {
		t.Fatal(err)
	}
	times := domain.StateTimes{ObservedAt: &source, ReceivedAt: row.observed.receivedAt, LastChanged: row.observed.lastChanged, LastUpdated: row.observed.lastUpdated, LastReported: row.observed.lastReported}
	observed, err := domain.NewObservedStateSnapshot(entity, quality, &value, times, stamp, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha164-fixture")
	if err != nil {
		t.Fatal(err)
	}
	record, err := NewRecorderRecord(row.sequence, instance, observed, domain.Causality{}, row.admittedAt, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	return record
}
func TestNHA164CursorInvalidatesOnActualRawPurge(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch := nha031Epoch(t, 165)
	nha031Open(t, s, epoch)
	now := time.Now().UTC().Truncate(time.Microsecond)
	rows := []RecorderRecord{nha032EntityRecord(t, "nha164-purge", 1, now, now, 1, domain.SemanticMeasurement), nha032EntityRecord(t, "nha164-purge", 2, now, now, 2, domain.SemanticMeasurement)}
	nha032Persist(t, s, epoch, 0, 2, rows, nil, nil)
	q := nha164Query(t, historyquery.Observations, "nha164-purge", now.Add(-time.Hour), now.Add(time.Hour), 1)
	page, err := s.ReadHistoricalRecorderPage(ctx, q, nil)
	if err != nil {
		t.Fatal(err)
	}
	cursor := page.Next
	page.Release()
	if _, _, err = s.PurgeRecorderRaw(ctx, now.Add(time.Microsecond)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReadHistoricalRecorderPage(ctx, q, cursor); !errors.Is(err, historyquery.ErrStale) {
		t.Fatalf("purged continuation was not stale: %v", err)
	}
}
func TestNHA164PersistedTracesUntilAndCursorRoundTrip(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch := nha186Epoch(t, "00000000000000000000000000000164")
	if _, err := s.OpenExecutionHistoryEpoch(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	batch := nha186Batch(t, epoch, 1, nha186Record(t, epoch, 1, at, 2), nha186Record(t, epoch, 2, at.Add(time.Second), 2), nha186Record(t, epoch, 3, at.Add(2*time.Second), 2))
	if _, err := s.CommitExecutionHistoryBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	query := HistoryPageQuery{Since: at.Add(-time.Second), Until: at.Add(2 * time.Second), Limit: 1, ClipRetention: true}
	first, err := s.ReadExecutionHistoryPage(ctx, query)
	if err != nil || len(first.Records) != 1 || first.Next == nil || first.Epoch != epoch {
		t.Fatalf("current persisted epoch not discovered: %+v %v", first, err)
	}
	raw, err := first.Next.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	query.Cursor, err = ParseHistoryPageCursor(raw)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ReadExecutionHistoryPage(ctx, query)
	if err != nil || len(second.Records) != 1 || second.Records[0].Record.ID().Ordinal() != 2 || second.Next != nil {
		t.Fatalf("exclusive until/keyset changed: %+v %v", second, err)
	}
}

func TestNHA164AggregatePaginationPurgeRevisionAndRollback(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch, at := nha032SeedWeighted(t, s)
	if _, _, err := s.AggregateRecorderPrefix(ctx, nha032Boundary(t, epoch, 3, at.Add(2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	q := nha164Query(t, historyquery.Aggregates, "nha032-measurement", at, at.Add(2*time.Hour), 1)
	first, err := s.ReadHistoricalRecorderPage(ctx, q, nil)
	if err != nil {
		t.Fatal(err)
	}
	cursor := first.Next
	first.Release()
	if cursor == nil {
		t.Fatal("test requires second physical SQL page")
	}
	var before int64
	if err = s.pool.QueryRow(ctx, `SELECT revision FROM public.recorder_maintenance_head WHERE singleton=true`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	// A trigger failure happens after deletion but before commit; both group
	// removal and the revision update must roll back as one production transaction.
	if _, err = s.pool.Exec(ctx, `CREATE FUNCTION public.nha164_reject_revision() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic rejection'; END $$; CREATE TRIGGER nha164_reject_revision BEFORE UPDATE ON public.recorder_maintenance_head FOR EACH ROW EXECUTE FUNCTION public.nha164_reject_revision()`); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.PurgeRecorderAggregates(ctx, at.Add(time.Hour)); err == nil {
		t.Fatal("injected failure did not roll back purge")
	}
	var after int64
	var groups int
	if err = s.pool.QueryRow(ctx, `SELECT revision,(SELECT count(*) FROM public.recorder_hourly_aggregates) FROM public.recorder_maintenance_head WHERE singleton=true`).Scan(&after, &groups); err != nil || after != before || groups != 2 {
		t.Fatalf("purge/revision did not roll back atomically: %d %d %v", after, groups, err)
	}
	if _, err = s.pool.Exec(ctx, `DROP TRIGGER nha164_reject_revision ON public.recorder_maintenance_head`); err != nil {
		t.Fatal(err)
	}
	if n, _, err := s.PurgeRecorderAggregates(ctx, at.Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("actual purge failed: %d %v", n, err)
	}
	if _, err = s.ReadHistoricalRecorderPage(ctx, q, cursor); !errors.Is(err, historyquery.ErrStale) {
		t.Fatalf("actual purge did not invalidate cursor: %v", err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT revision FROM public.recorder_maintenance_head WHERE singleton=true`).Scan(&after); err != nil || after != before+1 {
		t.Fatalf("missing revision bump: %d %v", after, err)
	}
	if n, _, err := s.PurgeRecorderAggregates(ctx, at.Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("zero purge changed class: %d %v", n, err)
	}
	var repeated int64
	if err = s.pool.QueryRow(ctx, `SELECT revision FROM public.recorder_maintenance_head WHERE singleton=true`).Scan(&repeated); err != nil || repeated != after {
		t.Fatalf("zero purge bumped revision: %d %v", repeated, err)
	}
}
func TestNHA164ClosedDatabaseNeverProducesCompleteEmptyPage(t *testing.T) {
	s := nha032SQLFixture(t)
	s.Close()
	now := time.Now().UTC()
	q := nha164Query(t, historyquery.Observations, "nha164-closed", now.Add(-time.Hour), now, 1)
	page, err := s.ReadHistoricalRecorderPage(context.Background(), q, nil)
	if !errors.Is(err, historyquery.ErrUnavailable) || page.Coverage.Kind() != RecorderCoverageReadUnspecified {
		t.Fatalf("unavailable DB masqueraded as complete empty page: %+v %v", page, err)
	}
}

type nha164PayloadSpy struct{ payloadReads atomic.Int32 }

func (s *nha164PayloadSpy) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "SELECT payload,payload_digest") {
		s.payloadReads.Add(1)
	}
	return ctx
}
func (*nha164PayloadSpy) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func TestNHA164OversizedValidPointRejectedBeforePayloadRead(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch := nha031Epoch(t, 166)
	nha031Open(t, s, epoch)
	now := time.Now().UTC().Truncate(time.Microsecond)
	row := nha032EntityRecord(t, "nha164-oversized", 1, now, now, 0, domain.SemanticMeasurement)
	row = nha164SourceRecord(t, row, now, strings.Repeat("synthetic", 32<<10))
	nha032Persist(t, s, epoch, 0, 1, []RecorderRecord{row}, nil, nil)
	spy := &nha164PayloadSpy{}
	configuration := s.pool.Config()
	configuration.ConnConfig.Tracer = spy
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	s.pool.Close()
	s.pool = pool
	q := nha164Query(t, historyquery.Observations, "nha164-oversized", now.Add(-time.Hour), now.Add(time.Hour), 100)
	if _, err = s.ReadHistoricalRecorderPage(ctx, q, nil); !errors.Is(err, historyquery.ErrPointTooLarge) {
		t.Fatalf("oversized valid recorder point not refused: %v", err)
	}
	if spy.payloadReads.Load() != 0 {
		t.Fatal("oversized payload materialized before refusal")
	}
}
func TestNHA164ReaderOwnerChargesCopiesAndThirdReaderDeadline(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch := nha031Epoch(t, 167)
	nha031Open(t, s, epoch)
	now := time.Now().UTC().Truncate(time.Microsecond)
	row := nha032EntityRecord(t, "nha164-owner", 1, now, now, 0, domain.SemanticMeasurement)
	row = nha164SourceRecord(t, row, now, strings.Repeat("s", 120<<10))
	nha032Persist(t, s, epoch, 0, 1, []RecorderRecord{row}, nil, nil)
	q := nha164Query(t, historyquery.Observations, "nha164-owner", now.Add(-time.Hour), now.Add(time.Hour), 100)
	runtime.GC()
	previousGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(previousGC)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	first, err := s.ReadHistoricalRecorderPage(ctx, q, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	second, err := s.ReadHistoricalRecorderPage(ctx, q, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	// Keep two independent SQL+decoded+wire payload owners alive together. This
	// observes actual heap allocation, rather than asserting len(payload) alone.
	if len(first.Observations) != 1 || len(second.Observations) != 1 {
		t.Fatal("large owner fixture was not read")
	}
	runtime.ReadMemStats(&after)
	if after.TotalAlloc-before.TotalAlloc > 2*historyquery.ReaderReservationBytes {
		t.Fatalf("actual owners exceeded reservations: %d", after.TotalAlloc-before.TotalAlloc)
	}
	bounded, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err = s.ReadHistoricalRecorderPage(bounded, q, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("third reader bypassed shared gate: %v", err)
	}
	if time.Since(started) > historyquery.QueryTimeout {
		t.Fatal("reader acquisition escaped parent/2s budget")
	}
	first.Release()
	second.Release()
	again, err := s.ReadHistoricalRecorderPage(ctx, q, nil)
	if err != nil {
		t.Fatal(err)
	}
	again.Release()
}

func TestNHA164RawHistoricalUnitsAreIndependentOfAnalyticalAdmission(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch := nha031Epoch(t, 168)
	nha031Open(t, s, epoch)
	now := time.Now().UTC().Truncate(time.Microsecond)
	entity, _ := domain.ParseEntityID("nha164-long-historical-unit")
	instance, _ := domain.ParseIntegrationInstanceID("nha164-unit")
	symbol := strings.Repeat("u", 300)
	units := domain.NewUnitCatalog()
	if err := units.Register(symbol, domain.DimensionPower, 0.01, 0); err != nil {
		t.Fatal(err)
	}
	unit, err := units.Parse(symbol)
	if err != nil {
		t.Fatal(err)
	}
	baseUnit, err := units.Parse("W")
	if err != nil {
		t.Fatal(err)
	}
	measurement, err := domain.NewMeasurement(0, baseUnit, 0, domain.SemanticMeasurement, domain.AggregationCurrent)
	if err != nil {
		t.Fatal(err)
	}
	measurement, err = measurement.Convert(unit)
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.NewMeasurementValue(measurement)
	if err != nil {
		t.Fatal(err)
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceEstimated)
	source, _ := domain.NewSourceStamp("nha164-historical", nil)
	state, err := domain.NewObservedStateSnapshot(entity, quality, &value, domain.StateTimes{ReceivedAt: now, LastChanged: now, LastUpdated: now, LastReported: now}, source, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	row, err := NewRecorderRecord(1, instance, state, domain.Causality{}, now, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	nha032Persist(t, s, epoch, 0, 1, []RecorderRecord{row}, nil, nil)
	q := nha164Query(t, historyquery.Observations, "nha164-long-historical-unit", now.Add(-time.Hour), now.Add(time.Hour), 1)
	page, err := s.ReadHistoricalRecorderPage(ctx, q, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer page.Release()
	if len(page.Observations) != 1 || page.Observations[0].Descriptor == nil || page.Observations[0].Descriptor.Unit != symbol || page.Observations[0].Descriptor.Precision != -2 || page.Observations[0].State.GetMeasurement().Value != 0 {
		t.Fatal("current/analytical unit admission rewrote a valid historical descriptor")
	}
}
