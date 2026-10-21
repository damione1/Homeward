package integrations_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
)

type nha029GateClock struct{}

func (nha029GateClock) Now() time.Time { return time.Now() }

func nha029GatedService(t *testing.T, driver integrations.CommandDriver, waiters int, drain time.Duration) (*integrations.DurableCommandService, *nha050Rig, *storage.Store) {
	t.Helper()
	store, _ := nha024Store(t)
	rig := nha050Setup(t, driver)
	limits, err := integrations.NewDurableGateLimits(waiters, drain)
	if err != nil {
		t.Fatal(err)
	}
	service, err := integrations.NewDurableCommandServiceWithClockAndGate(context.Background(), rig.host, rig.gen, store, nha029GateClock{}, limits)
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
	return service, rig, store
}

func TestNHA029GateConstructorRejectsTypedNilBeforeAcquiringHostOrSQL(t *testing.T) {
	store, _ := nha024Store(t)
	rig := nha050Setup(t, &nha029InternalDriver{})
	limits, err := integrations.NewDurableGateLimits(2, 6*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var typedNil *nha029NilContext
	var rejected *integrations.DurableCommandService
	var refusal error
	var panicValue any
	func() {
		defer func() { panicValue = recover() }()
		rejected, refusal = integrations.NewDurableCommandServiceWithClockAndGate(typedNil, rig.host, rig.gen, store, nha029GateClock{}, limits)
	}()
	if rejected != nil {
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = rejected.Close(ctx)
		})
	}
	// The same Host and Store must remain available after the rejection. This
	// catches an admission path that touched either ownership guard first.
	service, err := integrations.NewDurableCommandServiceWithClockAndGate(context.Background(), rig.host, rig.gen, store, nha029GateClock{}, limits)
	if service != nil {
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := service.Close(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil || service == nil {
		t.Fatalf("typed-nil attempt reserved Host or SQL: service=%v err=%v", service, err)
	}
	if panicValue != nil || rejected != nil || !errors.Is(refusal, integrations.ErrInvalidInput) {
		t.Fatalf("typed-nil context was not rejected before acquisition: panic=%v service=%v err=%v", panicValue, rejected, refusal)
	}
}

func TestNHA029GateBlocksBeforeOccurrenceCallbackAndResolvesOneCycle(t *testing.T) {
	service, _, _ := nha029GatedService(t, &nha029InternalDriver{}, 1, 6*time.Second)
	port, err := service.InternalAutomationPort()
	if err != nil {
		t.Fatal(err)
	}
	pause, err := service.PauseAndDrain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Quarantine(pause); err != nil {
		t.Fatal(err)
	}
	if err := service.Quarantine(pause); !errors.Is(err, integrations.ErrDurablePauseHandle) {
		t.Fatalf("quarantine was not a single transition: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started, callback, result, done := make(chan struct{}), make(chan struct{}), make(chan error, 1), make(chan struct{})
	go func() {
		defer close(done)
		close(started)
		result <- port.WithOccurrence(ctx, "gate/waited/action", func(context.Context) error { close(callback); return nil })
	}()
	t.Cleanup(func() { <-done })
	<-started
	select {
	case <-callback:
		t.Fatal("new occurrence entered while activation was quarantined")
	case <-time.After(30 * time.Millisecond):
	}
	if err := service.ResumeOld(pause); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("waited occurrence was not resumed")
	}
	select {
	case <-callback:
	default:
		t.Fatal("resumed occurrence never entered callback")
	}
	if err := service.PublishNew(pause); !errors.Is(err, integrations.ErrDurablePauseHandle) {
		t.Fatalf("consumed pause resolved twice: %v", err)
	}
	next, err := service.PauseAndDrain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ResumeOld(pause); !errors.Is(err, integrations.ErrDurablePauseHandle) {
		t.Fatalf("old-cycle pause resumed a new cycle: %v", err)
	}
	if err := service.PublishNew(next); err != nil {
		t.Fatal(err)
	}
	last, err := service.PauseAndDrain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Quarantine(last); err != nil {
		t.Fatal(err)
	}
	if err := service.PublishNew(last); err != nil {
		t.Fatalf("quarantine consumed handle before committed resolution: %v", err)
	}
}

func TestNHA029GateTimeoutRetainsOccurrenceAndRestoresOldAdmissions(t *testing.T) {
	service, _, _ := nha029GatedService(t, &nha029InternalDriver{}, 2, 6*time.Second)
	port, err := service.InternalAutomationPort()
	if err != nil {
		t.Fatal(err)
	}
	entered, release, result, done := make(chan struct{}), make(chan struct{}), make(chan error, 1), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() { unblock(); <-done })
	go func() {
		defer close(done)
		result <- port.WithOccurrence(context.Background(), "gate/active/action", func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	deadline, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if handle, err := service.PauseAndDrain(deadline); handle != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain timeout fabricated a pause handle: %v %v", handle, err)
	}
	if err := port.WithOccurrence(context.Background(), "gate/after-timeout", func(context.Context) error { return nil }); err != nil {
		t.Fatalf("pre-commit timeout did not restore old admissions: %v", err)
	}
	unblock()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic oracle did not execute callback")
			}
		}()
		_ = port.WithOccurrence(context.Background(), "gate/panic", func(context.Context) error { panic("synthetic callback panic") })
	}()
	pause, err := service.PauseAndDrain(context.Background())
	if err != nil {
		t.Fatalf("panic orphaned occurrence lease: %v", err)
	}
	if err := service.ResumeOld(pause); err != nil {
		t.Fatal(err)
	}
}

