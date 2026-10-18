package integrations_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
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
	"google.golang.org/protobuf/proto"
)

func nha024Store(t *testing.T) (*storage.Store, storage.Settings) {
	store, settings, _ := nha024StoreCluster(t)
	return store, settings
}

func nha024StoreCluster(t *testing.T) (*storage.Store, storage.Settings, testpg.Cluster) {
	t.Helper()
	cluster := testpg.Start(t)
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(name string) (string, bool) {
		return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD"
	})
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
	return store, settings, cluster
}

func TestNHA024PostgresShutdownRefusesNewDurableCommand(t *testing.T) {
	store, settings, cluster := nha024StoreCluster(t)
	rig := nha023SetupBare(t, &nha022Driver{})
	service, err := integrations.NewDurableCommandService(context.Background(), rig.host, rig.gen, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = service.Close(ctx)
	})
	if err := cluster.Stop(); err != nil {
		t.Fatal(err)
	}
	if got, err := service.Dispatch(context.Background(), nha022Actor(t), rig.request); got != nil || !errors.Is(err, integrations.ErrDurableLedgerUnavailable) || rig.driver.count() != 0 {
		t.Fatalf("PG shutdown = %v %v sends=%d", got, err, rig.driver.count())
	}
	if _, err := storage.Open(context.Background(), settings); err == nil {
		t.Fatal("cold start opened unavailable PostgreSQL")
	}
	var releaseErr *storage.RuntimeReleaseError
	if err := service.Close(context.Background()); !errors.As(err, &releaseErr) {
		t.Fatalf("closed PostgreSQL falsely confirmed runtime release: %v", err)
	}
	volatile, err := integrations.NewVolatileCommandService(rig.host, rig.gen, integrations.DefaultVolatileCommandLimits())
	if err != nil {
		t.Fatal(err)
	}
	loaded := proto.Clone(rig.request).(*actions.CommandRequest)
	loaded.CommandId = "loaded-volatile-after-pg-down"
	response, err := volatile.Dispatch(context.Background(), nha022Actor(t), loaded)
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || rig.driver.count() != 1 {
		t.Fatalf("loaded volatile route under PG outage = %v %v sends=%d", response, err, rig.driver.count())
	}
}

