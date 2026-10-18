package energy

import (
	"context"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/testpg"
)

type nha108RollingLease struct {
	mu      sync.Mutex
	revoked bool
	calls   int
}

func (l *nha108RollingLease) Lease(_ context.Context, _ Profile, report ReportIdentity, reportTick, checkedTick time.Duration) (Lease, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.revoked {
		return Lease{}, false, nil
	}
	lease, err := NewLease(report, reportTick, checkedTick+3*time.Minute)
	return lease, err == nil, err
}
func (l *nha108RollingLease) revoke() {
	l.mu.Lock()
	l.revoked = true
	l.mu.Unlock()
}
func (l *nha108RollingLease) restore() {
	l.mu.Lock()
	l.revoked = false
	l.mu.Unlock()
}
func (l *nha108RollingLease) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

type nha108ManualPilotClock struct {
	source   *nha108Clock
	fire     chan time.Time
	requests chan time.Duration
}

func (c *nha108ManualPilotClock) NowTick() time.Duration { return c.source.MonoNow() }
func (c *nha108ManualPilotClock) After(delay time.Duration) <-chan time.Time {
	select {
	case c.requests <- delay:
	default:
	}
	return c.fire
}

type nha108PilotTasks struct {
	ctx  context.Context
	wg   sync.WaitGroup
	errs chan error
}

func (tasks *nha108PilotTasks) Go(task integrations.Task) error {
	tasks.wg.Add(1)
	go func() {
		defer tasks.wg.Done()
		if err := task(tasks.ctx); err != nil {
			select {
			case tasks.errs <- err:
			default:
			}
		}
	}()
	return nil
}

func nha108LeftOutput(t *testing.T, r *nha108Rig) (*integrations.Session, integrations.Binding, domain.EntityID, integrations.SourceKey) {
	t.Helper()
	instance, _ := domain.ParseIntegrationInstanceID("nha108-left-derived")
	session, err := r.host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := integrations.NewSourceKey("computed-energy", "left-counter")
	description := &wireregistry.CapabilityDescription{SchemaId: contracts.MeasurementSchema, Major: 1, DisplayName: "Estimated left energy", Endpoint: "energy", Constraints: &wireregistry.CapabilityDescription_Measurement{Measurement: &wireregistry.MeasurementConstraints{Unit: "kWh", Precision: 2, Semantic: wireregistry.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL_INCREASING, Aggregation: wireregistry.MeasurementAggregation_MEASUREMENT_AGGREGATION_SUM}}}
	candidate, err := integrations.NewCandidate(key, []*wireregistry.CapabilityDescription{description})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	_, output, err := bindings[0].IDs()
	if err != nil {
		t.Fatal(err)
	}
	return session, bindings[0], output, key
}