func TestNHA029GateCloseWaitsForAnAdmittedOccurrence(t *testing.T) {
	service, _, _ := nha029GatedService(t, &nha029InternalDriver{}, 1, 6*time.Second)
	port, err := service.InternalAutomationPort()
	if err != nil {
		t.Fatal(err)
	}
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	go func() {
		defer close(finished)
		_ = port.WithOccurrence(context.Background(), "gate/close-active", func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()
	t.Cleanup(func() { unblock(); <-finished })
	<-entered
	short, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := service.Close(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close failed to retain active occurrence: %v", err)
	}
	select {
	case <-service.Done():
		t.Fatal("Done closed before occurrence callback returned")
	default:
	}
	unblock()
	<-finished
	long, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := service.Close(long); err != nil {
		t.Fatal(err)
	}
}

type nha029GateBlockingDriver struct {
	store   *storage.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	relOnce sync.Once
}

func (d *nha029GateBlockingDriver) unblock() { d.relOnce.Do(func() { close(d.release) }) }

func (d *nha029GateBlockingDriver) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	if !command.ReadyForTransport() {
		return integrations.ErrCommandAuthorizationDenied
	}
	id, _, err := command.RouteIdentity()
	if err != nil {
		return err
	}
	record, present, err := d.store.CommandIntent(ctx, id)
	if err != nil || !present || record.State() != storage.IntentDispatching || record.OriginKind() != storage.IntentOriginInternalAutomation {
		return errors.New("gate driver entered before durable dispatching intent")
	}
	d.once.Do(func() { close(d.entered) })
	<-d.release
	return nil
}

func TestNHA029GateDrainsOnlyAfterDurableResultAndKeepsOneLedger(t *testing.T) {
	driver := &nha029GateBlockingDriver{entered: make(chan struct{}), release: make(chan struct{})}
	service, rig, store := nha029GatedService(t, driver, 2, 6*time.Second)
	t.Cleanup(driver.unblock)
	driver.store = store
	port, err := service.InternalAutomationPort()
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := rig.fixture.Sequencer().CurrentAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	request := nha022Request(t, rig.identities[0].Entity, epoch, contracts.SwitchSchema, "relay", nha022Switch())
	request.CommandId = "nha029-gate-durable-result"
	id, err := domain.ParseCommandID(request.CommandId)
	if err != nil {
		t.Fatal(err)
	}
	dispatched, dispatchDone := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(dispatchDone)
		dispatched <- port.WithOccurrence(context.Background(), "gate/durable/one", func(ctx context.Context) error {
			response, err := port.Dispatch(ctx, request)
			if err != nil {
				return err
			}
			if response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
				return errors.New("fake effect did not complete through 024")
			}
			return nil
		})
	}()
	t.Cleanup(func() { driver.unblock(); <-dispatchDone })
	select {
	case <-driver.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("durable driver was not entered")
	}
	paused, pauseDone := make(chan *integrations.ActivationPause, 1), make(chan struct{})
	failed := make(chan error, 1)
	go func() {
		defer close(pauseDone)
		handle, err := service.PauseAndDrain(context.Background())
		if err != nil {
			failed <- err
			return
		}
		paused <- handle
	}()
	t.Cleanup(func() { driver.unblock(); <-pauseDone })
	select {
	case <-paused:
		t.Fatal("pause returned before driver and ledger result")
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(30 * time.Millisecond):
	}
	record, present, err := store.CommandIntent(context.Background(), id)
	if err != nil || !present || record.State() != storage.IntentDispatching {
		t.Fatalf("driver did not hold a durable in-flight intent: present=%t state=%v err=%v", present, record.State(), err)
	}
	driver.unblock()
	if err := <-dispatched; err != nil {
		t.Fatal(err)
	}
	select {
	case handle := <-paused:
		record, present, err := store.CommandIntent(context.Background(), id)
		if err != nil || !present || record.State() != storage.IntentResult {
			t.Fatalf("pause preceded terminal ledger persistence: present=%t state=%v err=%v", present, record.State(), err)
		}
		if err := service.PublishNew(handle); err != nil {
			t.Fatal(err)
		}
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("pause did not finish after the durable result")
	}
}

