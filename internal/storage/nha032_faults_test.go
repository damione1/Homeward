package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/recordermaintenance"
	"github.com/jackc/pgx/v5"
)

func TestNHA032LostCommitACKDoesNotDoubleContribution(t *testing.T) {
	store, settings := nha051Store(t)
	epoch, at := nha032SeedWeighted(t, store)
	b := nha032Boundary(t, epoch, 3, at.Add(time.Hour))
	proxy := nha032StartCommitACKProxy(t, int(settings.port))
	settings.port = uint16(proxy.port(t))
	writer, err := Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	proxy.armed.Store(true)
	finished := make(chan error, 1)
	go func() { _, _, err := writer.AggregateRecorderPrefix(context.Background(), b); finished <- err }()
	defer proxy.allowDrop()
	select {
	case <-proxy.held:
	case err := <-finished:
		t.Fatalf("returned before durable ACK trap %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("real PG COMMIT not trapped")
	}
	nha032AssertMean(t, store, at)
	proxy.allowDrop()
	select {
	case err = <-finished:
		if !errors.Is(err, recordermaintenance.ErrCommitUnknown) {
			t.Fatalf("lost ACK classification %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not join")
	}
	n, more, err := store.AggregateRecorderPrefix(context.Background(), b)
	if err != nil || n != 0 || more {
		t.Fatalf("resolution did not see committed cursor %d %v %v", n, more, err)
	}
	nha032AssertMean(t, store, at)
}
func TestNHA032CoverageAndRowsShareOnePostgresSnapshot(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch := nha031Epoch(t, 322)
	nha031Open(t, s, epoch)
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var before string
	if err = tx.QueryRow(ctx, `SELECT checkpoint::text FROM public.recorder_epochs WHERE epoch=$1`, epoch.data()).Scan(&before); err != nil || before != "0" {
		t.Fatalf("initial snapshot %s %v", before, err)
	}
	row := nha032Record(t, 2, at, at, 25, domain.SemanticMeasurement)
	lost := uint64(1)
	nha032Persist(t, s, epoch, 0, 2, []RecorderRecord{row}, []RecorderGap{{From: 1, Through: 1, KnownLost: &lost, Reason: RecorderGapOverflow}}, nil)
	through := uint64(2)
	request := nha031Request(t, epoch, 0, epoch, &through)
	proof, err := readRecorderCoverageTx(ctx, ctx, tx, request, recorderCoverageProductionLimits)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM public.state_history WHERE epoch=$1`, epoch.data()).Scan(&count); err != nil || count != 0 || len(proof.Gaps()) != 0 || proof.Epochs()[0].Checkpoint() != 0 {
		t.Fatalf("mixed snapshot count=%d proof=%+v err=%v", count, proof, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	proof, err = s.ReadRecorderCoverage(ctx, request)
	if err != nil || len(proof.Gaps()) != 1 || proof.Epochs()[0].Checkpoint() != 2 {
		t.Fatalf("new snapshot missing atomic batch %+v %v", proof, err)
	}
}
func TestNHA032GapAndEpochNeverProduceCounterDelta(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch := nha031Epoch(t, 323)
	nha031Open(t, s, epoch)
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	a := nha032Record(t, 1, at, at, 100, domain.SemanticTotalIncreasing)
	b := nha032Record(t, 3, at.Add(time.Minute), at.Add(time.Minute), 150, domain.SemanticTotalIncreasing)
	lost := uint64(1)
	nha032Persist(t, s, epoch, 0, 3, []RecorderRecord{a, b}, []RecorderGap{{From: 2, Through: 2, KnownLost: &lost, Reason: RecorderGapOverflow}}, nil)
	if _, _, err := s.AggregateRecorderPrefix(ctx, nha032Boundary(t, epoch, 3, at.Add(2*time.Minute))); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ReadRecorderHourlyAggregates(ctx, "nha032-measurement", at, at.Add(time.Hour))
	if err != nil || len(rows) != 1 || rows[0].Values.Input().HasContribution || rows[0].Values.Input().Reasons&recordermaintenance.Gap == 0 {
		t.Fatalf("gap delta fabricated %+v %v", rows, err)
	}
	epoch2 := nha031Epoch(t, 324)
	nha031Open(t, s, epoch2)
	c := nha032Record(t, 1, at.Add(2*time.Minute), at.Add(2*time.Minute), 170, domain.SemanticTotalIncreasing)
	nha032Persist(t, s, epoch2, 0, 1, []RecorderRecord{c}, nil, nil)
	if _, _, err = s.AggregateRecorderPrefix(ctx, nha032Boundary(t, epoch2, 1, at.Add(3*time.Minute))); err != nil {
		t.Fatal(err)
	}
	rows, err = s.ReadRecorderHourlyAggregates(ctx, "nha032-measurement", at, at.Add(time.Hour))
	if err != nil || rows[0].Values.Input().HasContribution || rows[0].Values.Input().Reasons&recordermaintenance.EpochBoundary == 0 {
		t.Fatalf("epoch delta fabricated %+v %v", rows, err)
	}
}
func TestNHA032RawPurgeRollbackKeepsPayloadAndProofTogether(t *testing.T) {
	s := nha032SQLFixture(t)
	epoch, at := nha032SeedWeighted(t, s)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION public.nha032_reject_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'nha032 delete rollback'; END $$; CREATE TRIGGER nha032_reject_delete BEFORE DELETE ON public.state_history FOR EACH ROW EXECUTE FUNCTION public.nha032_reject_delete()`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PurgeRecorderRaw(ctx, at.Add(2*time.Hour)); err == nil {
		t.Fatal("delete failure hidden")
	}
	var raw, markers, aggregates int
	var cutoff time.Time
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM public.state_history),(SELECT count(*) FROM public.recorder_retention_ranges),(SELECT count(*) FROM public.recorder_hourly_aggregates),raw_cutoff FROM public.recorder_maintenance_head WHERE singleton=true`).Scan(&raw, &markers, &aggregates, &cutoff); err != nil || raw != 3 || markers != 0 || aggregates != 0 || !cutoff.Equal(time.Time{}) {
		t.Fatalf("non-atomic raw purge %d %d %d %v %v", raw, markers, aggregates, cutoff, err)
	}
	if cp, err := s.RecorderCheckpoint(ctx, epoch); err != nil || cp.Sequence != 3 {
		t.Fatalf("checkpoint mutated %+v %v", cp, err)
	}
}
