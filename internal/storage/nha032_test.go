package storage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/recordermaintenance"
	"github.com/jackc/pgx/v5"
)

// The ordinary Recorder fixture uses the same production Migrate chain as Run.
// No test installs the aggregate schema outside the registered migration.
func nha032SQLFixture(t *testing.T) *Store {
	t.Helper()
	return nha030RecorderSQLFixture(t)
}

func nha032Migration20Fixture(t *testing.T) (*Store, Settings) {
	t.Helper()
	if len(migrations) < 21 || migrations[19].version.Number() != 20 || migrations[20].version.Number() != 21 || migrations[20].sql != recorderAggregateSchemaSQL {
		t.Fatal("registered append-only migration 20 to 21 missing")
	}
	settings := testSettings(t, testCluster(t))
	s, err := Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if version, err := s.migrate(context.Background(), migrations[:20]); err != nil || version.Number() != 20 {
		t.Fatalf("historical production migration 20: %d %v", version.Number(), err)
	}
	return s, settings
}

func TestNHA032Migration20To21PreservesHistoryAndReopens(t *testing.T) {
	s, settings := nha032Migration20Fixture(t)
	ctx := context.Background()
	epoch := nha031Epoch(t, 326)
	nha031Open(t, s, epoch)
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	row := nha032Record(t, 1, at, at, 10, domain.SemanticMeasurement)
	batch, err := NewRecorderBatch(epoch, 0, 1, []RecorderRecord{row}, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The current batch writer requires migration21's fence. Populate only
	// historical data through the unchanged row encoder while still on20.
	if err = s.InTransaction(ctx, func(tx pgx.Tx) error {
		if err := insertRecorderState(ctx, tx, epoch, row); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE public.recorder_epochs SET checkpoint=1,last_batch_from=0,last_batch_to=1,last_batch_digest=$2 WHERE epoch=$1`, epoch.data(), batch.digest[:])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	historyEpoch := nha186Epoch(t, "00000000000000000000000000000326")
	if _, err = s.OpenExecutionHistoryEpoch(ctx, historyEpoch); err != nil {
		t.Fatal(err)
	}
	trace := nha186Batch(t, historyEpoch, 1, nha186Record(t, historyEpoch, 1, at, 2))
	if result, err := s.CommitExecutionHistoryBatch(ctx, trace); err != nil || result != HistoryCommitted {
		t.Fatalf("historical migration20 trace: %v %v", result, err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO public.registry_devices(id) VALUES('nha032-upgrade-business-sentinel')`); err != nil {
		t.Fatal(err)
	}
	const historicalSnapshot = `SELECT jsonb_build_object('raw',(SELECT to_jsonb(h) FROM public.state_history h WHERE epoch=$1),'recorder',(SELECT to_jsonb(e) FROM public.recorder_epochs e WHERE epoch=$1),'trace',(SELECT to_jsonb(r) FROM public.execution_history_records r WHERE epoch=$2),'history',(SELECT to_jsonb(e) FROM public.execution_history_epochs e WHERE epoch=$2),'registry',(SELECT to_jsonb(d) FROM public.registry_devices d WHERE id='nha032-upgrade-business-sentinel'))::text`
	var before string
	if err = s.pool.QueryRow(ctx, historicalSnapshot, epoch.data(), historyEpoch.String()).Scan(&before); err != nil {
		t.Fatal(err)
	}
	var applied20 time.Time
	if err = s.pool.QueryRow(ctx, `SELECT applied_at FROM public.homeward_schema_migrations WHERE version=20`).Scan(&applied20); err != nil {
		t.Fatal(err)
	}
	if version, err := s.Migrate(ctx); err != nil || version.Number() != 21 {
		t.Fatalf("production upgrade20 to21: %d %v", version.Number(), err)
	}
	var tables, versions int
	var floor, applied21 time.Time
	if err = s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('recorder_maintenance_head','recorder_retention_ranges','recorder_numeric_continuations','recorder_analytical_breaks','recorder_hourly_aggregates')),(SELECT count(*) FROM public.homeward_schema_migrations),raw_cutoff,(SELECT applied_at FROM public.homeward_schema_migrations WHERE version=21) FROM public.recorder_maintenance_head WHERE singleton=true`).Scan(&tables, &versions, &floor, &applied21); err != nil || tables != 5 || versions != 21 || !floor.Equal(time.Time{}) {
		t.Fatalf("incomplete registered schema: tables=%d versions=%d floor=%v %v", tables, versions, floor, err)
	}
	s.Close()
	reopened, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for range 2 {
		if version, err := reopened.Migrate(ctx); err != nil || version.Number() != 21 {
			t.Fatalf("production reopen/repeat: %d %v", version.Number(), err)
		}
	}
	var after string
	var after20, after21 time.Time
	if err = reopened.pool.QueryRow(ctx, historicalSnapshot, epoch.data(), historyEpoch.String()).Scan(&after); err != nil || before != after {
		t.Fatalf("upgrade/reopen changed historical payload, digest, checkpoint or business row: %v", err)
	}
	if err = reopened.pool.QueryRow(ctx, `SELECT (SELECT applied_at FROM public.homeward_schema_migrations WHERE version=20),(SELECT applied_at FROM public.homeward_schema_migrations WHERE version=21),(SELECT count(*) FROM public.homeward_schema_migrations)`).Scan(&after20, &after21, &versions); err != nil || !applied20.Equal(after20) || !applied21.Equal(after21) || versions != 21 {
		t.Fatalf("repeat changed migration history: %v", err)
	}
	if checkpoint, err := reopened.RecorderCheckpoint(ctx, epoch); err != nil || !checkpoint.Confirms(batch) {
		t.Fatalf("historical recorder checkpoint: %+v %v", checkpoint, err)
	}
	boundary := nha032Boundary(t, epoch, 1, at.Add(time.Hour))
	for attempt := 0; attempt < 2; attempt++ {
		want := int64(1)
		if attempt == 1 {
			want = 0
		}
		if n, more, err := reopened.AggregateRecorderPrefix(ctx, boundary); err != nil || n != want || more {
			t.Fatalf("upgraded prefix attempt%d: %d %v %v", attempt, n, more, err)
		}
	}
	aggregates, err := reopened.ReadRecorderHourlyAggregates(ctx, "nha032-measurement", at, at.Add(time.Hour))
	if err != nil || len(aggregates) != 1 {
		t.Fatalf("upgraded aggregate: %+v %v", aggregates, err)
	}
	if mean, present := aggregates[0].Values.Mean(); !present || mean != 10 || aggregates[0].Values.Input().Duration != time.Hour {
		t.Fatalf("upgraded/replayed prefix duplicated historical contribution: %+v", aggregates[0])
	}
}

func TestNHA032Migration21RollsBackDDLAndVersion(t *testing.T) {
	s, _ := nha032Migration20Fixture(t)
	ctx := context.Background()
	// A real DDL collision occurs after the migration's first four tables.
	// The production plan must roll all of them back with its version row.
	if _, err := s.pool.Exec(ctx, `CREATE TABLE public.recorder_hourly_aggregates(sentinel text PRIMARY KEY); INSERT INTO public.recorder_hourly_aggregates VALUES('nha032-preserved-collision'); INSERT INTO public.registry_devices(id) VALUES('nha032-rollback-business-sentinel')`); err != nil {
		t.Fatal(err)
	}
	_, err := s.Migrate(ctx)
	var migrationErr *Error
	if !errors.As(err, &migrationErr) || migrationErr.Kind() != MigrationFailed {
		t.Fatalf("registered migration collision not rolled back: %v", err)
	}
	var created, versions, highest, collision, business int
	if err = s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('recorder_maintenance_head','recorder_retention_ranges','recorder_numeric_continuations','recorder_analytical_breaks')),(SELECT count(*) FROM public.homeward_schema_migrations),(SELECT max(version) FROM public.homeward_schema_migrations),(SELECT count(*) FROM public.recorder_hourly_aggregates WHERE sentinel='nha032-preserved-collision'),(SELECT count(*) FROM public.registry_devices WHERE id='nha032-rollback-business-sentinel')`).Scan(&created, &versions, &highest, &collision, &business); err != nil || created != 0 || versions != 20 || highest != 20 || collision != 1 || business != 1 {
		t.Fatalf("partial migration survived rollback: tables=%d history=%d/%d collision=%d business=%d %v", created, versions, highest, collision, business, err)
	}
	if _, err = s.pool.Exec(ctx, `DROP TABLE public.recorder_hourly_aggregates`); err != nil {
		t.Fatal(err)
	}
	if version, err := s.Migrate(ctx); err != nil || version.Number() != 21 {
		t.Fatalf("production migration did not recover after collision removal: %d %v", version.Number(), err)
	}
}

