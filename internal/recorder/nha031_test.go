package recorder

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

func nha031PublishBurst(t *testing.T, session *integrations.Session, binding integrations.Binding, count int) {
	t.Helper()
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for i := 0; i < count; i++ {
		value := domain.NewSwitchValue(i%2 == 0)
		observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Publish(ctx, binding, observation); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
}

func TestNHA031OverflowExposesBoundedLiveLoss(t *testing.T) {
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	writer := &nha030Writer{blockSecond: make(chan struct{}), secondEntered: make(chan struct{}), secondFinished: make(chan struct{})}
	recording, err := Start(context.Background(), fixture.Sequencer(), writer, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		select {
		case <-writer.blockSecond:
		default:
			close(writer.blockSecond)
		}
	}()
	instance, err := domain.ParseIntegrationInstanceID("nha031-live")
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := fake.NewDriver(time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	binding, err := driver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-writer.secondEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("writer did not block")
	}
	nha031PublishBurst(t, session, binding, 3000)
	status, err := recording.Status()
	if err != nil {
		t.Fatal(err)
	}
	epoch, from, through, present := status.ActiveCoverage.Bounds()
	if !present || epoch != recording.epoch || from != status.Checkpoint+1 || through < from || status.LostEntries == 0 || !status.HistoryDegraded || status.QueuedEntries > MaxQueuedEntries || status.QueuedBytes > DefaultLimits().QueueBytes() {
		t.Fatalf("live at-risk coverage absent/unbounded: %+v", status)
	}
	if known, ok := status.ActiveCoverage.KnownLost(); !ok || known == 0 || known > through-from+1 {
		t.Fatalf("known evictions: %d %v in %d..%d", known, ok, from, through)
	}
	// A returned value cannot mutate the Recorder's next observation.
	status.ActiveCoverage = LiveCoverage{}
	again, err := recording.Status()
	if err != nil || !again.ActiveCoverage.present {
		t.Fatal("status shared mutable live coverage")
	}
	close(writer.blockSecond)
	if err := host.CloseAdmissions(); err != nil {
		t.Fatal(err)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := recording.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}

type nha031PGWriter struct {
	store                     *storage.Store
	mu                        sync.Mutex
	ackLost                   bool
	commits                   int
	attempts                  int
	ackBatch                  storage.RecorderBatch
	ackReturned, ackConfirmed bool
	afterAckReads             int
}

func (w *nha031PGWriter) OpenRecorderEpoch(ctx context.Context, e storage.RecorderEpoch, at time.Time) (storage.RecorderPrevious, storage.TransactionOutcome, error) {
	return w.store.OpenRecorderEpoch(ctx, e, at)
}
func (w *nha031PGWriter) PersistRecorderBatch(ctx context.Context, b storage.RecorderBatch) (storage.TransactionOutcome, error) {
	w.mu.Lock()
	w.attempts++
	w.mu.Unlock()
	outcome, err := w.store.PersistRecorderBatch(ctx, b)
	if outcome == storage.TransactionCommitted && err == nil {
		w.mu.Lock()
		w.commits++
		lose := w.ackLost
		w.ackLost = false
		if lose {
			w.ackBatch, w.ackReturned = b, true
		}
		w.mu.Unlock()
		if lose {
			return storage.TransactionIndeterminate, errors.New("ack lost after PG COMMIT")
		}
	}
	return outcome, err
}
func (w *nha031PGWriter) RecorderCheckpoint(ctx context.Context, e storage.RecorderEpoch) (storage.RecorderCheckpoint, error) {
	checkpoint, err := w.store.RecorderCheckpoint(ctx, e)
	w.mu.Lock()
	if w.ackReturned {
		w.afterAckReads++
		if err == nil && checkpoint.Confirms(w.ackBatch) {
			w.ackConfirmed = true
		}
	}
	w.mu.Unlock()
	return checkpoint, err
}
func (w *nha031PGWriter) CloseRecorderEpoch(ctx context.Context, e storage.RecorderEpoch, checkpoint uint64) (storage.TransactionOutcome, error) {
	return w.store.CloseRecorderEpoch(ctx, e, checkpoint)
}

func nha031RecorderPG(t *testing.T) (*storage.Store, int) {
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
	return store, cluster.Port
}

func TestNHA031PostgresBlockedWriteRecoveryCommitsGapAndWatermark(t *testing.T) {
	store, port := nha031RecorderPG(t)
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	writer := &nha031PGWriter{store: store}
	recording, err := Start(context.Background(), fixture.Sequencer(), writer, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = host.CloseAdmissions()
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = recording.Stop(stopCtx)
	}()
	configuration, err := pgx.ParseConfig(fmt.Sprintf("host=127.0.0.1 port=%d dbname=postgres user=homeward_test sslmode=disable", port))
	if err != nil {
		t.Fatal(err)
	}
	configuration.Password = testpg.Password()
	locker, err := pgx.ConnectConfig(context.Background(), configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locker.Close(context.Background()) })
	observer, err := pgx.ConnectConfig(context.Background(), configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observer.Close(context.Background()) })
	lockTx, err := locker.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lockTx.Rollback(context.Background()) }()
	epochText, err := recording.epoch.Hex()
	if err != nil {
		t.Fatal(err)
	}
	var locked []byte
	if err := lockTx.QueryRow(context.Background(), `SELECT epoch FROM public.recorder_epochs WHERE epoch=decode($1,'hex') FOR UPDATE`, epochText).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha031-pg-recovery")
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := fake.NewDriver(time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	binding, err := driver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for {
		var waiting bool
		if err := observer.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid<>pg_backend_pid() AND wait_event_type='Lock' AND query LIKE '%recorder_epochs%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			writer.mu.Lock()
			attempts, commits := writer.attempts, writer.commits
			writer.mu.Unlock()
			status, _ := recording.Status()
			var activity string
			_ = observer.QueryRow(context.Background(), `SELECT coalesce(string_agg(pid::text||':'||state||':'||coalesce(wait_event_type,'')||':'||left(query,100),' | '),'') FROM pg_stat_activity WHERE pid<>pg_backend_pid() AND datname=current_database()`).Scan(&activity)
			t.Fatalf("Recorder did not wait on the real PostgreSQL row lock: attempts=%d commits=%d phase=%v last=%v activity=%s", attempts, commits, status.Phase, status.LastError, activity)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The lock outlives an actual 2s PG attempt timeout; the Recorder must
	// retry against PostgreSQL while the transaction is still blocked.
	deadline = time.Now().Add(4 * time.Second)
	for {
		writer.mu.Lock()
		attempts, commits := writer.attempts, writer.commits
		writer.mu.Unlock()
		if attempts >= 3 && commits == 1 {
			break
		} // initial image + two blocked attempts
		if time.Now().After(deadline) {
			t.Fatalf("PG lock did not cause a real timeout/retry: attempts=%d commits=%d", attempts, commits)
		}
		time.Sleep(10 * time.Millisecond)
	}
	nha031PublishBurst(t, session, binding, 3000)
	request, err := storage.NewRecorderCoverageRequest(recording.epoch, 0, recording.epoch, nil)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.ReadRecorderCoverage(context.Background(), request)
	if err != nil || before.Kind() != storage.RecorderCoverageReadComplete || len(before.Gaps()) != 0 {
		t.Fatalf("pre-recovery: %#v %v", before, err)
	}
	if err := lockTx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := host.CloseAdmissions(); err != nil {
		t.Fatal(err)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := recording.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	after, err := store.ReadRecorderCoverage(context.Background(), request)
	if err != nil || len(after.Gaps()) == 0 {
		t.Fatalf("recovery gap absent: %#v %v", after, err)
	}
	_, _, _, through := after.Bounds()
	if through == 0 || through != recording.status.Checkpoint {
		t.Fatalf("watermark mismatch: %d vs %d", through, recording.status.Checkpoint)
	}
}

func TestNHA031LostCommitAcknowledgementReadsBeforeRetry(t *testing.T) {
	store, _ := nha031RecorderPG(t)
	epoch, err := storage.ParseRecorderEpoch("abababababababababababababababab")
	if err != nil {
		t.Fatal(err)
	}
	writer := &nha031PGWriter{store: store}
	if _, outcome, err := writer.OpenRecorderEpoch(context.Background(), epoch, time.Now()); outcome != storage.TransactionCommitted || err != nil {
		t.Fatal(err)
	}
	initial, err := storage.NewRecorderBatch(epoch, 0, 0, nil, nil, 0, &storage.RecorderImage{Boundary: 0})
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := writer.PersistRecorderBatch(context.Background(), initial); outcome != storage.TransactionCommitted || err != nil {
		t.Fatal(err)
	}
	one := uint64(1)
	gap, err := storage.NewRecorderBatch(epoch, 0, 1, nil, []storage.RecorderGap{{From: 1, Through: 1, KnownLost: &one, Reason: storage.RecorderGapOverflow, UncertainEnvelope: true}}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	writer.mu.Lock()
	writer.ackLost = true
	writer.mu.Unlock()
	r := &Recorder{writer: writer, epoch: epoch, readCtx: context.Background(), status: Status{Phase: PhaseRunning}}
	if err := r.write(gap); err != nil {
		t.Fatal(err)
	}
	writer.mu.Lock()
	commits, attempts, afterAckReads, ackConfirmed := writer.commits, writer.attempts, writer.afterAckReads, writer.ackConfirmed
	writer.mu.Unlock()
	if commits != 2 || attempts != 2 || afterAckReads < 1 || !ackConfirmed {
		t.Fatalf("ACK was not resolved by real checkpoint read before return: commits=%d attempts=%d reads=%d confirms=%v", commits, attempts, afterAckReads, ackConfirmed)
	}
	request, err := storage.NewRecorderCoverageRequest(epoch, 0, epoch, nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ReadRecorderCoverage(context.Background(), request)
	if err != nil || len(snapshot.Gaps()) != 1 {
		t.Fatalf("committed gap missing or duplicated: %#v %v", snapshot, err)
	}
}
