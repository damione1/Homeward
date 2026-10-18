package integrations_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/light"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

func nha179Durable(t *testing.T, rig nha179Rig, store *storage.Store) *integrations.DurableCommandService {
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

func nha179DurableSlot(t *testing.T, service *integrations.DurableCommandService, id string) *light.PreparationSlot {
	t.Helper()
	return nha179Slot(t, service, nha179Call(t, "light.turn_on", "{brightness_step_pct: 15}"), id)
}

func TestNHA179DurablePreparedResultReplayConflictAndRestartStatus(t *testing.T) {
	store, settings := nha024Store(t)
	rig := nha179Setup(t, false)
	observed := 30.0
	rig.publish(t, true, &observed, domain.FreshnessFresh, domain.ProvenanceObserved)
	service := nha179Durable(t, rig, store)
	slot := nha179DurableSlot(t, service, "nha179-durable-result")
	prepared, err := slot.PrepareOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wire, err := prepared.Request()
	if err != nil {
		t.Fatal(err)
	}
	first, err := slot.Dispatch(context.Background())
	if err != nil || first == nil || first.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || rig.driver.Count() != 1 {
		t.Fatalf("first prepared durable dispatch: %v %v sends=%d", first, err, rig.driver.Count())
	}
	observed = 60
	rig.publish(t, true, &observed, domain.FreshnessFresh, domain.ProvenanceObserved)
	rig.discoverMQTT(t, 255)
	replay, err := slot.Dispatch(context.Background())
	if err != nil || !proto.Equal(first, replay) || rig.driver.Count() != 1 {
		t.Fatalf("durable retry recomputed changed state/profile: %v %v sends=%d", replay, err, rig.driver.Count())
	}
	changed := proto.Clone(wire).(*actions.CommandRequest)
	changed.GetArguments().GetSetLight().On = nil
	if response, err := service.Dispatch(context.Background(), nha022Actor(t), changed); response != nil || !errors.Is(err, integrations.ErrCommandIDConflict) {
		t.Fatalf("same-ID wire conflict: %v %v", response, err)
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Close)
	secondRig := nha179Setup(t, false)
	second := nha179Durable(t, secondRig, reopened)
	id, _ := domain.ParseCommandID("nha179-durable-result")
	status, found, err := second.Status(context.Background(), id)
	if err != nil || !found || status.State != storage.IntentResult || !proto.Equal(status.Response, first) || secondRig.driver.Count() != 0 {
		t.Fatalf("lost slot was reconstructed after restart: %+v found=%t err=%v sends=%d", status, found, err, secondRig.driver.Count())
	}
}

func TestNHA179DurablePreparedCommitFailureHasNoSendOrRecord(t *testing.T) {
	store, _ := nha024Store(t)
	rig := nha179Setup(t, false)
	observed := 30.0
	rig.publish(t, true, &observed, domain.FreshnessFresh, domain.ProvenanceObserved)
	service := nha179Durable(t, rig, store)
	slot := nha179DurableSlot(t, service, "nha179-durable-refused-commit")
	if _, err := slot.PrepareOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	nha024Exec(t, store, `CREATE FUNCTION public.nha179_refuse_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'deferred failure'; END $$;
		CREATE CONSTRAINT TRIGGER nha179_refuse_commit AFTER INSERT ON public.command_intents DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha179_refuse_commit()`)
	response, err := slot.Dispatch(context.Background())
	if response != nil || !errors.Is(err, integrations.ErrDurableCommitUnknown) || rig.driver.Count() != 0 {
		t.Fatalf("prepared crossed failed commit: %v %v sends=%d", response, err, rig.driver.Count())
	}
	id, _ := domain.ParseCommandID("nha179-durable-refused-commit")
	_, found, err := service.Status(context.Background(), id)
	if err != nil || found {
		t.Fatalf("rolled-back prepared intent persisted: found=%t err=%v", found, err)
	}
}

func TestNHA179DurablePreparedAcceptedThenProfileChangeRefusesBeforeSend(t *testing.T) {
	store, _ := nha024Store(t)
	rig := nha179Setup(t, false)
	observed := 30.0
	rig.publish(t, true, &observed, domain.FreshnessFresh, domain.ProvenanceObserved)
	service := nha179Durable(t, rig, store)
	slot := nha179DurableSlot(t, service, "nha179-durable-profile-fence")
	if _, err := slot.PrepareOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	nha024Exec(t, store, `CREATE FUNCTION public.nha179_pause_dispatching() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.state = 'dispatching' THEN PERFORM pg_advisory_xact_lock(9179, 1); END IF; RETURN NEW; END $$;
		CREATE TRIGGER nha179_pause_dispatching BEFORE UPDATE ON public.command_intents FOR EACH ROW EXECUTE FUNCTION public.nha179_pause_dispatching()`)
	locked := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	blocker := make(chan error, 1)
	go func() {
		blocker <- store.InTransaction(context.Background(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(context.Background(), `SELECT pg_advisory_xact_lock(9179, 1)`); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	select {
	case <-locked:
	case err := <-blocker:
		t.Fatalf("prepared dispatch barrier failed before lock: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("prepared dispatch barrier never locked")
	}
	type result struct {
		response *actions.CommandResponse
		err      error
	}
	done := make(chan result, 1)
	go func() { response, err := slot.Dispatch(context.Background()); done <- result{response, err} }()
	id, _ := domain.ParseCommandID("nha179-durable-profile-fence")
	deadline := time.After(5 * time.Second)
	for {
		status, found, _ := service.Status(context.Background(), id)
		if found && status.State == storage.IntentAccepted {
			break
		}
		select {
		case <-deadline:
			t.Fatal("prepared intent never committed accepted")
		case <-time.After(10 * time.Millisecond):
		}
	}
	rig.discoverMQTT(t, 255)
	unblock()
	if err := <-blocker; err != nil {
		t.Fatal(err)
	}
	outcome := <-done
	if outcome.err != nil || outcome.response == nil || outcome.response.Status != actions.ActionStatus_ACTION_STATUS_FAILED || rig.driver.Count() != 0 {
		t.Fatalf("profile changed before final fence: %v %v sends=%d", outcome.response, outcome.err, rig.driver.Count())
	}
	status, found, err := service.Status(context.Background(), id)
	if err != nil || !found || status.State != storage.IntentResult || status.Response.Status != actions.ActionStatus_ACTION_STATUS_FAILED {
		t.Fatalf("prepared refusal not durably recorded: %+v found=%t err=%v", status, found, err)
	}
	replay, err := slot.Dispatch(context.Background())
	if err != nil || !proto.Equal(outcome.response, replay) || rig.driver.Count() != 0 {
		t.Fatalf("stale prepared replay emitted: %v %v sends=%d", replay, err, rig.driver.Count())
	}
}

func TestNHA179DurablePreparedDispatchingCrashRecoversWithoutSlotOrSend(t *testing.T) {
	store, settings := nha024Store(t)
	rig := nha179Setup(t, false)
	observed := 30.0
	rig.publish(t, true, &observed, domain.FreshnessFresh, domain.ProvenanceObserved)
	service := nha179Durable(t, rig, store)
	slot := nha179DurableSlot(t, service, "nha179-durable-crash")
	if _, err := slot.PrepareOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var entered atomic.Int32
	rig.driver.call = func(context.Context, integrations.AdmittedCommand) error {
		entered.Add(1)
		panic("synthetic process loss after prepared driver entry")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected synthetic crash")
			}
		}()
		_, _ = slot.Dispatch(context.Background())
	}()
	if entered.Load() != 1 {
		t.Fatal("prepared route never reached fake driver")
	}
	id, _ := domain.ParseCommandID("nha179-durable-crash")
	before, found, err := service.Status(context.Background(), id)
	if err != nil || !found || before.State != storage.IntentDispatching {
		t.Fatalf("pre-restart dispatching intent: %+v found=%t err=%v", before, found, err)
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Close)
	secondRig := nha179Setup(t, false)
	second := nha179Durable(t, secondRig, reopened)
	after, found, err := second.Status(context.Background(), id)
	if err != nil || !found || after.State != storage.IntentIndeterminate || after.Response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || secondRig.driver.Count() != 0 || entered.Load() != 1 {
		t.Fatalf("reboot reconstructed lost prepared slot: %+v found=%t err=%v sends=%d first=%d", after, found, err, secondRig.driver.Count(), entered.Load())
	}
}
