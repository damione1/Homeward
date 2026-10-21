package integrations_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/observability"
	"github.com/damione1/Homeward/internal/storage"
)

func nha056DurableHealth(t *testing.T, host *integrations.Manager) *observability.HealthCell {
	t.Helper()
	cells, err := host.ObservationCells()
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := cells.Epoch()
	if err != nil {
		t.Fatal(err)
	}
	health, err := observability.NewHealthCell(epoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.AttachObservationHealth(health); err != nil {
		t.Fatal(err)
	}
	// This is the old successful Recorder fact that DCS must supersede with
	// its own real transaction outcome, rather than leaving AVAILABLE stale.
	if err := health.PublishDurableFact(obs.DurableAvailability_DURABLE_AVAILABILITY_AVAILABLE, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	return health
}

func nha056AssertDurableFact(t *testing.T, health *observability.HealthCell, expected obs.DurableAvailability) {
	t.Helper()
	facts, err := health.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	durable, err := facts.Durable()
	if err != nil || durable.Availability != expected || durable.GetObservedAt() == nil || !durable.GetObservedAt().AsTime().After(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("durable health did not publish the real DCS transaction outcome", err)
	}
}

func TestNHA056DurableRecoveryCountsOnlyConfirmedCommitAndNeverReplays(t *testing.T) {
	store, _ := nha024Store(t)
	var entered atomic.Int32
	driver := &nha022Driver{onDispatch: func() error { entered.Add(1); panic("isolated crash after fake driver entry") }}
	firstRig := nha023SetupBare(t, driver)
	first := nha024Service(t, firstRig, store)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("fake driver was not entered")
			}
		}()
		_, _ = first.Dispatch(context.Background(), nha022Actor(t), firstRig.request)
	}()
	intent, found, err := first.Status(context.Background(), nha024ID(t, firstRig.request.CommandId))
	if err != nil || !found || intent.State != storage.IntentDispatching || entered.Load() != 1 {
		t.Fatal("recovery precondition absent", err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A deferred trigger fails the real recovery COMMIT, after its UPDATE ran.
	// No acknowledged row count may escape that completion-unknown outcome.
	nha024Exec(t, store, `CREATE FUNCTION public.nha056_refuse_recovery() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic deferred recovery failure'; END $$;
		CREATE CONSTRAINT TRIGGER nha056_refuse_recovery AFTER UPDATE ON public.command_intents DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha056_refuse_recovery()`)
	secondDriver := &nha022Driver{}
	secondRig := nha023SetupBare(t, secondDriver)
	health := nha056DurableHealth(t, secondRig.host)
	if _, err := integrations.NewDurableCommandService(context.Background(), secondRig.host, secondRig.gen, store); !errors.Is(err, integrations.ErrDurableCommitUnknown) {
		t.Fatal("deferred recovery commit was not uncertain", err)
	}
	nha056AssertDurableFact(t, health, obs.DurableAvailability_DURABLE_AVAILABILITY_DEGRADED)
	metrics, err := secondRig.host.ObservationActions()
	if err != nil || metrics.DurableRecoveredIndeterminateTotal.GetExact() != 0 {
		t.Fatal("unconfirmed recovery counted", err)
	}
	nha024Exec(t, store, `DROP TRIGGER nha056_refuse_recovery ON public.command_intents; DROP FUNCTION public.nha056_refuse_recovery()`)
	second := nha024Service(t, secondRig, store)
	nha056AssertDurableFact(t, health, obs.DurableAvailability_DURABLE_AVAILABILITY_AVAILABLE)
	metrics, err = secondRig.host.ObservationActions()
	if err != nil || metrics.DurableRecoveredIndeterminateTotal.GetExact() != 1 {
		t.Fatal("confirmed recovery not counted exactly once", err)
	}
	for range 3 {
		response, err := second.Dispatch(context.Background(), nha022Actor(t), firstRig.request)
		if !errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || secondDriver.count() != 0 {
			t.Fatal("recovery replayed or lost uncertainty", err)
		}
		// The recovery COMMIT was confirmed even though the physical outcome
		// remains indeterminate; replay cannot add a new storage health fact.
		nha056AssertDurableFact(t, health, obs.DurableAvailability_DURABLE_AVAILABILITY_AVAILABLE)
	}
	if err := second.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	metrics, err = secondRig.host.ObservationActions()
	if err != nil || metrics.DurableRecoveredIndeterminateTotal.GetExact() != 1 {
		t.Fatal("repeated reads or replays double counted recovery", err)
	}
}

func TestNHA056DurableFinalPersistenceFailureCountsUncertaintyWithoutReplay(t *testing.T) {
	store, _ := nha024Store(t)
	driver := &nha022Driver{}
	rig := nha023SetupBare(t, driver)
	health := nha056DurableHealth(t, rig.host)
	service := nha024Service(t, rig, store)
	nha056AssertDurableFact(t, health, obs.DurableAvailability_DURABLE_AVAILABILITY_AVAILABLE)
	// Accepted and dispatching commits succeed. Only the final result COMMIT
	// fails, after the real isolated fake driver has been entered once.
	nha024Exec(t, store, `CREATE FUNCTION public.nha056_refuse_final() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.state IN ('result', 'indeterminate') THEN RAISE EXCEPTION 'synthetic deferred final result failure'; END IF;
		RETURN NEW; END $$;
		CREATE CONSTRAINT TRIGGER nha056_refuse_final AFTER UPDATE ON public.command_intents DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha056_refuse_final()`)
	for range 4 {
		response, err := service.Dispatch(context.Background(), nha022Actor(t), rig.request)
		if !errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || driver.count() != 1 {
			t.Fatal("final persistence failure became certain or replayed the fake effect", err)
		}
		nha056AssertDurableFact(t, health, obs.DurableAvailability_DURABLE_AVAILABILITY_DEGRADED)
		intent, found, err := service.Status(context.Background(), nha024ID(t, rig.request.CommandId))
		if err != nil || !found || intent.State != storage.IntentDispatching {
			t.Fatal("unconfirmed final result was presented as committed", err)
		}
		// A successful status read, a runtime Ping or an uncertain replay is
		// not an acknowledged durable write and cannot clear this failure.
		nha056AssertDurableFact(t, health, obs.DurableAvailability_DURABLE_AVAILABILITY_DEGRADED)
		metrics, err := rig.host.ObservationActions()
		if err != nil || metrics.DurableRecoveredIndeterminateTotal.GetExact() != 0 {
			t.Fatal("final failure invented a recovery commit", err)
		}
		foundDurable := false
		for _, ledger := range metrics.FirstAttempts {
			if ledger.Ledger == obs.AttemptLedger_ATTEMPT_LEDGER_DURABLE {
				foundDurable = true
				if ledger.IndeterminateTotal.GetExact() != 1 {
					t.Fatal("final persistence failure was lost or recounted on replay")
				}
			}
		}
		if !foundDurable {
			t.Fatal("durable first-attempt metric absent")
		}
	}
}
