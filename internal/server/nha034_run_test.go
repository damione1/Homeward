package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/jobs"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

// These tests use the production RunWithFactory composition. The fake pilot's
// process-local wait proves the lifecycle boundary; Homeward has no ordinary
// script engine in this tranche, so this is not script-engine qualification.
type nha034OrdinaryWaitPilot struct {
	started   chan struct{}
	trigger   chan struct{}
	waiting   chan struct{}
	release   chan struct{}
	cancelled chan struct{}
	finished  chan struct{}
	effects   atomic.Int32
}

func nha034NewOrdinaryWaitPilot() *nha034OrdinaryWaitPilot {
	return &nha034OrdinaryWaitPilot{
		started: make(chan struct{}), trigger: make(chan struct{}), waiting: make(chan struct{}),
		release: make(chan struct{}), cancelled: make(chan struct{}), finished: make(chan struct{}),
	}
}

func (p *nha034OrdinaryWaitPilot) Start(_ context.Context, _ integrations.SessionHost, tasks integrations.Tasks) error {
	return tasks.Go(func(ctx context.Context) error {
		defer close(p.finished)
		close(p.started)
		select {
		case <-ctx.Done():
			return nil
		case <-p.trigger:
		}
		close(p.waiting)
		select {
		case <-ctx.Done():
			close(p.cancelled)
			return nil
		case <-p.release:
			if ctx.Err() == nil {
				p.effects.Add(1)
			}
		}
		<-ctx.Done()
		return nil
	})
}

func (*nha034OrdinaryWaitPilot) Stop(context.Context) error { return nil }

type nha034RunClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *nha034RunClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *nha034RunClock) set(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

type nha034RunHandle struct {
	cancel context.CancelFunc
	ready  chan struct{}
	done   chan struct{}
	err    error // Published by closing done; read only after observing done.
}

func nha034LaunchRun(t *testing.T, cfg config.Config, listen ListenFunc, factory RuntimeFactory) *nha034RunHandle {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	run := &nha034RunHandle{cancel: cancel, ready: make(chan struct{}), done: make(chan struct{})}
	go func() {
		run.err = RunWithFactory(ctx, cfg, listen, func(net.Addr) { close(run.ready) }, factory)
		close(run.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-run.done:
		case <-time.After(12 * time.Second):
			t.Error("NHA034 Run did not join during cleanup")
		}
	})
	return run
}

func (r *nha034RunHandle) waitReady(t *testing.T) {
	t.Helper()
	select {
	case <-r.ready:
	case <-r.done:
		t.Fatalf("Run exited before ready: %v", r.err)
	case <-time.After(10 * time.Second):
		t.Fatal("NHA034 Run did not become ready")
	}
}

func (r *nha034RunHandle) waitDone(t *testing.T) error {
	t.Helper()
	select {
	case <-r.done:
		return r.err
	case <-time.After(10 * time.Second):
		t.Fatal("NHA034 Run exceeded its shared shutdown bound")
		return nil
	}
}

func nha034WaitSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("NHA034 did not observe %s", name)
	}
}

