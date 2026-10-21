package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/recordermaintenance"
)

// The test advances the ordinary minute ticker. It does not call cycle or a
// substitute maintenance function; Run owns the real worker and the real Store.
type nha032RunClock struct {
	mu          sync.Mutex
	at          time.Time
	ticks       chan time.Time
	created     atomic.Int32
	stopped     chan struct{}
	stopRelease chan struct{}
	stopOnce    sync.Once
}

func nha032Clock() *nha032RunClock {
	return &nha032RunClock{at: time.Now().UTC(), ticks: make(chan time.Time, 1), stopped: make(chan struct{}), stopRelease: make(chan struct{})}
}
func (c *nha032RunClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *nha032RunClock) NewTicker(d time.Duration) recordermaintenance.Ticker {
	if d != recordermaintenance.CycleInterval {
		panic("unexpected maintenance cadence")
	}
	c.created.Add(1)
	return c
}
func (c *nha032RunClock) C() <-chan time.Time { return c.ticks }
func (c *nha032RunClock) Stop()               { c.stopOnce.Do(func() { close(c.stopped) }); <-c.stopRelease }
func (c *nha032RunClock) advance() {
	c.mu.Lock()
	c.at = c.at.Add(recordermaintenance.CycleInterval)
	at := c.at
	c.mu.Unlock()
	select {
	case c.ticks <- at:
	default:
	}
}
func (c *nha032RunClock) releaseStop() {
	select {
	case <-c.stopRelease:
	default:
		close(c.stopRelease)
	}
}
func nha032ObservedWorker(t *testing.T, observed <-chan *recordermaintenance.Worker) *recordermaintenance.Worker {
	t.Helper()
	select {
	case owner := <-observed:
		return owner
	case <-time.After(8 * time.Second):
		t.Fatal("Run did not prepare its maintenance owner")
		return nil
	}
}