// A direct human command has no automation occurrence lease to hide an early
// release of the DCS active lease. The PostgreSQL trigger holds the actual
// terminal UPDATE after the fake driver returns, so the short drain deadline
// must expire synchronously while that write is still in flight.
func TestNHA029GateDrainIncludesBlockedFinalSQLAndRefusedFinalization(t *testing.T) {
	for _, test := range []struct {
		name   string
		refuse bool
	}{
		{name: "committed-result"},
		{name: "refused-result", refuse: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			driver := &nha022Driver{}
			service, rig, store := nha029GatedService(t, driver, 2, 6*time.Second)
			epoch, err := rig.fixture.Sequencer().CurrentAdmissionEpoch()
			if err != nil {
				t.Fatal(err)
			}
			request := nha022Request(t, rig.identities[0].Entity, epoch, contracts.SwitchSchema, "relay", nha022Switch())
			request.CommandId = "nha029-gate-final-sql-" + test.name
			id, err := domain.ParseCommandID(request.CommandId)
			if err != nil {
				t.Fatal(err)
			}
			function := `CREATE FUNCTION public.nha029_hold_finalization() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
				IF OLD.state='dispatching' AND NEW.state IN ('result','indeterminate') THEN
					PERFORM pg_advisory_xact_lock(9026, 29);`
			if test.refuse {
				function += ` RAISE EXCEPTION 'NHA029 refused terminal command update';`
			}
			function += ` END IF; RETURN NEW; END $$;
				CREATE TRIGGER nha029_hold_finalization BEFORE UPDATE ON public.command_intents
				FOR EACH ROW EXECUTE FUNCTION public.nha029_hold_finalization()`
			nha024Exec(t, store, function)
			block := nha026HoldAdvisory(t, store, 29)
			dispatchCtx, cancelDispatch := context.WithTimeout(rig.ctx, 10*time.Second)
			type dispatchResult struct {
				response *actions.CommandResponse
				err      error
			}
			result, dispatchDone := make(chan dispatchResult, 1), make(chan struct{})
			var pending *integrations.ActivationPause
			resolved := false
			t.Cleanup(func() {
				block.Release()
				cancelDispatch()
				select {
				case <-dispatchDone:
				case <-time.After(8 * time.Second):
					t.Error("dispatch did not join after releasing final SQL lock")
				}
				if pending != nil && !resolved {
					if err := service.ResumeOld(pending); err != nil {
						t.Errorf("unresolved activation pause during cleanup: %v", err)
					}
				}
			})
			go func() {
				defer close(dispatchDone)
				response, err := service.Dispatch(dispatchCtx, rig.actor, request)
				result <- dispatchResult{response: response, err: err}
			}()
			nha026WaitForCommandUpdateBlocked(t, store)
			if effects := driver.count(); effects != 1 {
				t.Fatalf("fake driver did not return exactly once before final SQL: %d", effects)
			}
			shortCtx, stopShort := context.WithTimeout(context.Background(), 40*time.Millisecond)
			shortPause, pauseErr := service.PauseAndDrain(shortCtx)
			stopShort()
			pending = shortPause
			if shortPause != nil || !errors.Is(pauseErr, context.DeadlineExceeded) {
				t.Fatalf("drain returned while terminal UPDATE was blocked: handle=%v err=%v", shortPause, pauseErr)
			}
			record, present, err := store.CommandIntent(context.Background(), id)
			if err != nil || !present || record.State() != storage.IntentDispatching {
				t.Fatalf("blocked final SQL was not still dispatching: present=%t state=%v err=%v", present, record.State(), err)
			}
			block.Release()
			if err := nha026Await(t, block.done); err != nil {
				t.Fatal(err)
			}
			got := nha026Await(t, result)
			select {
			case <-dispatchDone:
			case <-time.After(3 * time.Second):
				t.Fatal("dispatch did not join after final SQL outcome")
			}
			if test.refuse {
				if !errors.Is(got.err, integrations.ErrCommandOutcomeIndeterminate) || got.response == nil ||
					got.response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
					t.Fatalf("refused final SQL lost indeterminate outcome: response=%v err=%v", got.response, got.err)
				}
			} else if got.err != nil || got.response == nil || got.response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
				t.Fatalf("unblocked final SQL did not persist result: response=%v err=%v", got.response, got.err)
			}
			longCtx, stopLong := context.WithTimeout(context.Background(), 5*time.Second)
			pending, err = service.PauseAndDrain(longCtx)
			stopLong()
			if err != nil || pending == nil {
				t.Fatalf("drain did not complete after final SQL attempt: handle=%v err=%v", pending, err)
			}
			record, present, err = store.CommandIntent(context.Background(), id)
			wantState := storage.IntentResult
			if test.refuse {
				wantState = storage.IntentDispatching
			}
			if err != nil || !present || record.State() != wantState || driver.count() != 1 {
				t.Fatalf("drained ledger or effect changed: present=%t state=%v effects=%d err=%v", present, record.State(), driver.count(), err)
			}
			if err := service.ResumeOld(pending); err != nil {
				t.Fatal(err)
			}
			resolved = true
			if test.refuse {
				replayCtx, stopReplay := context.WithTimeout(rig.ctx, 5*time.Second)
				defer stopReplay()
				replay, err := service.Dispatch(replayCtx, rig.actor, request)
				if !errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) || replay == nil ||
					replay.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || driver.count() != 1 {
					t.Fatalf("uncertain intent was resent: response=%v effects=%d err=%v", replay, driver.count(), err)
				}
			}
		})
	}
}