func nha034WaitRunPolls(t *testing.T, timer *nha034ServerTimer) {
	t.Helper()
	for range jobs.Workers {
		select {
		case duration := <-timer.entered:
			if duration != jobs.PollInterval {
				t.Fatalf("healthy Run poll interval: %s", duration)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Run workers did not reach bounded polling barriers")
		}
	}
}

func nha034RunDefinition(t *testing.T, name string, deadline time.Time) (jobs.Definition, jobs.OccurrenceID, jobs.IdempotencyKey) {
	t.Helper()
	id, err := jobs.ParseJobID(name)
	if err != nil {
		t.Fatal(err)
	}
	version, err := jobs.NewVersion(1)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := jobs.ParseBackupKey(name + "-backup")
	if err != nil {
		t.Fatal(err)
	}
	work, err := jobs.NewSystemBackup(backup)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := jobs.AdmitDefinition(jobs.DefinitionInput{
		ID: id, Version: version, DeadlineUTC: deadline, Timezone: "America/Toronto",
		ExpiresAtUTC: deadline.Add(time.Hour), LatePolicy: jobs.RunBeforeExpiry(), Work: work,
	})
	if err != nil {
		t.Fatal(err)
	}
	occurrence, err := jobs.ParseOccurrenceID(name + "-occurrence")
	if err != nil {
		t.Fatal(err)
	}
	key, err := jobs.ParseIdempotencyKey(name + "-idempotency")
	if err != nil {
		t.Fatal(err)
	}
	return definition, occurrence, key
}

func nha034RunServices(t *testing.T, ready <-chan DurableJobsServices) DurableJobsServices {
	t.Helper()
	select {
	case services := <-ready:
		return services
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not publish its admitted job services")
		return DurableJobsServices{}
	}
}

func nha034RunRecord(t *testing.T, services DurableJobsServices, occurrence jobs.OccurrenceID) jobs.Record {
	t.Helper()
	record, found, err := services.Get(context.Background(), occurrence)
	if err != nil || !found {
		t.Fatalf("Run occurrence: found=%v err=%v", found, err)
	}
	return record
}

func TestNHA034RunRestartsDurableBackupAndCancelsOrdinaryWait(t *testing.T) {
	cluster := testpg.Start(t)
	cfg := nha030ServerConfig(t, cluster.Port)
	observer := nha030ServerObserver(t, cluster.Port)
	now := time.Now().UTC().Truncate(time.Microsecond)
	clock := &nha034RunClock{now: now}
	definition, occurrence, key := nha034RunDefinition(t, "run-backup", now.Add(time.Hour))
	backup := &nha034ServerBackup{}
	instance, err := domain.ParseIntegrationInstanceID("nha034-ordinary-wait")
	if err != nil {
		t.Fatal(err)
	}
	makeFactory := func(pilot *nha034OrdinaryWaitPilot, timer *nha034ServerTimer, expected jobs.CreateResult, ready chan<- DurableJobsServices) RuntimeFactory {
		return func(context.Context, *storage.Store) (RuntimePlan, error) {
			spec, err := integrations.NewPilotSpec(instance, func() integrations.Pilot { return pilot })
			if err != nil {
				return RuntimePlan{}, err
			}
			composition, err := NewDurableJobsComposition(DurableJobsInput{
				Backup: backup, Clock: clock, Timer: timer,
				Configure: func(ctx context.Context, services DurableJobsServices) error {
					result, outcome, err := services.Schedule(ctx, definition, occurrence, key)
					if err != nil || result != expected || outcome != storage.TransactionCommitted {
						return fmt.Errorf("NHA034 Run schedule result=%d outcome=%d: %w", result, outcome, err)
					}
					ready <- services
					return nil
				},
			})
			return RuntimePlan{Identity: unconfiguredIdentity{}, Pilots: []integrations.PilotSpec{spec}, DurableJobs: composition}, err
		}
	}
	var listener net.ListenConfig
	firstPilot := nha034NewOrdinaryWaitPilot()
	firstTimer := &nha034ServerTimer{entered: make(chan time.Duration, jobs.Workers), tick: make(chan struct{}, jobs.Workers)}
	firstServicesReady := make(chan DurableJobsServices, 1)
	first := nha034LaunchRun(t, cfg, listener.Listen, makeFactory(firstPilot, firstTimer, jobs.Created, firstServicesReady))
	first.waitReady(t)
	firstServices := nha034RunServices(t, firstServicesReady)
	nha034WaitRunPolls(t, firstTimer)
	nha034WaitSignal(t, firstPilot.started, "supervised ordinary task start")
	close(firstPilot.trigger)
	nha034WaitSignal(t, firstPilot.waiting, "ordinary wait entry")
	before := nha034RunRecord(t, firstServices, occurrence)
	if before.State() != jobs.Pending || before.IdempotencyKey() != key || backup.effects.Load() != 0 {
		t.Fatal("future durable backup did not remain persisted pending")
	}
	first.cancel()
	if err := first.waitDone(t); err != nil {
		t.Fatal(err)
	}
	nha034WaitSignal(t, firstPilot.cancelled, "ordinary wait cancellation")
	nha034WaitSignal(t, firstPilot.finished, "supervised ordinary task join")
	close(firstPilot.release) // An old volatile timer cannot revive a joined task.
	if firstPilot.effects.Load() != 0 {
		t.Fatal("cancelled ordinary wait emitted an effect")
	}

	secondPilot := nha034NewOrdinaryWaitPilot()
	secondTimer := &nha034ServerTimer{entered: make(chan time.Duration, jobs.Workers), tick: make(chan struct{}, jobs.Workers)}
	secondServicesReady := make(chan DurableJobsServices, 1)
	second := nha034LaunchRun(t, cfg, listener.Listen, makeFactory(secondPilot, secondTimer, jobs.AlreadyPresent, secondServicesReady))
	second.waitReady(t)
	secondServices := nha034RunServices(t, secondServicesReady)
	nha034WaitRunPolls(t, secondTimer)
	nha034WaitSignal(t, secondPilot.started, "reopened supervised ordinary task start")
	reopened := nha034RunRecord(t, secondServices, occurrence)
	if reopened.State() != jobs.Pending || reopened.Occurrence() != before.Occurrence() || reopened.IdempotencyKey() != before.IdempotencyKey() {
		t.Fatal("Run reopen changed the pending durable occurrence identity")
	}
	clock.set(definition.Input().DeadlineUTC)
	for range jobs.Workers {
		secondTimer.tick <- struct{}{}
	}
	nha034WaitRunPolls(t, secondTimer)
	if record := nha034RunRecord(t, secondServices, occurrence); record.State() != jobs.Succeeded || backup.effects.Load() != 1 {
		t.Fatalf("reopened durable backup: state=%d effects=%d", record.State(), backup.effects.Load())
	}
	select {
	case <-secondPilot.waiting:
		t.Fatal("reopen resumed an ordinary wait without a new trigger")
	default:
	}
	if firstPilot.effects.Load() != 0 || secondPilot.effects.Load() != 0 {
		t.Fatal("ordinary work survived the first Run")
	}
	queryCtx, queryCancel := context.WithTimeout(context.Background(), time.Second)
	var definitions, occurrences int
	err = observer.QueryRow(queryCtx, `SELECT (SELECT count(*) FROM public.scheduled_job_definitions),count(*) FROM public.scheduled_jobs`).Scan(&definitions, &occurrences)
	queryCancel()
	if err != nil || definitions != 1 || occurrences != 1 {
		t.Fatalf("ordinary wait acquired durable storage: definitions=%d occurrences=%d err=%v", definitions, occurrences, err)
	}
	second.cancel()
	if err := second.waitDone(t); err != nil {
		t.Fatal(err)
	}
	nha034WaitSignal(t, secondPilot.finished, "reopened ordinary task join")
}

type nha034RunBlockedBackup struct {
	entered chan struct{}
	release chan struct{}
	left    chan struct{}
}

func (b *nha034RunBlockedBackup) ExecuteBackup(context.Context, jobs.BackupKey, jobs.IdempotencyKey) (jobs.Outcome, error) {
	close(b.entered)
	defer close(b.left)
	<-b.release // Deliberately violates cancellation to exercise actual Done ownership.
	return jobs.Uncertain, nil
}

func nha034ProbeRunStore(store *storage.Store) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return store.InTransaction(ctx, func(tx pgx.Tx) error { return tx.QueryRow(ctx, "SELECT 1").Scan(new(int)) })
}

