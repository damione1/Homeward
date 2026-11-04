package integrations_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

func TestNHA201DurableAdmissionHandsOffOnceAfterCommit(t *testing.T) {
	store, _ := nha024Store(t)
	driver := &nha022Driver{entered: make(chan struct{}), release: make(chan struct{})}
	fixture := nha050Setup(t, driver)
	service, err := integrations.NewDurableCommandService(fixture.ctx, fixture.host, fixture.gen, store)
	if err != nil {
		t.Fatal(err)
	}
	continuation, cancelContinuation := context.WithCancel(fixture.ctx)
	defer func() {
		cancelContinuation()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = service.Close(ctx)
	}()
	request, err := fixture.prepared(t, service, "a123456789abcdef0123456789abcdef").Request()
	if err != nil {
		t.Fatal(err)
	}
	capability, _ := apitypes.ParseCapabilityName("homes/synthetic/entities/relay/capabilities/power")
	key, _ := apitypes.ParseRequestID("12345678-1234-4123-8123-123456789abc")
	metadata, err := storage.NewCommandAPIMetadata(capability, &key, sha256.Sum256([]byte("synthetic explicit power intention")))
	if err != nil {
		t.Fatal(err)
	}
	http, cancelHTTP := context.WithCancel(fixture.ctx)
	complete := make(chan struct{})
	response, err := service.Admit(http, continuation, fixture.actor, request, metadata, func() { close(complete) })
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_ACCEPTED {
		t.Fatal("not admitted", response, err)
	}
	cancelHTTP()
	select {
	case <-driver.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("retained occurrence did not dispatch")
	}
	id, _ := domain.ParseCommandID(request.CommandId)
	record, found, err := store.CommandIntent(fixture.ctx, id)
	if err != nil || !found {
		t.Fatal("handle preceded durable reservation", err)
	}
	if got, present := record.APIMetadata(); !present || got != metadata {
		t.Fatal("API provenance not atomic with intent")
	}
	duplicateDone := make(chan struct{})
	if _, err := service.Admit(fixture.ctx, continuation, fixture.actor, request, metadata, func() { close(duplicateDone) }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-duplicateDone:
	case <-time.After(5 * time.Second):
		t.Fatal("duplicate started another occurrence")
	}
	conflict, _ := storage.NewCommandAPIMetadata(capability, &key, sha256.Sum256([]byte("different intention")))
	if _, err := service.RecordedAPICommand(fixture.ctx, id, conflict); !errors.Is(err, integrations.ErrCommandIDConflict) {
		t.Fatal("changed intent accepted", err)
	}
	close(driver.release)
	// End progress tracking without ever cancelling the completed HTTP caller's
	// retained dispatch. Its outcome remains explicit, never a replay invitation.
	deadline := time.Now().Add(5 * time.Second)
	for driver.count() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancelContinuation()
	select {
	case <-complete:
	case <-time.After(5 * time.Second):
		t.Fatal("occurrence failed to release credential")
	}
	if driver.count() != 1 {
		t.Fatal("duplicate physical attempt", driver.count())
	}
	record, found, err = store.CommandIntent(fixture.ctx, id)
	if err != nil || !found || len(record.APITerminalResponse()) == 0 {
		t.Fatal("terminal native proof not persisted", err)
	}
	if _, err := service.Admit(fixture.ctx, fixture.ctx, fixture.actor, request, metadata, func() {}); err != nil {
		t.Fatal("read-only reconciliation failed", err)
	}
	if driver.count() != 1 {
		t.Fatal("reconciliation redispatched")
	}
	home := capability.Entity().Home()
	names, next, err := service.APICommandNames(fixture.ctx, fixture.actor, home, nil, 1, time.Now())
	if err != nil || len(names) != 1 || next != nil {
		t.Fatal("authorized operation listing", names, next, err)
	}
	foundNames, err := service.APICommandsByRequest(fixture.ctx, fixture.actor, home, key, time.Now())
	if err != nil || len(foundNames) != 1 || foundNames[0] != names[0] {
		t.Fatal("UUID reconciliation", foundNames, err)
	}
	fixture.policy(t, nil, nil, 2)
	names, _, err = service.APICommandNames(fixture.ctx, fixture.actor, home, nil, 1, time.Now())
	if err != nil || len(names) != 0 {
		t.Fatal("revoked operation leaked through list", names, err)
	}
	foundNames, err = service.APICommandsByRequest(fixture.ctx, fixture.actor, home, key, time.Now())
	if err != nil || len(foundNames) != 0 {
		t.Fatal("revoked operation leaked through request lookup", foundNames, err)
	}
}