func nha024Service(t *testing.T, rig nha023Rig, store *storage.Store) *integrations.DurableCommandService {
	t.Helper()
	service, err := integrations.NewDurableCommandService(context.Background(), rig.host, rig.gen, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := service.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return service
}

func nha024ID(t *testing.T, value string) domain.CommandID {
	t.Helper()
	id, err := domain.ParseCommandID(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func nha024Exec(t *testing.T, store *storage.Store, sql string) {
	t.Helper()
	if err := store.InTransaction(context.Background(), func(tx pgx.Tx) error { _, err := tx.Exec(context.Background(), sql); return err }); err != nil {
		t.Fatal(err)
	}
}

func TestNHA024AcceptedBeforeFakeDispatchAndDedup(t *testing.T) {
	store, _ := nha024Store(t)
	var observed atomic.Bool
	driver := &nha022Driver{onDispatch: func() error {
		record, found, err := store.CommandIntent(context.Background(), nha024ID(t, "command-1"))
		if err != nil || !found || record.State() != storage.IntentDispatching {
			return fmt.Errorf("driver saw no committed dispatching intention: %v %t %v", record.State(), found, err)
		}
		observed.Store(true)
		return nil
	}}
	rig := nha023SetupBare(t, driver)
	service := nha024Service(t, rig, store)
	actor := nha022Actor(t)
	first, err := service.Dispatch(context.Background(), actor, rig.request)
	if err != nil || first.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || !observed.Load() || driver.count() != 1 {
		t.Fatalf("dispatch = %v %v count=%d", first, err, driver.count())
	}
	replay, err := service.Dispatch(context.Background(), actor, rig.request)
	if err != nil || replay.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || driver.count() != 1 {
		t.Fatalf("replay = %v %v count=%d", replay, err, driver.count())
	}
	changed := proto.Clone(rig.request).(*actions.CommandRequest)
	changed.ConfigRevision++
	if got, err := service.Dispatch(context.Background(), actor, changed); got != nil || !errors.Is(err, integrations.ErrCommandIDConflict) {
		t.Fatalf("conflict = %v %v", got, err)
	}
	other, _ := domain.ParseActorID("different-actor")
	if got, err := service.Dispatch(context.Background(), other, rig.request); got != nil || !errors.Is(err, integrations.ErrCommandIDConflict) {
		t.Fatalf("actor conflict = %v %v", got, err)
	}
	status, found, err := service.Status(context.Background(), nha024ID(t, "command-1"))
	if err != nil || !found || status.State != storage.IntentResult || status.Response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("status = %+v %t %v", status, found, err)
	}
}

func TestNHA024ConcurrentSameIDHasOneFakeDriverEntry(t *testing.T) {
	store, _ := nha024Store(t)
	driver := &nha022Driver{entered: make(chan struct{}), release: make(chan struct{})}
	rig := nha023SetupBare(t, driver)
	service := nha024Service(t, rig, store)
	firstDone := make(chan error, 1)
	go func() {
		_, err := service.Dispatch(context.Background(), nha022Actor(t), rig.request)
		firstDone <- err
	}()
	select {
	case <-driver.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first driver entry absent")
	}
	second, err := service.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if !errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) || second == nil || second.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || driver.count() != 0 {
		t.Fatalf("pending duplicate = %v %v calls=%d", second, err, driver.count())
	}
	close(driver.release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	third, err := service.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if err != nil || third == nil || third.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || driver.count() != 1 {
		t.Fatalf("completed duplicate = %v %v calls=%d", third, err, driver.count())
	}
}

func TestNHA024OversizedIntentAndZeroServiceAreRejected(t *testing.T) {
	store, _ := nha024Store(t)
	rig := nha023SetupBare(t, &nha022Driver{})
	service := nha024Service(t, rig, store)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := integrations.NewDurableCommandService(canceled, rig.host, rig.gen, store); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled startup = %v", err)
	}
	tooLarge := proto.Clone(rig.request).(*actions.CommandRequest)
	tooLarge.Arguments = &actions.ActionArgs{Arguments: &actions.ActionArgs_Extension{Extension: &actions.ExtensionPayload{SchemaId: "vendor.synthetic.v1", ProtobufPayload: make([]byte, 65<<10)}}}
	if got, err := service.Dispatch(context.Background(), nha022Actor(t), tooLarge); got != nil || !errors.Is(err, integrations.ErrCommandIntentTooLarge) || rig.driver.count() != 0 {
		t.Fatalf("oversized = %v %v", got, err)
	}
	_, found, err := service.Status(context.Background(), nha024ID(t, "command-1"))
	if err != nil || found {
		t.Fatalf("oversized intention persisted: %t %v", found, err)
	}
	var zero integrations.DurableCommandService
	if got, err := zero.Dispatch(context.Background(), nha022Actor(t), rig.request); got != nil || !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("zero service Dispatch = %v %v", got, err)
	}
	if _, _, err := zero.Status(context.Background(), nha024ID(t, "command-1")); !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("zero service Status = %v", err)
	}
	if err := zero.Close(context.Background()); !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("zero service Close = %v", err)
	}
}

func TestNHA024DeferredCommitFailureAndUnavailableRefuseBeforeDispatch(t *testing.T) {
	store, _ := nha024Store(t)
	driver := &nha022Driver{}
	rig := nha023SetupBare(t, driver)
	service := nha024Service(t, rig, store)
	nha024Exec(t, store, `CREATE FUNCTION public.nha024_refuse_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'deferred failure'; END $$;
		CREATE CONSTRAINT TRIGGER nha024_refuse_commit AFTER INSERT ON public.command_intents DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha024_refuse_commit()`)
	got, err := service.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if got != nil || !errors.Is(err, integrations.ErrDurableCommitUnknown) || driver.count() != 0 {
		t.Fatalf("failed commit = %v %v count=%d", got, err, driver.count())
	}
	_, found, err := store.CommandIntent(context.Background(), nha024ID(t, "command-1"))
	if err != nil || found {
		t.Fatalf("rolled-back intent found=%t err=%v", found, err)
	}
	nha024Exec(t, store, `DROP TRIGGER nha024_refuse_commit ON public.command_intents; DROP FUNCTION public.nha024_refuse_commit(); DROP TABLE public.command_intents`)
	got, err = service.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if got != nil || !errors.Is(err, integrations.ErrDurableLedgerUnavailable) || driver.count() != 0 {
		t.Fatalf("unavailable ledger = %v %v count=%d", got, err, driver.count())
	}
}

