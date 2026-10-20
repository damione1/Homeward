package executionhistory

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/scriptcommands"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

type nha186NilContext struct{ context.Context }

func TestNHA186CloseBeforeWorkerDoesNotClaimDurableHistory(t *testing.T) {
	var zero Sink
	if err := zero.Start(); !errors.Is(err, storage.ErrHistoryInvalid) {
		t.Fatalf("zero sink worker admitted: %v", err)
	}
	if err := zero.Close(context.Background()); !errors.Is(err, storage.ErrHistoryInvalid) {
		t.Fatalf("zero sink close admitted: %v", err)
	}
	empty := nha186Sink(t)
	if err := empty.Close(context.Background()); err != nil || empty.Status().Undrained {
		t.Fatalf("empty prepared sink: %v %+v", err, empty.Status())
	}
	sink := nha186Sink(t)
	sink.ObserveCommand(nha186Observation())
	if err := sink.Close(context.Background()); !errors.Is(err, storage.ErrHistoryUnavailable) {
		t.Fatalf("accepted history silently lost: %v", err)
	}
	status := sink.Status()
	if !status.Undrained || !status.Degraded || status.QueueRecords != 1 || status.CommittedOrdinal != 0 {
		t.Fatalf("false persistence acknowledgement: %+v", status)
	}
	select {
	case <-sink.Done():
	default:
		t.Fatal("unstarted sink did not finish")
	}
}

func TestNHA186HistoryRejectsTypedNilContextAndSQLPositionOverflow(t *testing.T) {
	var invalid *nha186NilContext
	if validContext(invalid) {
		t.Fatal("typed nil context admitted")
	}
	if _, err := New(invalid, &storage.Store{}, "0123456789abcdef0123456789abcdef"); !errors.Is(err, storage.ErrHistoryInvalid) {
		t.Fatalf("nil constructor context: %v", err)
	}
	sink := nha186Sink(t)
	if err := sink.Close(invalid); !errors.Is(err, storage.ErrHistoryInvalid) {
		t.Fatalf("nil close context: %v", err)
	}
	observation := nha186Observation()
	observation.Source.Line = 1 << 31
	sink.ObserveCommand(observation)
	if sink.Status().QueueRecords != 0 || sink.Status().Rejected[storage.HistoryLossInvalidMetadata] != 1 {
		t.Fatal("SQL integer overflow admitted")
	}
}

func nha186Observation() scriptcommands.CommandObservation {
	id, _ := domain.ParseCommandID("nha186-synthetic-command")
	return scriptcommands.CommandObservation{At: time.Now().UTC(), Run: automationruns.HistoryRunRef{Generation: 1, RunID: 1, Provenance: automationruns.FixtureProvenance()}, NodeVisit: 1, CommandOrdinal: 1, ID: id, Outcome: scriptcommands.CommandDispatched}
}

func TestNHA186QueueCountBytesOversizePrecopyAndMemoryPlateau(t *testing.T) {
	for _, large := range []bool{false, true} {
		t.Run(fmt.Sprintf("large=%t", large), func(t *testing.T) {
			sink := nha186Sink(t)
			observation := nha186Observation()
			if large {
				observation.Source = automationruns.HistorySource{Object: strings.Repeat("o", 256), File: strings.Repeat("f", 512), Path: strings.Repeat("p", 1024)}
			}
			emit := func(count int) {
				for range count {
					sink.ObserveCommand(observation)
				}
			}
			emit(10000)
			first := sink.Status()
			if first.QueueRecords == 0 || first.QueueRecords > maxQueueRecords || first.QueueBytes > maxQueueBytes || !first.Degraded {
				t.Fatalf("bounds: %+v", first)
			}
			if !large && first.QueueRecords != maxQueueRecords {
				t.Fatalf("record limit not exercised: %+v", first)
			}
			if large && first.QueueRecords >= maxQueueRecords {
				t.Fatalf("byte limit not exercised: %+v", first)
			}
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			started := time.Now()
			emit(100000)
			elapsed := time.Since(started)
			runtime.GC()
			runtime.ReadMemStats(&after)
			second := sink.Status()
			if second.QueueBytes != first.QueueBytes || second.QueueRecords != first.QueueRecords {
				t.Fatal("queue grew beyond saturation")
			}
			if after.HeapAlloc > before.HeapAlloc+1<<20 {
				t.Fatalf("100k retained growth=%d", after.HeapAlloc-before.HeapAlloc)
			}
			if elapsed > 5*time.Second {
				t.Fatalf("100k bounded admissions took %s", elapsed)
			}
			if second.Rejected[storage.HistoryLossQueueFull] != uint64(110000-second.QueueRecords) {
				t.Fatal("known rejection count differs from actual attempts")
			}
			t.Logf("10k/100k plateau records=%d bytes=%d heap-before=%d heap-after=%d elapsed=%s", second.QueueRecords, second.QueueBytes, before.HeapAlloc, after.HeapAlloc, elapsed)
			observation.Source.Path = strings.Repeat("private-large-backing", 1<<20)
			allocations := testing.AllocsPerRun(20, func() { sink.ObserveCommand(observation) })
			if allocations != 0 || sink.Status().QueueBytes != first.QueueBytes {
				t.Fatalf("oversized metadata copied: allocations=%f", allocations)
			}
		})
	}
}