func TestNHA032RunProgressesWhileMaintenancePostgresIsLocked(t *testing.T) {
	environment := nha042ServerEnvironmentFor(t)
	clock := nha032Clock()
	clock.releaseStop()
	observed := make(chan *recordermaintenance.Worker, 1)
	dependencies := runDependencies{maintenanceClock: clock, observeRecorderMaintenance: func(w *recordermaintenance.Worker) { observed <- w }}
	rig := nha042StartServer(t, environment, "- stop: done\n", "", nha042ServerOptions{dependencies: &dependencies})
	rig.run.waitReady(t)
	owner := nha032ObservedWorker(t, observed)
	nha042ServerWait(t, "first Run maintenance cycle committed", func() bool { s := owner.Status(); return s.Cycles > 0 && !s.Active })
	if s := owner.Status(); s.LastError != nil {
		t.Fatalf("actual first cycle failed: %v", s.LastError)
	}
	lock, err := environment.observer.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(context.Background())
	if _, err = lock.Exec(context.Background(), `SELECT pg_advisory_xact_lock(481316,32)`); err != nil {
		t.Fatal(err)
	}
	before := owner.Status().Cycles
	clock.advance()
	nha042ServerWait(t, "real Run maintenance waits for PostgreSQL lock", func() bool {
		var count int
		err := environment.driverObserver.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE '%pg_advisory_xact_lock(481316,32)%'`).Scan(&count)
		if err != nil {
			t.Error(err)
			return false
		}
		return count > 0 && owner.Status().Active
	})
	started := time.Now()
	rig.pilot.emit(t, "nha042_start")
	nha042ServerWait(t, "routine terminal under maintenance SQL lock", func() bool {
		traces, _ := rig.composition.Traces()
		for _, trace := range traces {
			if trace.Automation == "nha042_server" && trace.RunID != 0 && trace.Decision == automationruns.DecisionFinished {
				return true
			}
		}
		return false
	})
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("volatile routine blocked for %s", elapsed)
	}
	if effects := rig.driver.snapshot(); len(effects) != 0 {
		t.Fatalf("pure routine emitted %d simulated effects", len(effects))
	}
	if err = lock.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	nha042ServerWait(t, "maintenance resumes through ordinary scheduler", func() bool { s := owner.Status(); return s.Cycles > before && !s.Active })
	clock.advance()
	nha042ServerWait(t, "second normal scheduled maintenance cycle", func() bool { s := owner.Status(); return s.Cycles > before+1 && !s.Active })
	if clock.created.Load() != 1 {
		t.Fatalf("created %d schedulers", clock.created.Load())
	}
	rig.run.cancel()
	if err = rig.run.waitDone(t); err != nil {
		t.Fatal(err)
	}
	nha034WaitSignal(t, owner.Done(), "Run maintenance Done before Store closure")
	nha034WaitRunStoreClosed(t, rig.store)
}

func TestNHA032RunRetainsStoreUntilRealMaintenanceDone(t *testing.T) {
	environment := nha042ServerEnvironmentFor(t)
	clock := nha032Clock()
	defer clock.releaseStop()
	observed := make(chan *recordermaintenance.Worker, 1)
	observedRuntime := make(chan *observationRuntime, 1)
	dependencies := runDependencies{maintenanceClock: clock,
		observeRecorderMaintenance: func(w *recordermaintenance.Worker) { observed <- w },
		observeRuntime:             func(runtime *observationRuntime) { observedRuntime <- runtime }}
	rig := nha042StartServer(t, environment, "- stop: done\n", "", nha042ServerOptions{dependencies: &dependencies})
	rig.run.waitReady(t)
	owner := nha032ObservedWorker(t, observed)
	var runtime *observationRuntime
	select {
	case runtime = <-observedRuntime:
	case <-time.After(5 * time.Second):
		t.Fatal("Run lost its NHA056 observation owner")
	}
	if runtime == nil || runtime.store != rig.store {
		t.Fatal("observation and maintenance did not borrow the same Run Store")
	}
	nha042ServerWait(t, "Run worker enters regular scheduler", func() bool { s := owner.Status(); return s.Cycles > 0 && !s.Active })
	rig.run.cancel()
	nha034WaitSignal(t, clock.stopped, "ordinary maintenance ticker Stop")
	// The injected ticker simulates a delayed scheduler destructor. The owner
	// is the real Run worker, and its actual Done has not closed yet.
	if err := rig.run.waitDone(t); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shared six-second close budget %v", err)
	}
	select {
	case <-owner.Done():
		t.Fatal("Done closed before scheduler really stopped")
	default:
	}
	if err := nha034ProbeRunStore(rig.store); err != nil {
		t.Fatalf("Store closed beneath live maintenance owner: %v", err)
	}
	if snapshot, err := runtime.healthSnapshot(); err != nil || snapshot.Health.Server.Readiness != obs.ServerReadiness_SERVER_READINESS_DRAINING {
		t.Fatalf("maintenance drain lost NHA056 Draining publication: %v", err)
	}
	clock.releaseStop()
	nha034WaitSignal(t, owner.Done(), "real maintenance owner completion")
	nha034WaitRunStoreClosed(t, rig.store)
}

func TestNHA032RunStartFailureJoinsPreparedMaintenanceOwner(t *testing.T) {
	environment := nha042ServerEnvironmentFor(t)
	clock := nha032Clock()
	clock.releaseStop()
	observed := make(chan *recordermaintenance.Worker, 1)
	dependencies := runDependencies{maintenanceClock: clock, observeRecorderMaintenance: func(w *recordermaintenance.Worker) { w.BeginClose(); observed <- w }}
	rig := nha042StartServer(t, environment, "- stop: done\n", "", nha042ServerOptions{dependencies: &dependencies})
	owner := nha032ObservedWorker(t, observed)
	if err := rig.run.waitDone(t); !errors.Is(err, recordermaintenance.ErrInvalid) {
		t.Fatalf("closed owner start accepted %v", err)
	}
	select {
	case <-owner.Done():
	default:
		t.Fatal("prepared owner not joined")
	}
	if clock.created.Load() != 0 {
		t.Fatal("failed Start created a scheduler")
	}
	nha034WaitRunStoreClosed(t, rig.store)
}

func TestNHA032RunPreparationRejectsTypedNilMaintenanceClock(t *testing.T) {
	environment := nha042ServerEnvironmentFor(t)
	var clock *nha032RunClock
	var observations atomic.Int32
	dependencies := runDependencies{maintenanceClock: clock, observeRecorderMaintenance: func(*recordermaintenance.Worker) { observations.Add(1) }}
	rig := nha042StartServer(t, environment, "- stop: done\n", "", nha042ServerOptions{dependencies: &dependencies})
	if err := rig.run.waitDone(t); !errors.Is(err, recordermaintenance.ErrInvalid) {
		t.Fatalf("typed nil clock admitted %v", err)
	}
	if observations.Load() != 0 {
		t.Fatal("invalid preparation exposed an owner")
	}
	nha034WaitRunStoreClosed(t, rig.store)
}