func TestNHA024CrashAfterFakeDispatchRecoversWithoutReplay(t *testing.T) {
	store, settings := nha024Store(t)
	var entered atomic.Int32
	driver := &nha022Driver{onDispatch: func() error { entered.Add(1); panic("fake process crash after driver entry") }}
	rig := nha023SetupBare(t, driver)
	service := nha024Service(t, rig, store)
	actor := nha022Actor(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected fake crash")
			}
		}()
		_, _ = service.Dispatch(context.Background(), actor, rig.request)
	}()
	if entered.Load() != 1 {
		t.Fatal("fake driver not entered")
	}
	before, found, err := service.Status(context.Background(), nha024ID(t, "command-1"))
	if err != nil || !found || before.State != storage.IntentDispatching {
		t.Fatalf("pre-restart = %+v %t %v", before, found, err)
	}
	secondRig := nha023SetupBare(t, &nha022Driver{})
	if _, err := integrations.NewDurableCommandService(context.Background(), secondRig.host, secondRig.gen, store); !errors.Is(err, integrations.ErrDurableRuntimeBusy) {
		t.Fatalf("live runtime recovery accepted: %v", err)
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	second := nha024Service(t, secondRig, reopened)
	defer second.Close(context.Background())
	after, found, err := second.Status(context.Background(), nha024ID(t, "command-1"))
	if err != nil || !found || after.State != storage.IntentIndeterminate || after.Response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
		t.Fatalf("recovered = %+v %t %v", after, found, err)
	}
	result, err := second.Dispatch(context.Background(), actor, rig.request)
	if !errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) || result.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || entered.Load() != 1 {
		t.Fatalf("old retry = %v %v sends=%d", result, err, entered.Load())
	}
}

