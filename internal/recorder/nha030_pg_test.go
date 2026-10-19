package recorder

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The real transaction commits; only the caller-visible acknowledgement is
// deliberately lost at this narrow Writer boundary.
type nha030LostAckWriter struct {
	*storage.Store
	mu     sync.Mutex
	writes int
	reads  int
	lost   bool
}

func (w *nha030LostAckWriter) PersistRecorderBatch(ctx context.Context, batch storage.RecorderBatch) (storage.TransactionOutcome, error) {
	outcome, err := w.Store.PersistRecorderBatch(ctx, batch)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	if outcome == storage.TransactionCommitted && err == nil && batch.Through() > 0 && !w.lost {
		w.lost = true
		return storage.TransactionIndeterminate, errors.New("simulated lost acknowledgement after PostgreSQL commit")
	}
	return outcome, err
}

func (w *nha030LostAckWriter) RecorderCheckpoint(ctx context.Context, epoch storage.RecorderEpoch) (storage.RecorderCheckpoint, error) {
	checkpoint, err := w.Store.RecorderCheckpoint(ctx, epoch)
	w.mu.Lock()
	w.reads++
	w.mu.Unlock()
	return checkpoint, err
}

func TestNHA030CommittedPostgresBatchWithLostAcknowledgementIsReadBeforeRetry(t *testing.T) {
	cluster := testpg.Start(t)
	settings, err := storage.ParseSettings(storage.ConnectionInput{
		Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable",
	}, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	writer := &nha030LostAckWriter{Store: store}
	recording, err := Start(ctx, fixture.Sequencer(), writer, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha030-lost-ack")
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
	if err := driver.Start(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := host.CloseAdmissions(); err != nil {
		t.Fatal(err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	if err := recording.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	writer.mu.Lock()
	writes, reads, lost := writer.writes, writer.reads, writer.lost
	writer.mu.Unlock()
	status, err := recording.Status()
	if err != nil || writes != 2 || reads != 1 || !lost || status.Checkpoint == 0 {
		t.Fatalf("committed batch was replayed or acknowledgement was trusted without read: writes=%d reads=%d lost=%v status=%+v err=%v", writes, reads, lost, status, err)
	}
}

type nha030Generation struct{}

func (nha030Generation) WithCurrent(ctx context.Context, fn func(uint64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(7)
}

// This test deliberately relies on the production migration. It qualifies
// the Run-era schema only after recorderSchemaSQL joins storage.Migrate.
func TestNHA030PostgresLockDoesNotBlockHostAdmission(t *testing.T) {
	cluster := testpg.Start(t)
	settings, err := storage.ParseSettings(storage.ConnectionInput{
		Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable",
	}, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	connect := func() *pgx.Conn {
		t.Helper()
		config, err := pgx.ParseConfig("host=127.0.0.1 port=" + strconv.Itoa(cluster.Port) + " dbname=postgres user=homeward_test sslmode=disable")
		if err != nil {
			t.Fatal(err)
		}
		config.Password = testpg.Password()
		conn, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close(context.Background()) })
		return conn
	}
	controller, observer := connect(), connect()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	recording, err := Start(ctx, fixture.Sequencer(), store, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer stopCancel()
		if err := recording.Stop(stopCtx); err != nil {
			t.Error(err)
		}
	})
	var released sync.Once
	lock, err := controller.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	release := func() { released.Do(func() { _ = lock.Rollback(context.Background()) }) }
	t.Cleanup(release)
	if _, err := lock.Exec(ctx, `LOCK TABLE public.state_history IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	var controllerPID int
	if err := lock.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&controllerPID); err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha030-pg-barrier")
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("fixture-controller", "relay")
	if err != nil {
		t.Fatal(err)
	}
	identities, err := fixture.Resolve(ctx, instance, []integrations.SourceKey{key})
	if err != nil || len(identities) != 1 {
		t.Fatalf("fake identity: %v", err)
	}
	alias, err := domain.ParseHAAlias("switch.nha030_pg_barrier")
	if err != nil {
		t.Fatal(err)
	}
	record, err := integrations.NewRegistryRecord(identities[0].Device, identities[0].Entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := fake.NewActionDriver(time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := host.AttachCommandDriver(session, driver); err != nil {
		t.Fatal(err)
	}
	volatile, err := integrations.NewVolatileCommandService(host, nha030Generation{}, integrations.DefaultVolatileCommandLimits())
	if err != nil {
		t.Fatal(err)
	}
	start, err := fixture.Sequencer().Start()
	if err != nil {
		t.Fatal(err)
	}
	epoch, _, err := start.Components()
	if err != nil {
		t.Fatal(err)
	}
	entity, err := identities[0].Entity.Value()
	if err != nil {
		t.Fatal(err)
	}
	actor, err := domain.ParseActorID("nha030-local-operator")
	if err != nil {
		t.Fatal(err)
	}
	request := &actions.CommandRequest{
		CommandId: "nha030-volatile-during-pg-lock", AdmissionEpoch: epoch, ConfigRevision: 7,
		Target:             &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_EntityId{EntityId: entity}},
		CapabilitySchemaId: contracts.SwitchSchema, Endpoint: "relay",
		Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: true}}},
		ExpiresAt: timestamppb.New(time.Now().Add(time.Minute)),
	}
	binding, err := driver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	blocked := false
	barrierDeadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(barrierDeadline) {
		if err := observer.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM pg_stat_activity a
			WHERE a.wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(a.pid))
			  AND a.query LIKE 'INSERT INTO public.state_history%'
		)`, controllerPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case <-recording.Done():
			t.Fatal("Recorder worker exited before reaching PostgreSQL lock")
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("Recorder writer never waited behind the observed PostgreSQL lock")
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	observedAt := time.Date(2026, 10, 18, 12, 0, 1, 0, time.UTC)
	publish := func(i int) error {
		value := domain.NewSwitchValue(i%2 == 0)
		observation, err := integrations.NewStateObservation("relay", quality, &value, &observedAt, observedAt, nil)
		if err != nil {
			return err
		}
		return session.Publish(ctx, binding, observation)
	}
	progress := make(chan error, 1)
	go func() { progress <- publish(0) }()
	select {
	case err := <-progress:
		if err != nil {
			t.Fatalf("Host admission failed behind SQL lock: %v", err)
		}
	case <-time.After(time.Second):
		release()
		t.Fatal("Host admission waited for Recorder SQL")
	}
	commandProgress := make(chan error, 1)
	go func() {
		response, err := volatile.Dispatch(ctx, actor, request)
		if err == nil && (response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED) {
			err = integrations.ErrInvalidInput
		}
		commandProgress <- err
	}()
	select {
	case err := <-commandProgress:
		if err != nil || len(driver.Commands()) != 1 {
			t.Fatalf("volatile command did not dispatch before SQL release: %v calls=%d", err, len(driver.Commands()))
		}
	case <-time.After(time.Second):
		release()
		t.Fatal("volatile command waited for Recorder SQL")
	}
	for i := 1; i < 10000; i++ {
		if err := publish(i); err != nil {
			t.Fatalf("Host admission %d failed while SQL locked: %v", i, err)
		}
	}
	status, err := recording.Status()
	if err != nil || status.QueuedBytes > status.QueueCapacityBytes || status.QueuedEntries > MaxQueuedEntries || status.Checkpoint != 0 || status.LostEntries == 0 {
		t.Fatalf("Recorder advanced or exceeded bounds behind SQL lock: %+v %v", status, err)
	}
	release()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer stopCancel()
	if err := recording.Stop(stopCtx); err != nil {
		t.Fatalf("Recorder failed after SQL lock release: %v", err)
	}
}