func nha034WaitRunStoreClosed(t *testing.T, store *storage.Store) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := nha034ProbeRunStore(store); err != nil {
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("Run kept Store open after all job owners joined")
		}
	}
}

func TestNHA034RunRetainsStoreUntilTimedOutJobActuallyDrains(t *testing.T) {
	cluster := testpg.Start(t)
	cfg := nha030ServerConfig(t, cluster.Port)
	observer := nha030ServerObserver(t, cluster.Port)
	definition, occurrence, key := nha034RunDefinition(t, "run-drain", time.Now().UTC().Truncate(time.Microsecond))
	backup := &nha034RunBlockedBackup{entered: make(chan struct{}), release: make(chan struct{}), left: make(chan struct{})}
	servicesReady := make(chan DurableJobsServices, 1)
	storeReady := make(chan *storage.Store, 1)
	composition, err := NewDurableJobsComposition(DurableJobsInput{
		Backup: backup, Clock: jobs.RealClock{}, Timer: jobs.RealTimer{},
		Configure: func(ctx context.Context, services DurableJobsServices) error {
			result, outcome, err := services.Schedule(ctx, definition, occurrence, key)
			if err != nil || result != jobs.Created || outcome != storage.TransactionCommitted {
				return fmt.Errorf("NHA034 drain schedule result=%d outcome=%d: %w", result, outcome, err)
			}
			servicesReady <- services
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var listener net.ListenConfig
	run := nha034LaunchRun(t, cfg, listener.Listen, func(_ context.Context, store *storage.Store) (RuntimePlan, error) {
		storeReady <- store
		return RuntimePlan{Identity: unconfiguredIdentity{}, DurableJobs: composition}, nil
	})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(backup.release) }) })
	run.waitReady(t)
	services := nha034RunServices(t, servicesReady)
	store := <-storeReady
	nha034WaitSignal(t, backup.entered, "backup handler entry")
	started := time.Now()
	run.cancel()
	if err := run.waitDone(t); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run lost the shared drain deadline: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 9*time.Second {
		t.Fatalf("Run multiplied its global six-second budget: %s", elapsed)
	}
	select {
	case <-services.scheduler.Done():
		t.Fatal("job Done closed before its handler actually left")
	default:
	}
	if err := nha034ProbeRunStore(store); err != nil {
		t.Fatalf("Run closed Store while a job still owned it: %v", err)
	}
	if record := nha034RunRecord(t, services, occurrence); record.State() != jobs.Dispatching {
		t.Fatalf("timed-out live handler was prematurely finalized: %d", record.State())
	}
	release.Do(func() { close(backup.release) })
	nha034WaitSignal(t, backup.left, "blocked backup release")
	nha034WaitSignal(t, services.scheduler.Done(), "actual job drainage")
	nha034WaitRunStoreClosed(t, store)
	// A healthy independent connection makes a pool-close failure distinct from
	// a database outage in this controlled fixture.
	queryCtx, queryCancel := context.WithTimeout(context.Background(), time.Second)
	defer queryCancel()
	var state int16
	if err := observer.QueryRow(queryCtx, `SELECT state FROM public.scheduled_jobs WHERE occurrence_id=$1`, "run-drain-occurrence").Scan(&state); err != nil || state != int16(jobs.Indeterminate) {
		t.Fatalf("drained uncertain job: state=%d err=%v", state, err)
	}
}