func nha032Record(t *testing.T, seq uint64, admitted, received time.Time, value float64, semantic domain.SemanticClass) RecorderRecord {
	t.Helper()
	return nha032EntityRecord(t, "nha032-measurement", seq, admitted, received, value, semantic)
}
func nha032EntityRecord(t *testing.T, name string, seq uint64, admitted, received time.Time, value float64, semantic domain.SemanticClass) RecorderRecord {
	t.Helper()
	entity, err := domain.ParseEntityID(name)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha032-fake")
	source, _ := domain.NewSourceStamp("nha032-source", nil)
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	unit, _ := domain.NewUnitCatalog().Parse("W")
	aggregation := domain.AggregationCurrent
	if semantic != domain.SemanticMeasurement {
		aggregation = domain.AggregationSum
	}
	measurement, err := domain.NewMeasurement(value, unit, 2, semantic, aggregation)
	if err != nil {
		t.Fatal(err)
	}
	v, err := domain.NewMeasurementValue(measurement)
	if err != nil {
		t.Fatal(err)
	}
	state, err := domain.NewObservedStateSnapshot(entity, quality, &v, domain.StateTimes{ReceivedAt: received, LastChanged: received, LastUpdated: received, LastReported: received}, source, seq, nil)
	if err != nil {
		t.Fatal(err)
	}
	row, err := NewRecorderRecord(seq, instance, state, domain.Causality{}, admitted, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	return row
}
func nha032Persist(t *testing.T, s *Store, epoch RecorderEpoch, from, through uint64, rows []RecorderRecord, gaps []RecorderGap, image *RecorderImage) RecorderBatch {
	t.Helper()
	batch, err := NewRecorderBatch(epoch, from, through, rows, gaps, 0, image)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := s.PersistRecorderBatch(context.Background(), batch); outcome != TransactionCommitted || err != nil {
		t.Fatalf("persist %v %v", outcome, err)
	}
	return batch
}
func nha032Boundary(t *testing.T, epoch RecorderEpoch, through uint64, at time.Time) recordermaintenance.Boundary {
	t.Helper()
	text, _ := epoch.Hex()
	b, err := recordermaintenance.NewBoundary(text, through, at, 0)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func nha032SeedWeighted(t *testing.T, s *Store) (RecorderEpoch, time.Time) {
	t.Helper()
	epoch := nha031Epoch(t, 320)
	nha031Open(t, s, epoch)
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	rows := []RecorderRecord{nha032Record(t, 1, at, at, 10, domain.SemanticMeasurement), nha032Record(t, 2, at.Add(15*time.Minute), at.Add(15*time.Minute), 30, domain.SemanticMeasurement), nha032Record(t, 3, at.Add(time.Hour), at.Add(time.Hour), 50, domain.SemanticMeasurement)}
	nha032Persist(t, s, epoch, 0, 3, rows, nil, nil)
	return epoch, at
}
func nha032AssertMean(t *testing.T, s *Store, at time.Time) {
	t.Helper()
	rows, err := s.ReadRecorderHourlyAggregates(context.Background(), "nha032-measurement", at, at.Add(time.Hour))
	if err != nil || len(rows) != 1 {
		t.Fatalf("aggregates %v %v", rows, err)
	}
	mean, ok := rows[0].Values.Mean()
	if !ok || mean != 25 || rows[0].Values.Input().Duration != time.Hour || rows[0].Values.Input().Reset != recordermaintenance.ResetNotApplicable {
		t.Fatalf("mean=%v/%v row=%+v", mean, ok, rows[0])
	}
}

func TestNHA032HourlySQLAtomicPrefixAndIdempotence(t *testing.T) {
	s := nha032SQLFixture(t)
	epoch, at := nha032SeedWeighted(t, s)
	b := nha032Boundary(t, epoch, 3, at.Add(time.Hour))
	n, more, err := s.AggregateRecorderPrefix(context.Background(), b)
	if err != nil || n != 3 || more {
		t.Fatalf("prefix %d %v %v", n, more, err)
	}
	nha032AssertMean(t, s, at)
	if n, more, err = s.AggregateRecorderPrefix(context.Background(), b); err != nil || n != 0 || more {
		t.Fatalf("repeat %d %v %v", n, more, err)
	}
	nha032AssertMean(t, s, at)
	if _, _, err = s.PurgeRecorderRaw(context.Background(), at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	nha032AssertMean(t, s, at)
	through := uint64(3)
	proof, err := s.ReadRecorderCoverage(context.Background(), nha031Request(t, epoch, 0, epoch, &through))
	if err != nil || len(proof.Retention()) == 0 {
		t.Fatalf("retention proof %v %v", proof, err)
	}
}
func TestNHA032RawFenceRetainsCheckpointDigestAndExactBoundary(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch := nha031Epoch(t, 321)
	nha031Open(t, s, epoch)
	cutoff := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	old := nha032Record(t, 1, cutoff.Add(6*24*time.Hour), cutoff.Add(-time.Microsecond), 7, domain.SemanticMeasurement)
	image := RecorderSnapshotRecord{observed: old.observed}
	image.digest = digestRecorderSnapshot(image)
	batch := nha032Persist(t, s, epoch, 0, 1, []RecorderRecord{old}, nil, &RecorderImage{Boundary: 1, States: []RecorderSnapshotRecord{image}})
	n, more, err := s.PurgeRecorderRaw(ctx, cutoff)
	if err != nil || n != 2 || more {
		t.Fatalf("raw purge %d %v %v", n, more, err)
	}
	cp, err := s.RecorderCheckpoint(ctx, epoch)
	if err != nil || !cp.Confirms(batch) {
		t.Fatalf("checkpoint changed %v %v", cp, err)
	}
	late := nha032Record(t, 2, cutoff.Add(7*24*time.Hour), cutoff.Add(-time.Microsecond), 9, domain.SemanticMeasurement)
	lateBatch := nha032Persist(t, s, epoch, 1, 2, []RecorderRecord{late}, nil, nil)
	cp, err = s.RecorderCheckpoint(ctx, epoch)
	if err != nil || !cp.Confirms(lateBatch) {
		t.Fatal("late digest/checkpoint lost")
	}
	equal := nha032Record(t, 3, cutoff.Add(8*24*time.Hour), cutoff, 0, domain.SemanticMeasurement)
	nha032Persist(t, s, epoch, 2, 3, []RecorderRecord{equal}, nil, nil)
	var states, images int
	if err = s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM public.state_history),(SELECT count(*) FROM public.recorder_snapshots)`).Scan(&states, &images); err != nil || states != 1 || images != 0 {
		t.Fatalf("resurrection/equality states=%d images=%d %v", states, images, err)
	}
	if n, more, err = s.PurgeRecorderRaw(ctx, cutoff); err != nil || n != 0 || more {
		t.Fatalf("idempotence %d %v %v", n, more, err)
	}
}
func TestNHA032FailedCommitRollsBackAnalyticalPrefix(t *testing.T) {
	s := nha032SQLFixture(t)
	epoch, at := nha032SeedWeighted(t, s)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION public.nha032_reject_progress() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'nha032 controlled rollback'; END $$; CREATE TRIGGER nha032_reject_progress BEFORE UPDATE ON public.recorder_maintenance_head FOR EACH ROW EXECUTE FUNCTION public.nha032_reject_progress()`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AggregateRecorderPrefix(ctx, nha032Boundary(t, epoch, 3, at.Add(time.Hour))); err == nil {
		t.Fatal("controlled failure accepted")
	}
	var aggregates, continuations, raw int
	var run int64
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM public.recorder_hourly_aggregates),(SELECT count(*) FROM public.recorder_numeric_continuations),(SELECT count(*) FROM public.state_history),analytic_run FROM public.recorder_maintenance_head WHERE singleton=true`).Scan(&aggregates, &continuations, &raw, &run); err != nil || aggregates != 0 || continuations != 0 || raw != 3 || run != 0 {
		t.Fatalf("partial commit %d %d %d %d %v", aggregates, continuations, raw, run, err)
	}
	if _, err := s.pool.Exec(ctx, `DROP TRIGGER nha032_reject_progress ON public.recorder_maintenance_head`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AggregateRecorderPrefix(ctx, nha032Boundary(t, epoch, 3, at.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	nha032AssertMean(t, s, at)
}
func TestNHA032ConcurrentWorkersDoNotDoubleCount(t *testing.T) {
	s := nha032SQLFixture(t)
	epoch, at := nha032SeedWeighted(t, s)
	b := nha032Boundary(t, epoch, 3, at.Add(time.Hour))
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := s.AggregateRecorderPrefix(context.Background(), b)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil && !errors.Is(err, recordermaintenance.ErrUnavailable) {
			t.Fatal(err)
		}
	}
	if _, _, err := s.AggregateRecorderPrefix(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	nha032AssertMean(t, s, at)
}
func TestNHA032PurgeClassesAndCivilCutoff(t *testing.T) {
	s := nha032SQLFixture(t)
	epoch, at := nha032SeedWeighted(t, s)
	ctx := context.Background()
	if _, _, err := s.AggregateRecorderPrefix(ctx, nha032Boundary(t, epoch, 3, at.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO public.registry_devices(id) VALUES('nha032-business-sentinel'); INSERT INTO public.auth_audit(occurred_at,action,outcome,route_path) VALUES('2026-07-01T11:59:59.999999Z',1,1,'nha032-old'),('2026-07-01T12:00:00Z',1,1,'nha032-equal'); INSERT INTO public.secret_access_audit(requesting_instance,target_instance,reason,recorded_at) VALUES('nha032-a','nha032-b','foreign_handle','2026-07-01T11:59:59.999999Z'),('nha032-a','nha032-b','foreign_handle','2026-07-01T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	n, more, err := s.PurgeRecorderAudit(ctx, time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))
	if err != nil || n != 2 || more {
		t.Fatalf("audit %d %v %v", n, more, err)
	}
	var registry, raw, auth, secret int
	if err = s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM public.registry_devices WHERE id='nha032-business-sentinel'),(SELECT count(*) FROM public.state_history),(SELECT count(*) FROM public.auth_audit WHERE route_path='nha032-equal'),(SELECT count(*) FROM public.secret_access_audit)`).Scan(&registry, &raw, &auth, &secret); err != nil || registry != 1 || raw != 3 || auth != 1 || secret != 1 {
		t.Fatalf("class isolation %d %d %d %d %v", registry, raw, auth, secret, err)
	}
	if n, _, err = s.PurgeRecorderAggregates(ctx, at.Add(time.Hour-time.Microsecond)); err != nil || n != 0 {
		t.Fatalf("overlap deleted %d %v", n, err)
	}
	if n, _, err = s.PurgeRecorderAggregates(ctx, at.Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("end equality retained %d %v", n, err)
	}
}
func TestNHA032SQLResetCodesAndAdmittedValues(t *testing.T) {
	for code, reset := range map[int16]recordermaintenance.Reset{1: recordermaintenance.ResetNotApplicable, 2: recordermaintenance.ResetUnknown, 3: recordermaintenance.ResetNotDetected, 4: recordermaintenance.ResetInferred} {
		var delta *float64
		if reset == recordermaintenance.ResetInferred {
			zero := float64(0)
			delta = &zero
		}
		v, err := aggregateValues(0, 0, nil, nil, delta, code, 0)
		if err != nil || v.Input().Reset != reset || int16(reset) != code {
			t.Fatalf("reset code %d -> %v %v", code, v, err)
		}
	}
	for _, code := range []int16{0, 5, 257} {
		if _, err := aggregateValues(0, 0, nil, nil, nil, code, 0); err == nil {
			t.Fatalf("unknown reset %d admitted", code)
		}
	}
}
func TestNHA032BarrierSuffixPersistsAndDoesNotDoubleCount(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch := nha031Epoch(t, 325)
	nha031Open(t, s, epoch)
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	a := nha032Record(t, 1, at, at, 10, domain.SemanticMeasurement)
	nha032Persist(t, s, epoch, 0, 1, []RecorderRecord{a}, nil, nil)
	if _, _, err := s.AggregateRecorderPrefix(ctx, nha032Boundary(t, epoch, 1, at.Add(30*time.Minute))); err != nil {
		t.Fatal(err)
	}
	b := nha032Record(t, 2, at.Add(time.Hour), at.Add(time.Hour), 30, domain.SemanticMeasurement)
	nha032Persist(t, s, epoch, 1, 2, []RecorderRecord{b}, nil, nil)
	if _, _, err := s.AggregateRecorderPrefix(ctx, nha032Boundary(t, epoch, 2, at.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ReadRecorderHourlyAggregates(ctx, "nha032-measurement", at, at.Add(time.Hour))
	if err != nil || len(rows) != 1 {
		t.Fatalf("suffix rows %+v %v", rows, err)
	}
	if mean, ok := rows[0].Values.Mean(); !ok || mean != 10 || rows[0].Values.Input().Duration != time.Hour {
		t.Fatalf("double suffix %+v", rows[0])
	}
}