func TestNHA029GateHumanDirectWaitsBeforeCommandIDClaim(t *testing.T) {
	driver := &nha022Driver{}
	service, rig, store := nha029GatedService(t, driver, 2, 6*time.Second)
	epoch, err := rig.fixture.Sequencer().CurrentAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	request := nha022Request(t, rig.identities[0].Entity, epoch, contracts.SwitchSchema, "relay", nha022Switch())
	request.CommandId = "nha029-gate-human-direct"
	id, err := domain.ParseCommandID(request.CommandId)
	if err != nil {
		t.Fatal(err)
	}
	pause, err := service.PauseAndDrain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(rig.ctx, 5*time.Second)
	defer cancel()
	type answer struct {
		response *actions.CommandResponse
		err      error
	}
	started, done, result := make(chan struct{}), make(chan struct{}), make(chan answer, 1)
	go func() {
		defer close(done)
		close(started)
		response, err := service.Dispatch(ctx, rig.actor, request)
		result <- answer{response, err}
	}()
	t.Cleanup(func() { cancel(); <-done })
	<-started
	select {
	case got := <-result:
		t.Fatalf("direct request crossed paused admission: response=%v err=%v", got.response, got.err)
	case <-time.After(30 * time.Millisecond):
	}
	if _, present, err := store.CommandIntent(context.Background(), id); err != nil || present || driver.count() != 0 {
		t.Fatalf("direct request claimed ID or effect before resume: present=%t effects=%d err=%v", present, driver.count(), err)
	}
	if err := service.ResumeOld(pause); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil || got.response == nil || got.response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || driver.count() != 1 {
			t.Fatalf("same direct request failed after resume: response=%v effects=%d err=%v", got.response, driver.count(), got.err)
		}
	case <-ctx.Done():
		t.Fatal("direct request did not resume")
	}
}

