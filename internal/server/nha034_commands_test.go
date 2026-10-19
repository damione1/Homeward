package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/jobs"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
)

type nha034Generation struct {
	mu       sync.Mutex
	revision uint64
}

func (g *nha034Generation) WithCurrent(ctx context.Context, fn func(uint64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return fn(g.revision)
}
func (g *nha034Generation) set(revision uint64) { g.mu.Lock(); g.revision = revision; g.mu.Unlock() }

type nha034JobDriver struct {
	effects atomic.Int32
	verify  func(context.Context, integrations.AdmittedCommand) error
}

func (driver *nha034JobDriver) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	if !command.ReadyForTransport() {
		return integrations.ErrCommandAuthorizationDenied
	}
	if driver.verify != nil {
		if err := driver.verify(ctx, command); err != nil {
			return err
		}
	}
	driver.effects.Add(1)
	return nil
}

type nha034CommandRig struct {
	host       *integrations.Manager
	fixture    *fake.Fixture
	session    *integrations.Session
	entity     domain.EntityID
	driver     *nha034JobDriver
	generation *nha034Generation
}

func nha034Host(t *testing.T, clock *nha141Clock, coordinator *sessionCoordinator) nha034CommandRig {
	t.Helper()
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.AttachAuthorization(coordinator.authority); err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha034-isolated-relay")
	key, _ := integrations.NewSourceKey("nha034-device", "relay")
	identities, err := fixture.Resolve(ctx, instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	identity := identities[0]
	alias, _ := domain.ParseHAAlias("switch.nha034_isolated")
	record, err := integrations.NewRegistryRecord(identity.Device, identity.Entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
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
	candidate, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "relay", DisplayName: "NHA034 isolated switch", Constraints: &registry.CapabilityDescription_Switch{Switch: &registry.SwitchConstraints{SetOn: true}}}})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := session.Discover(ctx, []integrations.Candidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	value := domain.NewSwitchValue(false)
	at := clock.Now()
	observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Publish(ctx, bindings[0], observation); err != nil {
		t.Fatal(err)
	}
	driver := new(nha034JobDriver)
	if err := host.AttachCommandDriver(session, driver); err != nil {
		t.Fatal(err)
	}
	return nha034CommandRig{host, fixture, session, identity.Entity, driver, &nha034Generation{revision: 7}}
}