func TestNHA186BlockedPostgresDoesNotBlockProducerAndPersistsLoss(t *testing.T) {
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
	var nilContext *nha186NilContext
	epoch, _ := storage.ParseHistoryEpoch("0123456789abcdef0123456789abcdef")
	if _, err := store.OpenExecutionHistoryEpoch(nilContext, epoch); !errors.Is(err, storage.ErrHistoryInvalid) {
		t.Fatalf("storage typed nil context: %v", err)
	}
	if _, err := store.ReadExecutionHistoryPage(nilContext, storage.HistoryPageQuery{Epoch: epoch, Limit: 1}); !errors.Is(err, storage.ErrHistoryInvalid) {
		t.Fatalf("storage read typed nil context: %v", err)
	}
	if _, err := store.PurgeExecutionHistory(nilContext, time.Now().UTC()); !errors.Is(err, storage.ErrHistoryInvalid) {
		t.Fatalf("storage purge typed nil context: %v", err)
	}
	sink, err := New(ctx, store, "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = sink.Close(stop)
	}()
	connect := func() *pgx.Conn {
		conn, err := pgx.Connect(ctx, fmt.Sprintf("host=127.0.0.1 port=%d dbname=postgres user=homeward_test password=%s sslmode=disable", cluster.Port, testpg.Password()))
		if err != nil {
			t.Fatal("isolated connection", err)
		}
		return conn
	}
	blocker, observer := connect(), connect()
	defer blocker.Close(ctx)
	defer observer.Close(ctx)
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var cutoff time.Time
	if err := tx.QueryRow(ctx, `SELECT retention_cutoff FROM public.execution_history_head WHERE singleton=true FOR UPDATE`).Scan(&cutoff); err != nil {
		t.Fatal(err)
	}
	observation := nha186Observation()
	for range 128 {
		sink.ObserveCommand(observation)
	}
	deadline := time.Now().Add(5 * time.Second)
	locked := false
	for time.Now().Before(deadline) {
		var waiting int
		if err := observer.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE '%execution_history_head%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting != 0 {
			locked = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !locked {
		t.Fatal("writer was never observed blocked in PostgreSQL")
	}
	started := time.Now()
	for range 10000 {
		sink.ObserveCommand(observation)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("simulated producer waited on SQL: %s", elapsed)
	}
	status := sink.Status()
	if !status.Pending || status.QueueRecords > 1024 || status.QueueBytes > 4<<20 || !status.Degraded {
		t.Fatalf("blocked status=%+v", status)
	}
	var rejected uint64
	for _, count := range status.Rejected {
		rejected += count
	}
	if rejected == 0 {
		t.Fatal("saturation loss missing")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	stop, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	if err := sink.Close(stop); err != nil {
		t.Fatal(err)
	}
	page, err := store.ReadExecutionHistoryPage(ctx, storage.HistoryPageQuery{Epoch: sink.epoch, Limit: 128})
	if err != nil {
		t.Fatal(err)
	}
	var durableLoss uint64
	for _, loss := range page.Losses {
		durableLoss += loss.KnownCount
		if !loss.UncertainEnvelope {
			t.Fatal("loss falsely localized to trace interval")
		}
	}
	final := sink.Status()
	var finalRejected uint64
	for _, count := range final.Rejected {
		finalRejected += count
	}
	if durableLoss != finalRejected || durableLoss < rejected || final.QueueRecords != 0 || final.Pending {
		t.Fatalf("recovery loss=%d want=%d status=%+v", durableLoss, finalRejected, final)
	}
	t.Logf("observed PG lock; 10k producer attempts=%s; durable loss=%d", time.Since(started), durableLoss)
}