func TestNHA201LateCausalConfirmationIsDurableAcrossRestart(t *testing.T) {
	t.Run("persisted-ack", func(t *testing.T) { nha201LateConfirmation(t, false) })
	t.Run("failed-ack-write", func(t *testing.T) { nha201LateConfirmation(t, true) })
}
func nha201LateConfirmation(t *testing.T, failACK bool) {
	store, _ := nha024Store(t)
	if failACK {
		// A PostgreSQL sequence survives rollback, recording the attempted ACK
		// without adding a production fault seam or a second command owner.
		outcome, err := store.InTransactionOutcome(context.Background(), func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `CREATE SEQUENCE public.nha201_ack_attempt;
CREATE FUNCTION public.nha201_reject_ack() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
IF NEW.api_acknowledged_response IS NOT NULL AND OLD.api_acknowledged_response IS NULL THEN
 PERFORM nextval('public.nha201_ack_attempt'); RAISE EXCEPTION 'injected ACK append failure';
END IF; RETURN NEW; END $$;
CREATE TRIGGER nha201_reject_ack BEFORE UPDATE ON public.command_intents FOR EACH ROW EXECUTE FUNCTION public.nha201_reject_ack();`)
			return err
		})
		if err != nil || outcome != storage.TransactionCommitted {
			t.Fatal("ACK fault fixture", outcome, err)
		}
	}
	commands := make(chan integrations.AdmittedCommand, 1)
	driver := &nha025Driver{call: func(_ context.Context, command integrations.AdmittedCommand) error {
		if err := command.Acknowledge(); err != nil {
			return err
		}
		commands <- command
		return nil
	}}
	fixture := nha050Setup(t, driver)
	service, err := integrations.NewDurableCommandService(fixture.ctx, fixture.host, fixture.gen, store)
	if err != nil {
		t.Fatal(err)
	}
	continuation, cancelContinuation := context.WithCancel(fixture.ctx)
	closeService := func(service *integrations.DurableCommandService) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := service.Close(ctx); err != nil {
			t.Error(err)
		}
	}
	defer func() { cancelContinuation(); closeService(service) }()
	request, err := fixture.prepared(t, service, "b123456789abcdef0123456789abcdef").Request()
	if err != nil {
		t.Fatal(err)
	}
	capability, _ := apitypes.ParseCapabilityName("homes/synthetic/entities/relay/capabilities/power")
	key, _ := apitypes.ParseRequestID("22345678-1234-4123-8123-123456789abc")
	metadata, err := storage.NewCommandAPIMetadata(capability, &key, sha256.Sum256([]byte("late correlated power intention")))
	if err != nil {
		t.Fatal(err)
	}
	complete := make(chan struct{})
	if _, err := service.Admit(fixture.ctx, continuation, fixture.actor, request, metadata, func() { close(complete) }); err != nil {
		t.Fatal(err)
	}
	var command integrations.AdmittedCommand
	select {
	case command = <-commands:
	case <-time.After(5 * time.Second):
		t.Fatal("native ACK did not arrive")
	}
	id, _ := domain.ParseCommandID(request.CommandId)
	nha025Status(t, fixture.host, id, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
	persistedDeadline := time.Now().Add(5 * time.Second)
	for {
		pending, found, readErr := store.CommandIntent(fixture.ctx, id)
		if readErr != nil || !found {
			t.Fatal("accepted command disappeared", readErr)
		}
		ackAttempted := len(pending.APIAcknowledgedResponse()) != 0
		if failACK {
			_, readErr = store.InTransactionOutcome(fixture.ctx, func(tx pgx.Tx) error {
				return tx.QueryRow(fixture.ctx, `SELECT is_called FROM public.nha201_ack_attempt`).Scan(&ackAttempted)
			})
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(pending.APIAcknowledgedResponse()) != 0 {
				t.Fatal("failed ACK became durable")
			}
		}
		if pending.State() == storage.IntentResult && ackAttempted {
			break
		}
		if time.Now().After(persistedDeadline) {
			t.Fatal("native ACK never persisted before late proof")
		}
		time.Sleep(time.Millisecond)
	}
	// Equal observed power without a command's qualified native causality cannot
	// confirm the public operation. Only the native owner may append evidence.
	fixture.publish(t, 0, true)
	nha025Status(t, fixture.host, id, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
	select {
	case <-complete:
		t.Fatal("ACK finalized before qualified confirmation")
	default:
	}
	record, found, err := store.CommandIntent(fixture.ctx, id)
	if err != nil || !found || len(record.APITerminalResponse()) != 0 {
		t.Fatal("ACK became a terminal proof", err)
	}
	cause, err := command.Causality()
	if err != nil {
		t.Fatal(err)
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	value := domain.NewSwitchValue(true)
	at := time.Now()
	observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	observation, err = observation.WithCausality(cause)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.session.Publish(fixture.ctx, fixture.bindings[0], observation); err != nil {
		t.Fatal(err)
	}
	select {
	case <-complete:
	case <-time.After(5 * time.Second):
		t.Fatal("late confirmation was not durably completed")
	}
	record, found, err = store.CommandIntent(fixture.ctx, id)
	if err != nil || !found {
		t.Fatal(err)
	}
	terminal := &actions.CommandResponse{}
	if proto.Unmarshal(record.APITerminalResponse(), terminal) != nil || terminal.Status != actions.ActionStatus_ACTION_STATUS_CONFIRMED {
		t.Fatal("native confirmed proof missing", terminal)
	}
	changed, _ := proto.Marshal(&actions.CommandResponse{CommandId: request.CommandId, Status: actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED})
	if _, err := store.AppendCommandAPITerminal(fixture.ctx, id, changed); err == nil {
		t.Fatal("terminal proof was mutable")
	}
	closeService(service)
	restartedDriver := &nha022Driver{}
	restarted := nha050Setup(t, restartedDriver)
	reopened, err := integrations.NewDurableCommandService(restarted.ctx, restarted.host, restarted.gen, store)
	if err != nil {
		t.Fatal(err)
	}
	defer closeService(reopened)
	status, found, err := reopened.Status(restarted.ctx, id)
	if err != nil || !found || status.APITerminal == nil || status.APITerminal.Status != actions.ActionStatus_ACTION_STATUS_CONFIRMED {
		t.Fatal("restart lost qualified proof", status, err)
	}
	if driver.Count() != 1 || restartedDriver.count() != 0 {
		t.Fatal("restart repeated physical command")
	}
}

func TestNHA201CloseDrainsAPIProofWithoutReplayingOrChangingNativeResult(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		name := "without-ack"
		if acknowledged {
			name = "with-native-ack"
		}
		t.Run(name, func(t *testing.T) {
			store, _ := nha024Store(t)
			driver := &nha025Driver{call: func(_ context.Context, command integrations.AdmittedCommand) error {
				if acknowledged {
					return command.Acknowledge()
				}
				return nil
			}}
			fixture := nha050Setup(t, driver)
			service, err := integrations.NewDurableCommandService(fixture.ctx, fixture.host, fixture.gen, store)
			if err != nil {
				t.Fatal(err)
			}
			retained, release := context.WithTimeout(fixture.ctx, time.Minute)
			defer release()
			closeService := func(service *integrations.DurableCommandService) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := service.Close(ctx); err != nil {
					t.Fatal("proof drain failed", err)
				}
			}
			defer closeService(service)
			request, err := fixture.prepared(t, service, "c123456789abcdef0123456789abcdef").Request()
			if err != nil {
				t.Fatal(err)
			}
			capability, _ := apitypes.ParseCapabilityName("homes/synthetic/entities/relay/capabilities/power")
			key, _ := apitypes.ParseRequestID("32345678-1234-4123-8123-123456789abc")
			metadata, err := storage.NewCommandAPIMetadata(capability, &key, sha256.Sum256([]byte("shutdown proof observation")))
			if err != nil {
				t.Fatal(err)
			}
			complete := make(chan struct{})
			if _, err := service.Admit(fixture.ctx, retained, fixture.actor, request, metadata, func() { close(complete) }); err != nil {
				t.Fatal(err)
			}
			id, _ := domain.ParseCommandID(request.CommandId)
			deadline := time.Now().Add(5 * time.Second)
			var before storage.IntentRecord
			for {
				var found bool
				before, found, err = store.CommandIntent(fixture.ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if found && before.State() == storage.IntentResult && (!acknowledged || len(before.APIAcknowledgedResponse()) != 0) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("native result or ACK was not persisted")
				}
				time.Sleep(time.Millisecond)
			}
			if len(before.APITerminalResponse()) != 0 {
				t.Fatal("proof wait already ended")
			}
			rawBefore := before.Response()
			closeService(service)
			if retained.Err() != nil {
				t.Fatal("service drain cancelled retained effect context")
			}
			select {
			case <-complete:
			case <-time.After(time.Second):
				t.Fatal("retained occurrence cleanup did not finish after drain")
			}
			after, found, err := store.CommandIntent(fixture.ctx, id)
			if err != nil || !found {
				t.Fatal(err)
			}
			if string(rawBefore) != string(after.Response()) {
				t.Fatal("API proof changed native dispatch response")
			}
			expected := actions.ActionStatus_ACTION_STATUS_INDETERMINATE
			if acknowledged {
				expected = actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED
			}
			terminal := &actions.CommandResponse{}
			if proto.Unmarshal(after.APITerminalResponse(), terminal) != nil || terminal.Status != expected {
				t.Fatal("drain promoted or erased native evidence", terminal)
			}
			restartedDriver := &nha022Driver{}
			restarted := nha050Setup(t, restartedDriver)
			reopened, err := integrations.NewDurableCommandService(restarted.ctx, restarted.host, restarted.gen, store)
			if err != nil {
				t.Fatal(err)
			}
			defer closeService(reopened)
			status, found, err := reopened.Status(restarted.ctx, id)
			if err != nil || !found || status.APITerminal == nil || status.APITerminal.Status != expected {
				t.Fatal("restart changed persisted evidence", status, err)
			}
			if driver.Count() != 1 || restartedDriver.count() != 0 {
				t.Fatal("drain/restart repeated effect")
			}
		})
	}
}