func TestNHA034RunPreparationFailureClosesJobsAndStore(t *testing.T) {
	cluster := testpg.Start(t)
	cfg := nha030ServerConfig(t, cluster.Port)
	observer := nha030ServerObserver(t, cluster.Port)
	definition, occurrence, key := nha034RunDefinition(t, "run-preparation", time.Now().UTC().Truncate(time.Microsecond).Add(time.Hour))
	failure := errors.New("NHA034 synthetic trusted configuration failure")
	servicesReady := make(chan DurableJobsServices, 1)
	storeReady := make(chan *storage.Store, 1)
	composition, err := NewDurableJobsComposition(DurableJobsInput{
		Backup: &nha034ServerBackup{}, Clock: jobs.RealClock{}, Timer: jobs.RealTimer{},
		Configure: func(ctx context.Context, services DurableJobsServices) error {
			result, outcome, err := services.Schedule(ctx, definition, occurrence, key)
			if err != nil || result != jobs.Created || outcome != storage.TransactionCommitted {
				return fmt.Errorf("NHA034 preparation schedule result=%d outcome=%d: %w", result, outcome, err)
			}
			servicesReady <- services
			return failure
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var listenerCalls atomic.Int32
	run := nha034LaunchRun(t, cfg, func(context.Context, string, string) (net.Listener, error) {
		listenerCalls.Add(1)
		return nil, errors.New("NHA034 preparation reached listener")
	}, func(_ context.Context, store *storage.Store) (RuntimePlan, error) {
		storeReady <- store
		return RuntimePlan{Identity: unconfiguredIdentity{}, DurableJobs: composition}, nil
	})
	if err := run.waitDone(t); !errors.Is(err, failure) {
		t.Fatalf("Run lost preparation failure: %v", err)
	}
	services := nha034RunServices(t, servicesReady)
	store := <-storeReady
	nha034WaitSignal(t, services.scheduler.Done(), "unstarted scheduler drainage")
	if listenerCalls.Load() != 0 || services.Status().Phase() != jobs.SchedulerStopped {
		t.Fatal("failed preparation left a running scheduler or listener")
	}
	_, outcome, err := services.Schedule(context.Background(), definition, occurrence, key)
	var jobError *jobs.Error
	if outcome != storage.TransactionNotCommitted || !errors.As(err, &jobError) || jobError.Kind() != jobs.Closed {
		t.Fatalf("failed runtime still admitted durable jobs: outcome=%d err=%v", outcome, err)
	}
	nha034WaitRunStoreClosed(t, store)
	queryCtx, queryCancel := context.WithTimeout(context.Background(), time.Second)
	defer queryCancel()
	var state int16
	if err := observer.QueryRow(queryCtx, `SELECT state FROM public.scheduled_jobs WHERE occurrence_id=$1`, "run-preparation-occurrence").Scan(&state); err != nil || state != int16(jobs.Pending) {
		t.Fatalf("acknowledged preparation job lost its durable row: state=%d err=%v", state, err)
	}
}