func nha108WaitTotal(t *testing.T, repository *Repository, profile Profile, want *big.Rat, wantCoverage Coverage) Checkpoint {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		checkpoint, err := repository.Load(context.Background(), profile)
		if err == nil && checkpoint.revision > 0 && checkpoint.coverage == wantCoverage {
			total, rationalErr := rational(checkpoint.totalNum, checkpoint.totalDen)
			if rationalErr == nil && total.Cmp(want) == 0 && !checkpoint.hasPending {
				return checkpoint
			}
		}
		select {
		case <-deadline:
			t.Fatalf("checkpoint total did not reach %s/%d: %+v %v", want, wantCoverage, checkpoint, err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestNHA108PilotRenewsHeldPowerAtTicksAndRevokesBeforeIntegration(t *testing.T) {
	cluster := testpg.Start(t)
	store := nha108Store(t, cluster.Port)
	defer store.Close()
	r := nha108NewRig(t, "W")
	outputSession, _, output, key := nha108LeftOutput(t, r)
	profile, err := HiloLeftV1(r.source, output, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := NewOutputBinding(profile, key)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	clock := &nha108ManualPilotClock{source: r.clock, fire: make(chan time.Time, 1), requests: make(chan time.Duration, 32)}
	leases := &nha108RollingLease{}
	factory, err := NewPilotFactory(r.sequence, repository, []OutputBinding{binding}, leases, clock)
	if err != nil {
		t.Fatal(err)
	}
	pilot := factory().(*Pilot)
	ctx, cancel := context.WithCancel(context.Background())
	tasks := &nha108PilotTasks{ctx: ctx, errs: make(chan error, 2)}
	if err := pilot.Start(ctx, outputSession, tasks); err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() {
		cancel()
		if err := pilot.Stop(context.Background()); err != nil {
			t.Error(err)
		}
		done := make(chan struct{})
		go func() { tasks.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("pilot tasks did not stop")
		}
		select {
		case err := <-tasks.errs:
			t.Errorf("pilot task: %v", err)
		default:
		}
	}()
	base := r.clock.Now()
	r.publish(t, 1000, &base, domain.AvailabilityAvailable, domain.ProvenanceObserved, 1)
	nha108WaitTotal(t, repository, profile, big.NewRat(0, 1), CoverageContinuous)
	for i := 1; i <= 3; i++ {
		select {
		case <-clock.requests:
		case <-time.After(3 * time.Second):
			t.Fatal("pilot did not schedule held-power tick")
		}
		at := base.Add(time.Duration(i) * 120 * time.Second)
		r.clock.set(at, time.Duration(i)*120*time.Second)
		clock.fire <- at
		nha108WaitTotal(t, repository, profile, big.NewRat(int64(i), 30), CoverageContinuous)
	}
	if leases.count() < 4 {
		t.Fatalf("lease source was not rechecked at every tick: %d", leases.count())
	}
	state, ok := r.fixture.State(r.source)
	if !ok {
		t.Fatal("source state missing")
	}
	_, _, times, _, _, err := state.Components()
	if err != nil || times.ObservedAt == nil || !times.ObservedAt.Equal(base) {
		t.Fatalf("tick refreshed source capture: %+v %v", times, err)
	}
	leases.revoke()
	at := base.Add(8 * time.Minute)
	r.clock.set(at, 8*time.Minute)
	clock.fire <- at
	nha108WaitTotal(t, repository, profile, big.NewRat(1, 10), CoverageDegraded)
}

func TestNHA108PilotReportAfterLeaseExpiryBeforeTimerCannotBridge(t *testing.T) {
	cluster := testpg.Start(t)
	store := nha108Store(t, cluster.Port)
	defer store.Close()
	r := nha108NewRig(t, "W")
	outputSession, outputBinding, output, _ := nha108LeftOutput(t, r)
	profile, err := HiloLeftV1(r.source, output, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	repository, _ := NewRepository(store)
	leases := &nha108RollingLease{}
	clock := &nha108ManualPilotClock{source: r.clock, fire: make(chan time.Time, 1), requests: make(chan time.Duration, 8)}
	pilot := &Pilot{sequence: r.sequence, repository: repository, leaseSource: leases, clock: clock, epoch: r.epoch}
	item := &counter{binding: outputBinding, state: newCheckpoint(profile)}
	base := r.clock.Now()
	first := r.publishNoSequence(t, 1000, nil, domain.AvailabilityAvailable, domain.ProvenanceObserved)
	if err := pilot.report(context.Background(), outputSession, item, first); err != nil || !item.hasLease {
		t.Fatalf("initial report: %+v %v", item, err)
	}
	firstRevision := item.state.lastReport.revision
	if delay, ok := pilot.nextTick(map[domain.EntityID]*counter{r.source: item}); !ok || delay != 120*time.Second {
		t.Fatalf("timer was not due at 120s: %s %t", delay, ok)
	}
	// Do not consume the timer. The event select may choose this report first.
	at := base.Add(4 * time.Minute)
	r.clock.set(at, 4*time.Minute)
	late := r.publishNoSequence(t, 1000, nil, domain.AvailabilityAvailable, domain.ProvenanceObserved)
	if err := pilot.report(context.Background(), outputSession, item, late); err != nil {
		t.Fatal(err)
	}
	if item.state.hasEnergy || item.state.coverage != CoverageDegraded || item.state.diagnostic != DiagnosticLeaseExpired || !item.state.hasPrevious {
		t.Fatalf("late report crossed expired lease: %+v", item.state)
	}
	if lateIdentity, err := NewReportIdentity(late); err != nil || lateIdentity.revision != firstRevision+1 || item.state.previous.report != lateIdentity {
		t.Fatalf("Host revision was not retained: %+v %v", lateIdentity, err)
	}
	at = base.Add(6 * time.Minute)
	r.clock.set(at, 6*time.Minute)
	current := r.publishNoSequence(t, 1000, nil, domain.AvailabilityAvailable, domain.ProvenanceObserved)
	if err := pilot.report(context.Background(), outputSession, item, current); err != nil {
		t.Fatal(err)
	}
	total, err := rational(item.state.totalNum, item.state.totalDen)
	if err != nil || total.Cmp(big.NewRat(1, 30)) != 0 {
		t.Fatalf("only post-gap interval should contribute: %v %v", total, err)
	}
	leases.revoke()
	at = base.Add(7 * time.Minute)
	r.clock.set(at, 7*time.Minute)
	revoked := r.publishNoSequence(t, 1000, nil, domain.AvailabilityAvailable, domain.ProvenanceObserved)
	if err := pilot.report(context.Background(), outputSession, item, revoked); err != nil {
		t.Fatal(err)
	}
	total, _ = rational(item.state.totalNum, item.state.totalDen)
	if total.Cmp(big.NewRat(1, 30)) != 0 || item.state.hasPrevious || item.state.diagnostic != DiagnosticLeaseExpired {
		t.Fatalf("revoked source contributed at report: %+v", item.state)
	}
}

func TestNHA108PilotWaitsForQueuedReportWithoutFalseGapOrTimerLoop(t *testing.T) {
	cluster := testpg.Start(t)
	store := nha108Store(t, cluster.Port)
	defer store.Close()
	r := nha108NewRig(t, "W")
	outputSession, outputBinding, output, _ := nha108LeftOutput(t, r)
	profile, err := HiloLeftV1(r.source, output, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	repository, _ := NewRepository(store)
	leases := &nha108RollingLease{}
	clock := &nha108ManualPilotClock{source: r.clock, fire: make(chan time.Time, 1), requests: make(chan time.Duration, 8)}
	pilot := &Pilot{sequence: r.sequence, repository: repository, leaseSource: leases, clock: clock, epoch: r.epoch}
	item := &counter{binding: outputBinding, state: newCheckpoint(profile)}
	base := r.clock.Now()
	first := r.publishNoSequence(t, 1000, nil, domain.AvailabilityAvailable, domain.ProvenanceObserved)
	if err := pilot.report(context.Background(), outputSession, item, first); err != nil {
		t.Fatal(err)
	}
	_, _, stamp, err := item.state.lastReport.Source()
	if err != nil {
		t.Fatal(err)
	}
	_, seq, err := stamp.Components()
	if err != nil || seq != nil {
		t.Fatalf("test source unexpectedly has protocol sequence: %v %v", seq, err)
	}
	r.clock.set(base.Add(119*time.Second), 119*time.Second)
	middle := r.publishNoSequence(t, 1000, nil, domain.AvailabilityAvailable, domain.ProvenanceObserved)
	r.clock.set(base.Add(120*time.Second), 120*time.Second)
	latest := r.publishNoSequence(t, 1000, nil, domain.AvailabilityAvailable, domain.ProvenanceObserved)
	before := item.state.revision
	if err := pilot.ticks(context.Background(), outputSession, map[domain.EntityID]*counter{r.source: item}); err != nil {
		t.Fatal(err)
	}
	if !item.awaitingReport || item.state.revision != before || leases.count() != 1 {
		t.Fatalf("timer mistook queued normal reports for revocation: %+v calls=%d", item, leases.count())
	}
	if _, ok := pilot.nextTick(map[domain.EntityID]*counter{r.source: item}); ok {
		t.Fatal("queued report left an active zero-delay timer loop")
	}
	item.awaitingReport = false // Recorder delivers the first queued entry.
	if err := pilot.report(context.Background(), outputSession, item, middle); err != nil {
		t.Fatal(err)
	}
	if !item.awaitingReport || item.state.coverage != CoverageContinuous || leases.count() != 1 {
		t.Fatalf("superseded report lost the old continuity proof: %+v calls=%d", item, leases.count())
	}
	item.awaitingReport = false // Recorder delivers the current entry.
	if err := pilot.report(context.Background(), outputSession, item, latest); err != nil {
		t.Fatal(err)
	}
	total, err := rational(item.state.totalNum, item.state.totalDen)
	if err != nil || total.Cmp(big.NewRat(1, 30)) != 0 || item.state.coverage != CoverageContinuous || item.awaitingReport || leases.count() != 2 {
		t.Fatalf("current report failed to complete one interval: total=%v state=%+v calls=%d err=%v", total, item.state, leases.count(), err)
	}
}

func TestNHA108PilotInitialLeaseRefusalLeavesNoReusableBound(t *testing.T) {
	cluster := testpg.Start(t)
	store := nha108Store(t, cluster.Port)
	defer store.Close()
	r := nha108NewRig(t, "W")
	outputSession, outputBinding, output, _ := nha108LeftOutput(t, r)
	profile, err := HiloLeftV1(r.source, output, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	repository, _ := NewRepository(store)
	leases := &nha108RollingLease{}
	leases.revoke()
	clock := &nha108ManualPilotClock{source: r.clock, fire: make(chan time.Time, 1), requests: make(chan time.Duration, 8)}
	pilot := &Pilot{sequence: r.sequence, repository: repository, leaseSource: leases, clock: clock, epoch: r.epoch}
	item := &counter{binding: outputBinding, state: newCheckpoint(profile)}
	base := r.clock.Now()
	first := r.publishNoSequence(t, 1000, nil, domain.AvailabilityAvailable, domain.ProvenanceObserved)
	if err := pilot.report(context.Background(), outputSession, item, first); err != nil {
		t.Fatal(err)
	}
	if item.state.revision != 0 || item.state.hasPrevious || item.hasLease {
		t.Fatalf("unproven initial power became reusable: %+v", item)
	}
	leases.restore()
	at := base.Add(2 * time.Minute)
	r.clock.set(at, 2*time.Minute)
	second := r.publishNoSequence(t, 1000, nil, domain.AvailabilityAvailable, domain.ProvenanceObserved)
	if err := pilot.report(context.Background(), outputSession, item, second); err != nil {
		t.Fatal(err)
	}
	if !item.state.hasPrevious || item.state.hasEnergy || !item.hasLease {
		t.Fatalf("first newly proven report integrated refusal gap: %+v", item)
	}
}