func TestNHA024DispatchingCommitPrecedesFinalFence(t *testing.T) {
	store, _ := nha024Store(t)
	driver := &nha022Driver{}
	rig := nha023SetupBare(t, driver)
	service := nha024Service(t, rig, store)
	nha024Exec(t, store, `CREATE FUNCTION public.nha024_pause_dispatching() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.state = 'dispatching' THEN PERFORM pg_advisory_xact_lock(9024, 1); END IF; RETURN NEW; END $$;
		CREATE TRIGGER nha024_pause_dispatching BEFORE UPDATE ON public.command_intents FOR EACH ROW EXECUTE FUNCTION public.nha024_pause_dispatching()`)
	locked := make(chan struct{})
	release := make(chan struct{})
	blocker := make(chan error, 1)
	go func() {
		blocker <- store.InTransaction(context.Background(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(context.Background(), `SELECT pg_advisory_xact_lock(9024, 1)`); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	result := make(chan error, 1)
	go func() { _, err := service.Dispatch(context.Background(), nha022Actor(t), rig.request); result <- err }()
	deadline := time.After(5 * time.Second)
	for {
		record, found, _ := store.CommandIntent(context.Background(), nha024ID(t, "command-1"))
		if found && record.State() == storage.IntentAccepted {
			break
		}
		select {
		case <-deadline:
			t.Fatal("accepted commit not observed")
		case <-time.After(10 * time.Millisecond):
		}
	}
	rig.gen.mu.Lock()
	rig.gen.revision = 8
	rig.gen.mu.Unlock()
	close(release)
	if err := <-blocker; err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if driver.count() != 0 {
		t.Fatalf("stale generation dispatched %d times", driver.count())
	}
	status, found, err := service.Status(context.Background(), nha024ID(t, "command-1"))
	if err != nil || !found || status.State != storage.IntentResult || status.Response.Status != actions.ActionStatus_ACTION_STATUS_FAILED {
		t.Fatalf("stale result = %+v %t %v", status, found, err)
	}
}

func TestNHA024CloseDeadlineKeepsLiveRuntimeGuard(t *testing.T) {
	store, _ := nha024Store(t)
	driver := &nha022Driver{entered: make(chan struct{}), release: make(chan struct{})}
	rig := nha023SetupBare(t, driver)
	service := nha024Service(t, rig, store)
	finished := make(chan error, 1)
	go func() { _, err := service.Dispatch(context.Background(), nha022Actor(t), rig.request); finished <- err }()
	select {
	case <-driver.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("fake driver did not start")
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := service.Close(closeCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close deadline = %v", err)
	}
	if got, err := service.Dispatch(context.Background(), nha022Actor(t), rig.request); got != nil || !errors.Is(err, integrations.ErrDurableLedgerUnavailable) {
		t.Fatalf("new admission after Close = %v %v", got, err)
	}
	secondRig := nha023SetupBare(t, &nha022Driver{})
	if _, err := integrations.NewDurableCommandService(context.Background(), secondRig.host, secondRig.gen, store); !errors.Is(err, integrations.ErrDurableRuntimeBusy) {
		t.Fatalf("recovery while driver alive = %v", err)
	}
	close(driver.release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := nha024Service(t, secondRig, store)
	status, found, err := second.Status(context.Background(), nha024ID(t, "command-1"))
	if err != nil || !found || status.State != storage.IntentResult {
		t.Fatalf("post-close status = %+v %t %v", status, found, err)
	}
}

func TestNHA024CloseCanceledIdleEventuallyReleasesGuard(t *testing.T) {
	store, settings := nha024Store(t)
	rig := nha023SetupBare(t, &nha022Driver{})
	service := nha024Service(t, rig, store)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.Close(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled idle Close = %v", err)
	}
	if got, err := service.Dispatch(context.Background(), nha022Actor(t), rig.request); got != nil || !errors.Is(err, integrations.ErrDurableLedgerUnavailable) {
		t.Fatalf("admission after closing = %v %v", got, err)
	}
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
	secondConnection, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer secondConnection.Close()
	secondRig := nha023SetupBare(t, &nha022Driver{})
	second := nha024Service(t, secondRig, secondConnection)
	defer second.Close(context.Background())
}

func TestNHA024RuntimeGuardBoundToStoreAndPhysicalSession(t *testing.T) {
	first, settings := nha024Store(t)
	other, _ := nha024Store(t)
	runtime, err := first.AcquireCommandRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	encoded, _ := proto.Marshal(&actions.CommandResponse{Status: actions.ActionStatus_ACTION_STATUS_INDETERMINATE})
	if _, err := other.RecoverCommandIntents(context.Background(), runtime, encoded); err == nil {
		t.Fatal("cross-database runtime guard admitted")
	}
	secondConnection, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer secondConnection.Close()
	if _, err := secondConnection.AcquireCommandRuntime(context.Background()); err == nil {
		t.Fatal("second session acquired a live runtime")
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("runtime release: %v", err)
	}
	reacquired, err := secondConnection.AcquireCommandRuntime(context.Background())
	if err != nil {
		t.Fatalf("physical session lock not released: %v", err)
	}
	if err := reacquired.Close(); err != nil {
		t.Fatalf("successor release: %v", err)
	}
}

func TestNHA024TypedBusyResultReplaysAcrossRestart(t *testing.T) {
	store, settings := nha024Store(t)
	driver := &nha022Driver{entered: make(chan struct{}), release: make(chan struct{})}
	rig := nha023SetupBare(t, driver)
	service := nha024Service(t, rig, store)
	router, err := integrations.NewCommandRouter(rig.host, rig.gen)
	if err != nil {
		t.Fatal(err)
	}
	holder := proto.Clone(rig.request).(*actions.CommandRequest)
	holder.CommandId = "volatile-holder"
	holderDone := make(chan error, 1)
	go func() { _, err := router.Dispatch(context.Background(), nha022Actor(t), holder); holderDone <- err }()
	select {
	case <-driver.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("lease holder absent")
	}
	first, err := service.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if !errors.Is(err, integrations.ErrCommandRouteBusy) || first == nil || first.Status != actions.ActionStatus_ACTION_STATUS_FAILED || driver.count() != 0 {
		t.Fatalf("busy = %v %v calls=%d", first, err, driver.count())
	}
	close(driver.release)
	if err := <-holderDone; err != nil {
		t.Fatal(err)
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	secondRig := nha023SetupBare(t, &nha022Driver{})
	second := nha024Service(t, secondRig, reopened)
	defer second.Close(context.Background())
	replay, err := second.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if !errors.Is(err, integrations.ErrCommandRouteBusy) || replay == nil || replay.Status != first.Status || secondRig.driver.count() != 0 {
		t.Fatalf("busy replay = %v %v calls=%d", replay, err, secondRig.driver.count())
	}
}

type nha024SyntheticCodec struct{}

func (nha024SyntheticCodec) Validate(message proto.Message) error {
	field := message.ProtoReflect().Descriptor().Fields().ByName("level")
	if field == nil || message.ProtoReflect().Get(field).Int() != 5 {
		return integrations.ErrInvalidInput
	}
	return nil
}

func nha024ExtensionRig(t *testing.T, schema string, driver *nha022Driver) nha023Rig {
	t.Helper()
	ctx := context.Background()
	_, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(fixture, fixture, catalog)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../contracts/golden/extension-description.bin")
	if err != nil {
		t.Fatal(err)
	}
	description := new(wireregistry.CapabilityDescription)
	if err := proto.Unmarshal(data, description); err != nil {
		t.Fatal(err)
	}
	description.SchemaId = schema
	description.DisplayName = "Synthetic " + schema
	if err := catalog.RegisterExtensionCodec(description, nha024SyntheticCodec{}); err != nil {
		t.Fatal(err)
	}
	instance := nha016Instance(t, "nha024-synthetic")
	key, err := integrations.NewSourceKey("fake-controller", "fake-action")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := fixture.Resolve(ctx, instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{nha022Record(t, ids[0], instance, key, "switch.synthetic", "switch.synthetic", false)}); err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := integrations.NewCandidate(key, []*wireregistry.CapabilityDescription{description})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Discover(ctx, []integrations.Candidate{candidate}); err != nil {
		t.Fatal(err)
	}
	if err := host.AttachCommandDriver(session, driver); err != nil {
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
	args := &actions.ActionArgs{Arguments: &actions.ActionArgs_Extension{Extension: &actions.ExtensionPayload{SchemaId: schema, Major: 1, DescriptorSha256: description.DescriptorSha256, ProtobufPayload: []byte{0x08, 0x05}}}}
	request := nha022Request(t, ids[0].Entity, epoch, schema, description.Endpoint, args)
	return nha023Rig{host: host, fixture: fixture, entity: ids[0].Entity, epoch: epoch, request: request, driver: driver, gen: &nha022Generation{revision: 7}}
}

func TestNHA024NonIdempotentActionsDoNotReplayAfterCrash(t *testing.T) {
	for _, schema := range []string{"vendor.toggle.v1", "vendor.unlock.v1", "vendor.watering.v1"} {
		t.Run(schema, func(t *testing.T) {
			store, settings := nha024Store(t)
			var effects atomic.Int32
			firstRig := nha024ExtensionRig(t, schema, &nha022Driver{onDispatch: func() error { effects.Add(1); panic("synthetic crash after accepted fake action") }})
			first := nha024Service(t, firstRig, store)
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("valid synthetic action did not reach fake driver")
					}
				}()
				_, _ = first.Dispatch(context.Background(), nha022Actor(t), firstRig.request)
			}()
			if effects.Load() != 1 {
				t.Fatal("first fake action absent")
			}
			if err := first.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			reopened, err := storage.Open(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			secondRig := nha024ExtensionRig(t, schema, &nha022Driver{onDispatch: func() error { effects.Add(1); return nil }})
			second := nha024Service(t, secondRig, reopened)
			defer second.Close(context.Background())
			status, found, err := second.Status(context.Background(), nha024ID(t, firstRig.request.CommandId))
			if err != nil || !found || status.State != storage.IntentIndeterminate {
				t.Fatalf("recovered = %+v %t %v", status, found, err)
			}
			old, err := second.Dispatch(context.Background(), nha022Actor(t), firstRig.request)
			if old == nil || old.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || !errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) || effects.Load() != 1 {
				t.Fatalf("old retry = %v %v effects=%d", old, err, effects.Load())
			}
			fresh := proto.Clone(secondRig.request).(*actions.CommandRequest)
			fresh.CommandId = "new-synthetic-intention"
			result, err := second.Dispatch(context.Background(), nha022Actor(t), fresh)
			if err != nil || result == nil || result.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || effects.Load() != 2 {
				t.Fatalf("fresh valid route = %v %v effects=%d", result, err, effects.Load())
			}
		})
	}
}

func TestNHA024PolicyClaimPreventsCrossLedgerDispatch(t *testing.T) {
	for _, firstPolicy := range []string{"volatile", "durable"} {
		t.Run(firstPolicy, func(t *testing.T) {
			store, _ := nha024Store(t)
			driver := &nha022Driver{}
			rig := nha023SetupBare(t, driver)
			actor := nha022Actor(t)
			var durable *integrations.DurableCommandService
			var volatile *integrations.VolatileCommandService
			var err error
			if firstPolicy == "volatile" {
				volatile, err = integrations.NewVolatileCommandService(rig.host, rig.gen, integrations.DefaultVolatileCommandLimits())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := volatile.Dispatch(context.Background(), actor, rig.request); err != nil {
					t.Fatal(err)
				}
				durable = nha024Service(t, rig, store)
				if got, err := durable.Dispatch(context.Background(), actor, rig.request); got != nil || !errors.Is(err, integrations.ErrCommandPolicyConflict) {
					t.Fatalf("volatile→durable = %v %v", got, err)
				}
			} else {
				durable = nha024Service(t, rig, store)
				if _, err := durable.Dispatch(context.Background(), actor, rig.request); err != nil {
					t.Fatal(err)
				}
				volatile, err = integrations.NewVolatileCommandService(rig.host, rig.gen, integrations.DefaultVolatileCommandLimits())
				if err != nil {
					t.Fatal(err)
				}
				if got, err := volatile.Dispatch(context.Background(), actor, rig.request); got != nil || !errors.Is(err, integrations.ErrCommandPolicyConflict) {
					t.Fatalf("durable→volatile = %v %v", got, err)
				}
			}
			if driver.count() != 1 {
				t.Fatalf("cross-policy send count = %d", driver.count())
			}
		})
	}
}

func TestNHA024DownLedgerCannotDowngradeSameID(t *testing.T) {
	store, _ := nha024Store(t)
	rig := nha023SetupBare(t, &nha022Driver{})
	durable := nha024Service(t, rig, store)
	nha024Exec(t, store, `DROP TABLE public.command_intents`)
	if got, err := durable.Dispatch(context.Background(), nha022Actor(t), rig.request); got != nil || !errors.Is(err, integrations.ErrDurableLedgerUnavailable) {
		t.Fatalf("ledger down = %v %v", got, err)
	}
	volatile, err := integrations.NewVolatileCommandService(rig.host, rig.gen, integrations.DefaultVolatileCommandLimits())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := volatile.Dispatch(context.Background(), nha022Actor(t), rig.request); got != nil || !errors.Is(err, integrations.ErrCommandPolicyConflict) || rig.driver.count() != 0 {
		t.Fatalf("downgrade = %v %v count=%d", got, err, rig.driver.count())
	}
}

func TestNHA024CanceledBeforeDriverPersistsTypedOutcome(t *testing.T) {
	store, settings := nha024Store(t)
	rig := nha023SetupBare(t, &nha022Driver{})
	paused := &nha022PausingGeneration{entered: make(chan struct{}), release: make(chan struct{})}
	service, err := integrations.NewDurableCommandService(context.Background(), rig.host, paused, store)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { _, err := service.Dispatch(ctx, nha022Actor(t), rig.request); finished <- err }()
	select {
	case <-paused.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("final fence not reached")
	}
	cancel()
	close(paused.release)
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("first canceled result = %v", err)
	}
	if rig.driver.count() != 0 {
		t.Fatal("canceled admission entered driver")
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	secondRig := nha023SetupBare(t, &nha022Driver{})
	second := nha024Service(t, secondRig, reopened)
	defer second.Close(context.Background())
	replayed, err := second.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if replayed == nil || replayed.Status != actions.ActionStatus_ACTION_STATUS_FAILED || !errors.Is(err, context.Canceled) || secondRig.driver.count() != 0 {
		t.Fatalf("canceled replay = %v %v sends=%d", replayed, err, secondRig.driver.count())
	}
	status, found, err := second.Status(context.Background(), nha024ID(t, "command-1"))
	if err != nil || !found || status.ErrorKind != storage.IntentCanceled {
		t.Fatalf("typed status = %+v %t %v", status, found, err)
	}
}

func TestNHA024ConcurrentPolicyClaimHasOneSender(t *testing.T) {
	store, _ := nha024Store(t)
	driver := &nha022Driver{}
	rig := nha023SetupBare(t, driver)
	durable := nha024Service(t, rig, store)
	volatile, err := integrations.NewVolatileCommandService(rig.host, rig.gen, integrations.DefaultVolatileCommandLimits())
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	type result struct {
		response *actions.CommandResponse
		err      error
	}
	results := make(chan result, 2)
	go func() {
		<-start
		r, e := durable.Dispatch(context.Background(), nha022Actor(t), rig.request)
		results <- result{r, e}
	}()
	go func() {
		<-start
		r, e := volatile.Dispatch(context.Background(), nha022Actor(t), rig.request)
		results <- result{r, e}
	}()
	close(start)
	a, b := <-results, <-results
	conflicts, dispatched := 0, 0
	for _, got := range []result{a, b} {
		if errors.Is(got.err, integrations.ErrCommandPolicyConflict) {
			conflicts++
		}
		if got.err == nil && got.response != nil && got.response.Status == actions.ActionStatus_ACTION_STATUS_DISPATCHED {
			dispatched++
		}
	}
	if conflicts != 1 || dispatched != 1 || driver.count() != 1 {
		t.Fatalf("claim results = %+v %+v sends=%d", a, b, driver.count())
	}
}