func nha034ActionPolicy(t *testing.T, store *storage.Store, coordinator *sessionCoordinator, account contracts.AccountID, entity domain.EntityID, allowed bool) {
	t.Helper()
	ctx := context.Background()
	guard, err := coordinator.enter(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.release()
	snapshot, err := store.GetAuthorizationPolicy(ctx, account, account)
	if err != nil {
		t.Fatal(err)
	}
	version, err := snapshot.Version()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := authorization.ExpectVersion(version)
	if err != nil {
		t.Fatal(err)
	}
	source, err := authorization.EmptySource(account)
	if allowed {
		accountText, _ := account.Value()
		entityText, _ := entity.Value()
		text := fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n  - scope: {family: actions, operation: read}\n    objects: {entities: {explicit: [{entity_id: %q, capabilities: {all: {}}}]}}\n  - scope: {family: actions, operation: write}\n    objects: {entities: {explicit: [{entity_id: %q, capabilities: {all: {}}}]}}\n", accountText, entityText, entityText)
		source, err = authorization.AdmitPolicySource(account, []byte(text))
	}
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := store.ReplaceAuthorizationPolicy(ctx, account, account, source, expected)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := outcome.Result(); err != nil {
		t.Fatal(err)
	}
	if err := guard.refreshAuthorization(ctx); err != nil {
		t.Fatal(err)
	}
}
func nha034CommandDefinition(t *testing.T, name string, rig nha034CommandRig, owner storage.SessionOwner, now time.Time) (jobs.Definition, jobs.OccurrenceID, jobs.IdempotencyKey) {
	t.Helper()
	text, _ := owner.Account.Value()
	actor, _ := domain.ParseActorID(text)
	grant, err := jobs.NewSessionGrant(actor, owner.Session)
	if err != nil {
		t.Fatal(err)
	}
	entity, _ := rig.entity.Value()
	wire := &actions.CommandRequest{ConfigRevision: 7, Target: &registry.EntityRef{Reference: &registry.EntityRef_EntityId{EntityId: entity}}, CapabilitySchemaId: contracts.SwitchSchema, Endpoint: "relay", Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: true}}}}
	work, err := jobs.NewSessionCommand(grant, wire)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := jobs.ParseJobID(name)
	version, _ := jobs.NewVersion(1)
	definition, err := jobs.AdmitDefinition(jobs.DefinitionInput{ID: id, Version: version, DeadlineUTC: now, Timezone: "America/Toronto", ExpiresAtUTC: now.Add(time.Hour), LatePolicy: jobs.RunBeforeExpiry(), Work: work})
	if err != nil {
		t.Fatal(err)
	}
	occurrence, _ := jobs.ParseOccurrenceID(name + "-occurrence")
	key, _ := jobs.ParseIdempotencyKey(name + "-idempotency")
	return definition, occurrence, key
}
func nha034ServerExec(t *testing.T, store *storage.Store, query string) {
	t.Helper()
	if err := store.InTransaction(context.Background(), func(tx pgx.Tx) error { _, err := tx.Exec(context.Background(), query); return err }); err != nil {
		t.Fatal(err)
	}
}
func nha034AwaitWorkers(t *testing.T, ctx context.Context, timer *nha034ServerTimer, count int) {
	t.Helper()
	for range count {
		select {
		case duration := <-timer.entered:
			if duration != jobs.PollInterval {
				t.Fatalf("poll duration=%s", duration)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
func nha034Tick(t *testing.T, ctx context.Context, timer *nha034ServerTimer) {
	t.Helper()
	select {
	case timer.tick <- struct{}{}:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	nha034AwaitWorkers(t, ctx, timer, 1)
}
func nha034ReadJob(t *testing.T, repository *jobs.Repository, occurrence jobs.OccurrenceID) jobs.Record {
	t.Helper()
	record, found, err := repository.Get(context.Background(), occurrence)
	if err != nil || !found {
		t.Fatalf("job read: found=%v err=%v", found, err)
	}
	return record
}

func TestNHA034SharedCommandLedgerAndCurrentConfigurationOwnerPermissions(t *testing.T) {
	store := nha034ServerStore(t)
	account := nha034Account(t, store)
	now := time.Now().UTC().Truncate(time.Microsecond)
	clock := newNHA141Clock(now)
	sessions, err := prepareLocalAuthorization(context.Background(), store, clock)
	if err != nil {
		t.Fatal(err)
	}
	owner, _ := nha034Session(t, store, account, "nha034-command-session", now)
	rig := nha034Host(t, clock, sessions)
	accountID, _ := account.ID()
	nha034ActionPolicy(t, store, sessions, accountID, rig.entity, true)
	timer := &nha034ServerTimer{entered: make(chan time.Duration, 8), tick: make(chan struct{})}
	var services DurableJobsServices
	composition, err := NewDurableJobsComposition(DurableJobsInput{Commands: &DurableJobCommandInput{rig.generation, rig.fixture}, Clock: clock, Timer: timer, Configure: func(_ context.Context, current DurableJobsServices) error { services = current; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runtime, err := newDurableJobsRuntime(ctx, store, rig.host, sessions, composition)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := runtime.Close(closeCtx); err != nil {
			t.Error(err)
		}
		if _, err := rig.host.ClosePreparedRuntime(closeCtx); err != nil {
			t.Error(err)
		}
	})
	shared, present := services.Commands()
	if !present || shared != runtime.services.commands {
		t.Fatal("command facade did not receive same service reference")
	}
	if _, err := integrations.NewDurableCommandService(ctx, rig.host, rig.generation, store); !errors.Is(err, integrations.ErrDurableRuntimeBusy) {
		t.Fatalf("second runtime admitted: %v", err)
	}
	rig.driver.verify = func(ctx context.Context, command integrations.AdmittedCommand) error {
		id, _, _, err := command.Identity()
		if err != nil {
			return err
		}
		intent, found, err := store.CommandIntent(ctx, id)
		if err != nil || !found || intent.State() != storage.IntentDispatching {
			return fmt.Errorf("driver entered before committed NHA-024 intent")
		}
		idText, _ := id.Value()
		var jobState int16
		if err := store.InTransaction(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT state FROM public.scheduled_jobs WHERE command_id=$1`, idText).Scan(&jobState)
		}); err != nil || jobState != int16(jobs.Dispatching) {
			return fmt.Errorf("driver entered before committed job dispatch point")
		}
		return nil
	}
	if err := runtime.Start(ctx); err != nil {
		t.Fatal(err)
	}
	nha034AwaitWorkers(t, ctx, timer, jobs.Workers)
	// The same healthy route is refused for an active real user whose current
	// 050 policy has no action grant; no command intent or driver entry follows.
	adminID, _ := account.ID()
	userInput, err := newAccountInput(ctx, "NHA034 denied user", "NHA034 denied user", "NHA034 denied user password", authv1.AccountRole_ACCOUNT_ROLE_USER)
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.CreateAccount(ctx, adminID, userInput)
	if err != nil {
		t.Fatal(err)
	}
	userOwner, _ := nha034Session(t, store, user, "nha034-denied-session", now)
	deniedDefinition, deniedOccurrence, deniedKey := nha034CommandDefinition(t, "denied-permissions", rig, userOwner, now)
	if _, _, err := services.Schedule(ctx, deniedDefinition, deniedOccurrence, deniedKey); err != nil {
		t.Fatal(err)
	}
	nha034Tick(t, ctx, timer)
	denied := nha034ReadJob(t, services.repository, deniedOccurrence)
	deniedID, _ := denied.CommandID()
	if denied.State() != jobs.Rejected || rig.driver.effects.Load() != 0 {
		t.Fatalf("permission denial reached driver: state=%d effects=%d", denied.State(), rig.driver.effects.Load())
	}
	if _, found, err := store.CommandIntent(ctx, deniedID); err != nil || found {
		t.Fatalf("denied action created durable command intent: %v %v", found, err)
	}
	for _, test := range []struct {
		name    string
		mutate  func()
		want    jobs.State
		effects int32
		intent  bool
	}{
		{"nominal", func() {}, jobs.Succeeded, 1, true},
		{"revoked-current-permissions", func() { nha034ActionPolicy(t, store, sessions, accountID, rig.entity, false) }, jobs.Rejected, 1, false},
		{"restored-current-permissions", func() { nha034ActionPolicy(t, store, sessions, accountID, rig.entity, true) }, jobs.Succeeded, 2, true},
		{"stale-configuration", func() { rig.generation.set(8) }, jobs.Rejected, 2, true},
		{"current-configuration", func() { rig.generation.set(7) }, jobs.Succeeded, 3, true},
		{"revoked-owner-route", func() {
			if err := rig.host.Revoke(rig.session); err != nil {
				t.Fatal(err)
			}
		}, jobs.Rejected, 3, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			definition, occurrence, key := nha034CommandDefinition(t, test.name, rig, owner, now)
			if _, _, err := services.Schedule(ctx, definition, occurrence, key); err != nil {
				t.Fatal(err)
			}
			test.mutate()
			nha034Tick(t, ctx, timer)
			record := nha034ReadJob(t, services.repository, occurrence)
			if record.State() != test.want || rig.driver.effects.Load() != test.effects {
				t.Fatalf("job=%d effects=%d want=%d/%d", record.State(), rig.driver.effects.Load(), test.want, test.effects)
			}
			id, present := record.CommandID()
			if !present {
				t.Fatal("stable command identity absent")
			}
			intent, found, err := store.CommandIntent(ctx, id)
			if err != nil || found != test.intent || (test.intent && intent.State() != storage.IntentResult) {
				t.Fatalf("shared ledger result: found=%v state=%d err=%v", found, intent.State(), err)
			}
		})
	}
}

func TestNHA034PossibleEffectLostFinalizationReopensIndeterminateWithoutResend(t *testing.T) {
	store := nha034ServerStore(t)
	account := nha034Account(t, store)
	now := time.Now().UTC().Truncate(time.Microsecond)
	clock := newNHA141Clock(now)
	sessions, err := prepareLocalAuthorization(context.Background(), store, clock)
	if err != nil {
		t.Fatal(err)
	}
	owner, _ := nha034Session(t, store, account, "nha034-crash-command-session", now)
	rig := nha034Host(t, clock, sessions)
	accountID, _ := account.ID()
	nha034ActionPolicy(t, store, sessions, accountID, rig.entity, true)
	definition, occurrence, key := nha034CommandDefinition(t, "possible-effect", rig, owner, now)
	firstTimer := &nha034ServerTimer{entered: make(chan time.Duration, 8), tick: make(chan struct{})}
	composition, err := NewDurableJobsComposition(DurableJobsInput{Commands: &DurableJobCommandInput{rig.generation, rig.fixture}, Clock: clock, Timer: firstTimer, Configure: func(ctx context.Context, services DurableJobsServices) error {
		_, _, err := services.Schedule(ctx, definition, occurrence, key)
		return err
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runtime, err := newDurableJobsRuntime(ctx, store, rig.host, sessions, composition)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := runtime.Close(closeCtx); err != nil {
			t.Error(err)
		}
		if _, err := rig.host.ClosePreparedRuntime(closeCtx); err != nil {
			t.Error(err)
		}
	})
	nha034ServerExec(t, store, `CREATE FUNCTION public.nha034_lost_finalization() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'simulated lost finalization'; END $$;
        CREATE TRIGGER nha034_lost_command_finalization BEFORE UPDATE ON public.command_intents FOR EACH ROW WHEN (OLD.state='dispatching' AND NEW.state<>'dispatching') EXECUTE FUNCTION public.nha034_lost_finalization();
        CREATE TRIGGER nha034_lost_job_finalization BEFORE UPDATE ON public.scheduled_jobs FOR EACH ROW WHEN (OLD.state=3 AND NEW.state<>3) EXECUTE FUNCTION public.nha034_lost_finalization()`)
	if err := runtime.Start(ctx); err != nil {
		t.Fatal(err)
	}
	nha034AwaitWorkers(t, ctx, firstTimer, jobs.Workers)
	if status := runtime.services.Status(); status.Phase() != jobs.SchedulerStorageDegraded {
		t.Fatalf("lost finalization did not degrade jobs: phase=%d", status.Phase())
	}
	// Simulated process stop, not an actual OS kill. The transient storage
	// error alone does not terminate the operational Host or scheduler.
	if err := runtime.Close(ctx); err != nil {
		t.Fatal(err)
	}
	record := nha034ReadJob(t, runtime.services.repository, occurrence)
	id, _ := record.CommandID()
	intent, found, err := store.CommandIntent(ctx, id)
	if record.State() != jobs.Dispatching || err != nil || !found || intent.State() != storage.IntentDispatching || rig.driver.effects.Load() != 1 {
		t.Fatalf("crash seam: job=%d intent=%d effects=%d err=%v", record.State(), intent.State(), rig.driver.effects.Load(), err)
	}
	if _, err := rig.host.ClosePreparedRuntime(ctx); err != nil {
		t.Fatal(err)
	}
	nha034ServerExec(t, store, `DROP TRIGGER nha034_lost_command_finalization ON public.command_intents; DROP TRIGGER nha034_lost_job_finalization ON public.scheduled_jobs; DROP FUNCTION public.nha034_lost_finalization()`)
	clock.Advance(now.Add(jobs.LeaseDuration + time.Microsecond))
	reopenedSessions, err := prepareLocalAuthorization(ctx, store, clock)
	if err != nil {
		t.Fatal(err)
	}
	reopened := nha034Host(t, clock, reopenedSessions)
	timer := &nha034ServerTimer{entered: make(chan time.Duration, 8), tick: make(chan struct{})}
	nextComposition, err := NewDurableJobsComposition(DurableJobsInput{Commands: &DurableJobCommandInput{reopened.generation, reopened.fixture}, Clock: clock, Timer: timer})
	if err != nil {
		t.Fatal(err)
	}
	next, err := newDurableJobsRuntime(ctx, store, reopened.host, reopenedSessions, nextComposition)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := next.Close(closeCtx); err != nil {
			t.Error(err)
		}
		if _, err := reopened.host.ClosePreparedRuntime(closeCtx); err != nil {
			t.Error(err)
		}
	})
	if err := next.Start(ctx); err != nil {
		t.Fatal(err)
	}
	nha034AwaitWorkers(t, ctx, timer, jobs.Workers)
	after := nha034ReadJob(t, next.services.repository, occurrence)
	afterID, _ := after.CommandID()
	intent, found, err = store.CommandIntent(ctx, afterID)
	if after.State() != jobs.Indeterminate || afterID != id || after.IdempotencyKey() != key || err != nil || !found || intent.State() != storage.IntentIndeterminate || reopened.driver.effects.Load() != 0 || rig.driver.effects.Load() != 1 {
		t.Fatalf("restart replayed possible effect: job=%d intent=%d original=%d restarted=%d err=%v", after.State(), intent.State(), rig.driver.effects.Load(), reopened.driver.effects.Load(), err)
	}
}