func TestNHA029GateRejectsForeignZeroCanceledAndJoinsBoundedWaiters(t *testing.T) {
	if _, err := integrations.NewDurableGateLimits(0, 6*time.Second); !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("zero waiter capacity admitted: %v", err)
	}
	if _, err := integrations.NewDurableGateLimits(1, time.Minute+time.Second); !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("unbounded drain admitted: %v", err)
	}
	service, _, _ := nha029GatedService(t, &nha029InternalDriver{}, 1, 6*time.Second)
	port, err := service.InternalAutomationPort()
	if err != nil {
		t.Fatal(err)
	}
	foreignService, _, _ := nha029GatedService(t, &nha029InternalDriver{}, 1, 6*time.Second)
	foreign, err := foreignService.PauseAndDrain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ResumeOld(foreign); !errors.Is(err, integrations.ErrDurablePauseHandle) {
		t.Fatalf("foreign service handle accepted: %v", err)
	}
	if err := foreignService.ResumeOld(foreign); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if handle, err := service.PauseAndDrain(canceled); handle != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled pause changed state: %v %v", handle, err)
	}
	var typedNil *nha029NilContext
	if handle, err := service.PauseAndDrain(typedNil); handle != nil || !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("typed-nil context admitted: %v %v", handle, err)
	}
	var zero integrations.DurableCommandService
	if handle, err := zero.PauseAndDrain(context.Background()); handle != nil || !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("zero service admitted pause: %v %v", handle, err)
	}
	if err := service.PublishNew(nil); !errors.Is(err, integrations.ErrDurablePauseHandle) {
		t.Fatalf("nil handle admitted: %v", err)
	}
	if err := service.ResumeOld(&integrations.ActivationPause{}); !errors.Is(err, integrations.ErrDurablePauseHandle) {
		t.Fatalf("zero handle admitted: %v", err)
	}
	pause, err := service.PauseAndDrain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second, err := service.PauseAndDrain(context.Background()); second != nil || !errors.Is(err, integrations.ErrDurablePauseConflict) {
		t.Fatalf("second pause crossed exclusive reservation: %v %v", second, err)
	}
	firstCtx, firstCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer firstCancel()
	results := make(chan error, 2)
	var waiters sync.WaitGroup
	callback := make(chan struct{})
	for _, cause := range []string{"gate/first-waiter", "gate/second-waiter"} {
		waiters.Add(1)
		go func(cause string) {
			defer waiters.Done()
			results <- port.WithOccurrence(firstCtx, cause, func(context.Context) error { close(callback); return nil })
		}(cause)
	}
	t.Cleanup(func() { firstCancel(); waiters.Wait() })
	select {
	case err := <-results:
		if !errors.Is(err, integrations.ErrDurableAdmissionCapacity) {
			t.Fatalf("bounded second waiter returned another result: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bounded second waiter did not refuse admission")
	}
	select {
	case <-callback:
		t.Fatal("paused callback entered before Close")
	default:
	}
	closeCtx, stopClose := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopClose()
	if err := service.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	if err := <-results; !errors.Is(err, integrations.ErrDurableLedgerUnavailable) {
		t.Fatalf("Close did not wake bounded waiter: %v", err)
	}
	select {
	case <-service.Done():
	default:
		t.Fatal("Done preceded waiter drain")
	}
	if err := service.ResumeOld(pause); !errors.Is(err, integrations.ErrDurablePauseHandle) {
		t.Fatalf("closed service accepted stale handle: %v", err)
	}
	if err := port.WithOccurrence(context.Background(), "gate/after-close", func(context.Context) error { return nil }); !errors.Is(err, integrations.ErrDurableLedgerUnavailable) {
		t.Fatalf("Close reopened occurrence admission: %v", err)
	}
}
