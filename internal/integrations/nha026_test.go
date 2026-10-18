package integrations_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

type nha026TimerRequest struct {
	delay time.Duration
	fire  chan time.Time
}
type nha026Timer struct{ waits chan nha026TimerRequest }

func (timer *nha026Timer) After(delay time.Duration) <-chan time.Time {
	fire := make(chan time.Time, 1)
	timer.waits <- nha026TimerRequest{delay, fire}
	return fire
}

type nha026Jitter struct{}

func (nha026Jitter) Offset(time.Duration) time.Duration { return 0 }

func nha026Await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("NHA-026 barrier timed out")
		var zero T
		return zero
	}
}

func nha026Done(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("NHA-026 task did not join")
	}
}

type nha026Pilot struct {
	host     *integrations.Manager
	driver   integrations.CommandDriver
	started  chan struct{}
	session  *integrations.Session
	binding  integrations.Binding
	task     func(context.Context, integrations.Tasks) error
	startErr error
	stops    atomic.Int32
}

func (p *nha026Pilot) Start(ctx context.Context, sessionHost integrations.SessionHost, tasks integrations.Tasks) error {
	session := sessionHost.(*integrations.Session)
	driver, err := fake.NewActionDriver(time.Now())
	if err != nil {
		return err
	}
	if err := driver.Start(ctx, sessionHost); err != nil {
		return err
	}
	p.binding, err = driver.Binding()
	if err != nil {
		return err
	}
	p.session = session
	if p.driver != nil {
		if err := p.host.AttachCommandDriver(session, p.driver); err != nil {
			return err
		}
	}
	close(p.started)
	if p.task != nil {
		if err := p.task(ctx, tasks); err != nil {
			return err
		}
	}
	return p.startErr
}
func (p *nha026Pilot) Stop(context.Context) error { p.stops.Add(1); return nil }

type nha026Rig struct {
	host      *integrations.Manager
	fixture   *fake.Fixture
	instances [2]domain.IntegrationInstanceID
	entities  [2]domain.EntityID
	request   *actions.CommandRequest
	service   *integrations.VolatileCommandService
}

func nha026Setup(t *testing.T) nha026Rig {
	rig := nha026SetupBare(t)
	var err error
	rig.service, err = integrations.NewVolatileCommandService(rig.host, &nha022Generation{revision: 7}, integrations.DefaultVolatileCommandLimits())
	if err != nil {
		t.Fatal(err)
	}
	return rig
}

func nha026SetupBare(t *testing.T) nha026Rig {
	t.Helper()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("fixture-controller", "relay")
	if err != nil {
		t.Fatal(err)
	}
	var rig nha026Rig
	rig.host, rig.fixture = host, fixture
	var records []integrations.RegistryRecord
	for i, name := range []string{"nha026_crashing", "nha026_other"} {
		instance := nha016Instance(t, name)
		ids, err := fixture.Resolve(context.Background(), instance, []integrations.SourceKey{key})
		if err != nil {
			t.Fatal(err)
		}
		rig.instances[i], rig.entities[i] = instance, ids[0].Entity
		records = append(records, nha022Record(t, ids[0], instance, key, "switch."+name+"_old", "switch."+name, false))
	}
	if err := fixture.PublishRegistry(context.Background(), records); err != nil {
		t.Fatal(err)
	}
	cursor, err := fixture.Sequencer().Start()
	if err != nil {
		t.Fatal(err)
	}
	epoch, _, err := cursor.Components()
	if err != nil {
		t.Fatal(err)
	}
	rig.request = nha022Request(t, rig.entities[0], epoch, contracts.SwitchSchema, "relay", nha022Switch())
	return rig
}

func nha026Supervisor(t *testing.T, host *integrations.Manager, specs []integrations.PilotSpec, restarts int, timer *nha026Timer) *integrations.Supervisor {
	t.Helper()
	policy, err := integrations.NewRetryPolicy(restarts, 20*time.Millisecond, time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := integrations.NewSupervisor(host, specs, policy, timer, nha026Jitter{})
	if err != nil {
		t.Fatal(err)
	}
	return supervisor
}
func nha026Spec(t *testing.T, id domain.IntegrationInstanceID, factory integrations.PilotFactory) integrations.PilotSpec {
	t.Helper()
	spec, err := integrations.NewPilotSpec(id, factory)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestNHA026CrashDuringFakeCommandFencesLateReplyAndKeepsOtherPilot(t *testing.T) {
	rig := nha026Setup(t)
	timer := &nha026Timer{waits: make(chan nha026TimerRequest, 2)}
	entered, release, trigger := make(chan struct{}), make(chan struct{}), make(chan struct{})
	panicDriver := &nha022Driver{onDispatch: func() error { close(entered); <-release; panic("fake command crash") }}
	first := &nha026Pilot{host: rig.host, driver: panicDriver, started: make(chan struct{})}
	first.task = func(ctx context.Context, tasks integrations.Tasks) error {
		return tasks.Go(func(ctx context.Context) error {
			select {
			case <-trigger:
			case <-ctx.Done():
				return ctx.Err()
			}
			_, _ = rig.service.Dispatch(ctx, nha022Actor(t), rig.request)
			return nil
		})
	}
	second := &nha026Pilot{host: rig.host, driver: &nha022Driver{}, started: make(chan struct{})}
	other := &nha026Pilot{host: rig.host, started: make(chan struct{})}
	factoryQueue := make(chan integrations.Pilot, 2)
	factoryQueue <- first
	factoryQueue <- second
	supervisor := nha026Supervisor(t, rig.host, []integrations.PilotSpec{
		nha026Spec(t, rig.instances[0], func() integrations.Pilot { return <-factoryQueue }),
		nha026Spec(t, rig.instances[1], func() integrations.Pilot { return other }),
	}, 1, timer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	nha026Done(t, first.started)
	nha026Done(t, other.started)
	close(trigger)
	nha026Done(t, entered)
	close(release)
	wait := nha026Await(t, timer.waits)
	if wait.delay < 10*time.Millisecond || wait.delay > 20*time.Millisecond {
		t.Fatalf("jitter bound: %v", wait.delay)
	}
	select {
	case <-first.session.Done():
	default:
		t.Fatal("crashed session lease survived")
	}
	state, ok := rig.fixture.State(rig.entities[0])
	if !ok {
		t.Fatal("missing unavailable projection")
	}
	_, quality, _, _, _, err := state.Components()
	if err != nil {
		t.Fatal(err)
	}
	availability, _, _, _ := quality.Components()
	if availability != domain.AvailabilityUnavailable {
		t.Fatalf("crashed state: %v", availability)
	}
	if _, usable := state.CriticalValue(); usable {
		t.Fatal("crashed value remained critical")
	}
	health, ok := rig.fixture.Health(rig.instances[0])
	if !ok {
		t.Fatal("missing degraded health")
	}
	status, _, _ := health.Components()
	if status != integrations.HealthDegraded {
		t.Fatalf("health = %v", status)
	}
	observation := nha016Observation(t, time.Now())
	if err := first.session.Publish(context.Background(), first.binding, observation); !errors.Is(err, integrations.ErrInactiveSession) {
		t.Fatalf("late reply = %v", err)
	}
	if err := other.session.Publish(context.Background(), other.binding, observation); err != nil {
		t.Fatalf("other pilot lost: %v", err)
	}
	if response, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request); response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || err == nil || panicDriver.count() != 0 {
		t.Fatalf("uncertain command replayed: %v %v", response, err)
	}
	wait.fire <- time.Now()
	nha026Done(t, second.started)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := supervisor.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	if first.stops.Load() != 1 || second.stops.Load() != 1 || other.stops.Load() != 1 {
		t.Fatal("cooperative pilots not stopped exactly once")
	}
	for _, instance := range rig.instances {
		status, _ := supervisor.Status(instance)
		if status.Tasks != 0 {
			t.Fatalf("tasks survived: %+v", status)
		}
	}
	if rig.host.AdmissionsOpen() {
		t.Fatal("shutdown admitted commands")
	}
}

func TestNHA026RetryBudgetDoesNotResetAfterQuickStarts(t *testing.T) {
	rig := nha026Setup(t)
	timer := &nha026Timer{waits: make(chan nha026TimerRequest, 3)}
	type attempt struct{ started, registered, crash chan struct{} }
	attempts := make(chan attempt, 3)
	factory := func() integrations.Pilot {
		a := attempt{make(chan struct{}), make(chan struct{}), make(chan struct{})}
		p := &nha026Pilot{host: rig.host, started: a.started}
		p.task = func(ctx context.Context, tasks integrations.Tasks) error {
			err := tasks.Go(func(context.Context) error { <-a.crash; return errors.New("fake worker crash after successful Start") })
			close(a.registered)
			return err
		}
		attempts <- a
		return p
	}
	supervisor := nha026Supervisor(t, rig.host, []integrations.PilotSpec{nha026Spec(t, rig.instances[0], factory)}, 2, timer)
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		a := nha026Await(t, attempts)
		nha026Done(t, a.started)
		nha026Done(t, a.registered)
		nha026AwaitPhase(t, supervisor, rig.instances[0], integrations.PilotRunning)
		close(a.crash)
		if i < 2 {
			wait := nha026Await(t, timer.waits)
			wait.fire <- time.Now()
		}
	}
	nha026Done(t, supervisor.Done())
	status, ok := supervisor.Status(rig.instances[0])
	if !ok || status.Phase != integrations.PilotDegraded || status.Attempts != 3 || status.Tasks != 0 {
		t.Fatalf("retry budget: %+v", status)
	}
	select {
	case <-timer.waits:
		t.Fatal("infinite retry")
	default:
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}

func TestNHA026ParentCancellationClosesAdmissionsBeforeChildAndJoins(t *testing.T) {
	rig := nha026Setup(t)
	seen := make(chan bool, 1)
	pilot := &nha026Pilot{host: rig.host, started: make(chan struct{})}
	pilot.task = func(ctx context.Context, tasks integrations.Tasks) error {
		return tasks.Go(func(ctx context.Context) error { <-ctx.Done(); seen <- rig.host.AdmissionsOpen(); return nil })
	}
	supervisor := nha026Supervisor(t, rig.host, []integrations.PilotSpec{nha026Spec(t, rig.instances[0], func() integrations.Pilot { return pilot })}, 0, &nha026Timer{waits: make(chan nha026TimerRequest, 1)})
	ctx, cancel := context.WithCancel(context.Background())
	if err := supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	nha026Done(t, pilot.started)
	cancel()
	if nha026Await(t, seen) {
		t.Fatal("child cancellation preceded admission closure")
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := supervisor.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	if pilot.stops.Load() != 1 {
		t.Fatal("pilot Stop was skipped")
	}
	if status, _ := supervisor.Status(rig.instances[0]); status.Tasks != 0 {
		t.Fatalf("live tasks: %+v", status)
	}
	if err := supervisor.Stop(context.Background()); err != nil {
		t.Fatalf("idempotent Stop: %v", err)
	}
}

func TestNHA026TaskQuotaAndFactoryPanicAreContained(t *testing.T) {
	rig := nha026Setup(t)
	firstReady, release := make(chan struct{}), make(chan struct{})
	first := &nha026Pilot{host: rig.host, started: make(chan struct{})}
	first.task = func(ctx context.Context, tasks integrations.Tasks) error { close(firstReady); <-release; return nil }
	secondCalled := make(chan struct{}, 1)
	second := &nha026Pilot{host: rig.host, started: make(chan struct{})}
	policy, _ := integrations.NewRetryPolicy(0, time.Millisecond, time.Millisecond, time.Second)
	limits, _ := integrations.NewTaskLimits(1, 1)
	supervisor, err := integrations.NewSupervisorWithLimits(rig.host, []integrations.PilotSpec{
		nha026Spec(t, rig.instances[0], func() integrations.Pilot { return first }),
		nha026Spec(t, rig.instances[1], func() integrations.Pilot { <-firstReady; secondCalled <- struct{}{}; return second }),
	}, policy, limits, &nha026Timer{waits: make(chan nha026TimerRequest, 1)}, nha026Jitter{})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	nha026Await(t, secondCalled)
	// Wait for the actual rejected admission before releasing the occupied slot.
	quotaStatus := nha026AwaitPhase(t, supervisor, rig.instances[1], integrations.PilotDegraded)
	if quotaStatus.Attempts != 1 || quotaStatus.Tasks != 0 || second.stops.Load() != 1 {
		t.Fatalf("quota rejection did not drain: %+v", quotaStatus)
	}
	select {
	case <-second.started:
		t.Fatal("quota admitted second Start")
	default:
	}
	close(release)
	// A separate Host proves that a panicking factory cannot terminate the process.
	otherRig := nha026Setup(t)
	panicking := nha026Supervisor(t, otherRig.host, []integrations.PilotSpec{nha026Spec(t, otherRig.instances[0], func() integrations.Pilot { panic("factory fault") })}, 0, &nha026Timer{waits: make(chan nha026TimerRequest, 1)})
	if err := panicking.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	nha026Done(t, panicking.Done())
	if status, _ := panicking.Status(otherRig.instances[0]); status.Phase != integrations.PilotDegraded {
		t.Fatalf("factory panic: %+v", status)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := panicking.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}

func nha026AwaitPhase(t *testing.T, supervisor *integrations.Supervisor, instance domain.IntegrationInstanceID, phase integrations.PilotPhase) integrations.PilotStatus {
	t.Helper()
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		status, ok := supervisor.Status(instance)
		if ok && status.Phase == phase {
			return status
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatalf("phase %v never reached, last=%+v", phase, status)
		}
	}
}

func TestNHA026SpontaneousSessionReplacementTriggersRetryWithoutOldOwner(t *testing.T) {
	rig := nha026Setup(t)
	timer := &nha026Timer{waits: make(chan nha026TimerRequest, 2)}
	first := &nha026Pilot{host: rig.host, started: make(chan struct{})}
	second := &nha026Pilot{host: rig.host, started: make(chan struct{})}
	queue := make(chan integrations.Pilot, 2)
	queue <- first
	queue <- second
	supervisor := nha026Supervisor(t, rig.host, []integrations.PilotSpec{nha026Spec(t, rig.instances[0], func() integrations.Pilot { return <-queue })}, 1, timer)
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	nha026Done(t, first.started)
	if _, err := rig.host.OpenSession(rig.instances[0]); err != nil {
		t.Fatal(err)
	}
	wait := nha026Await(t, timer.waits)
	if err := first.session.Publish(context.Background(), first.binding, nha016Observation(t, time.Now())); !errors.Is(err, integrations.ErrInactiveSession) {
		t.Fatalf("replaced owner published: %v", err)
	}
	if health, ok := rig.fixture.Health(rig.instances[0]); !ok {
		t.Fatal("lost session health absent")
	} else if status, _, _ := health.Components(); status != integrations.HealthDegraded {
		t.Fatalf("lost session health=%v", status)
	}
	wait.fire <- time.Now()
	nha026Done(t, second.started)
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}

func TestNHA026TasksStatusChangesAfterStartAndJoinTimeoutNeverOverlaps(t *testing.T) {
	// A late registered task changes Status live without a second phase change.
	rig := nha026Setup(t)
	registry := make(chan integrations.Tasks, 1)
	pilot := &nha026Pilot{host: rig.host, started: make(chan struct{})}
	pilot.task = func(ctx context.Context, tasks integrations.Tasks) error { registry <- tasks; return nil }
	supervisor := nha026Supervisor(t, rig.host, []integrations.PilotSpec{nha026Spec(t, rig.instances[0], func() integrations.Pilot { return pilot })}, 0, &nha026Timer{waits: make(chan nha026TimerRequest, 2)})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	tasks := nha026Await(t, registry)
	nha026AwaitPhase(t, supervisor, rig.instances[0], integrations.PilotRunning)
	release, taskDone := make(chan struct{}), make(chan struct{})
	if err := tasks.Go(func(context.Context) error { defer close(taskDone); <-release; return nil }); err != nil {
		t.Fatal(err)
	}
	if status, _ := supervisor.Status(rig.instances[0]); status.Tasks != 1 {
		t.Fatalf("late task count=%+v", status)
	}
	close(release)
	nha026Done(t, taskDone)
	deadline := time.After(time.Second)
	for {
		status, _ := supervisor.Status(rig.instances[0])
		if status.Tasks == 0 {
			break
		}
		select {
		case <-time.After(time.Millisecond):
		case <-deadline:
			t.Fatalf("completed task retained: %+v", status)
		}
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}

	// A noncooperative task can miss the join bound. It is never relaunched
	// concurrently, and a later join updates Status and preserves the error.
	rig2 := nha026Setup(t)
	block, fail, registered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	stubborn := &nha026Pilot{host: rig2.host, started: make(chan struct{})}
	stubborn.task = func(ctx context.Context, tasks integrations.Tasks) error {
		if err := tasks.Go(func(context.Context) error { <-block; return nil }); err != nil {
			return err
		}
		if err := tasks.Go(func(context.Context) error { <-fail; return errors.New("fake crash") }); err != nil {
			return err
		}
		close(registered)
		return nil
	}
	factoryCalls := make(chan struct{}, 2)
	policy, _ := integrations.NewRetryPolicy(1, time.Millisecond, time.Millisecond, 30*time.Millisecond)
	timer := &nha026Timer{waits: make(chan nha026TimerRequest, 2)}
	supervisor2, err := integrations.NewSupervisor(rig2.host, []integrations.PilotSpec{nha026Spec(t, rig2.instances[0], func() integrations.Pilot { factoryCalls <- struct{}{}; return stubborn })}, policy, timer, nha026Jitter{})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	nha026Done(t, registered)
	close(fail)
	status := nha026AwaitPhase(t, supervisor2, rig2.instances[0], integrations.PilotDegraded)
	if status.Tasks == 0 {
		t.Fatal("unjoined task disappeared from Status")
	}
	select {
	case <-factoryCalls:
	default:
		t.Fatal("first attempt not constructed")
	}
	select {
	case <-factoryCalls:
		t.Fatal("overlapping retry after timeout")
	default:
	}
	select {
	case <-timer.waits:
		t.Fatal("retry scheduled while old task lives")
	default:
	}
	shortStop, shortCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	if err := supervisor2.Stop(shortStop); !errors.Is(err, integrations.ErrPilotJoinTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded Stop while worker lives: %v", err)
	}
	shortCancel()
	if status, _ := supervisor2.Status(rig2.instances[0]); status.Tasks == 0 {
		t.Fatal("blocked worker disappeared after Stop deadline")
	}
	close(block)
	nha026Done(t, supervisor2.Done())
	if status, _ := supervisor2.Status(rig2.instances[0]); status.Tasks != 0 || status.Attempts != 1 {
		t.Fatalf("late join status=%+v", status)
	}
	stopCtx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := supervisor2.Stop(stopCtx2); !errors.Is(err, integrations.ErrPilotJoinTimeout) {
		t.Fatalf("join failure lost: %v", err)
	}
}

func TestNHA026ZeroValuesTypedNilAndOneSupervisorPerHost(t *testing.T) {
	rig := nha026Setup(t)
	instance := rig.instances[0]
	if _, err := integrations.NewPilotSpec(domain.IntegrationInstanceID{}, func() integrations.Pilot { return nil }); !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("zero instance=%v", err)
	}
	if _, err := integrations.NewPilotSpec(instance, nil); !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("nil factory=%v", err)
	}
	if _, err := integrations.NewRetryPolicy(-1, time.Second, time.Second, time.Second); !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("invalid policy=%v", err)
	}
	if _, err := integrations.NewTaskLimits(0, 1); !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("invalid task limits=%v", err)
	}
	var nilTimer *nha026Timer
	if _, err := integrations.NewSupervisor(rig.host, nil, integrations.DefaultRetryPolicy(), nilTimer, nha026Jitter{}); !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("typed nil timer=%v", err)
	}
	first := nha026Supervisor(t, rig.host, nil, 0, &nha026Timer{waits: make(chan nha026TimerRequest, 1)})
	if _, err := integrations.NewSupervisor(rig.host, nil, integrations.DefaultRetryPolicy(), &nha026Timer{waits: make(chan nha026TimerRequest, 1)}, nha026Jitter{}); !errors.Is(err, integrations.ErrSupervisorState) {
		t.Fatalf("second supervisor=%v", err)
	}
	if err := first.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := first.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if first.Phase() != integrations.SupervisorStopped {
		t.Fatal("zero-pilot watcher did not join")
	}
}

func TestNHA026TaskRegistrationRacingStopIsRefused(t *testing.T) {
	rig := nha026Setup(t)
	registry := make(chan integrations.Tasks, 1)
	pilot := &nha026Pilot{host: rig.host, started: make(chan struct{})}
	pilot.task = func(ctx context.Context, tasks integrations.Tasks) error { registry <- tasks; return nil }
	supervisor := nha026Supervisor(t, rig.host, []integrations.PilotSpec{nha026Spec(t, rig.instances[0], func() integrations.Pilot { return pilot })}, 0, &nha026Timer{waits: make(chan nha026TimerRequest, 1)})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	tasks := nha026Await(t, registry)
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- supervisor.Stop(stopCtx) }()
	deadline := time.After(time.Second)
	for rig.host.AdmissionsOpen() {
		select {
		case <-time.After(time.Millisecond):
		case <-deadline:
			t.Fatal("admissions stayed open")
		}
	}
	ran := make(chan struct{})
	if err := tasks.Go(func(context.Context) error { close(ran); return nil }); !errors.Is(err, integrations.ErrSupervisorState) {
		t.Fatalf("late task admission=%v", err)
	}
	if err := nha026Await(t, done); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ran:
		t.Fatal("task ran after shutdown admission fence")
	default:
	}
}

type nha026StopErrorPilot struct {
	*nha026Pilot
	stopErr error
}

func (p *nha026StopErrorPilot) Stop(context.Context) error { p.stops.Add(1); return p.stopErr }

func TestNHA026StopErrorIsReturnedAndTypedNilFactoryDegrades(t *testing.T) {
	rig := nha026Setup(t)
	stopFailure := errors.New("fake Stop failed")
	pilot := &nha026StopErrorPilot{nha026Pilot: &nha026Pilot{host: rig.host, started: make(chan struct{})}, stopErr: stopFailure}
	supervisor := nha026Supervisor(t, rig.host, []integrations.PilotSpec{nha026Spec(t, rig.instances[0], func() integrations.Pilot { return pilot })}, 0, &nha026Timer{waits: make(chan nha026TimerRequest, 1)})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	nha026Done(t, pilot.started)
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.Stop(stopCtx); !errors.Is(err, stopFailure) {
		t.Fatalf("Stop error disappeared: %v", err)
	}
	if status, _ := supervisor.Status(rig.instances[0]); status.Tasks != 0 {
		t.Fatalf("Stop error leaked task: %+v", status)
	}

	rig2 := nha026Setup(t)
	typedNil := nha026Supervisor(t, rig2.host, []integrations.PilotSpec{nha026Spec(t, rig2.instances[0], func() integrations.Pilot { var missing *nha026Pilot; return missing })}, 0, &nha026Timer{waits: make(chan nha026TimerRequest, 1)})
	if err := typedNil.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	nha026Done(t, typedNil.Done())
	if status, _ := typedNil.Status(rig2.instances[0]); status.Phase != integrations.PilotDegraded {
		t.Fatalf("typed nil pilot phase: %+v", status)
	}
	if health, ok := rig2.fixture.Health(rig2.instances[0]); !ok {
		t.Fatal("typed nil pilot missing health")
	} else if status, _, _ := health.Components(); status != integrations.HealthDegraded {
		t.Fatalf("typed nil pilot health=%v", status)
	}
	if err := typedNil.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}

type nha026ExternalDriver struct {
	entered         chan struct{}
	release         chan struct{}
	ignoreCancel    bool
	panicAfterEntry bool
}

func (d *nha026ExternalDriver) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	close(d.entered)
	if d.panicAfterEntry {
		panic("external fake driver fault")
	}
	if d.ignoreCancel {
		<-d.release
	} else {
		select {
		case <-d.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return ctx.Err()
}

type nha026DispatchResult struct {
	response *actions.CommandResponse
	err      error
}

func TestNHA026ExternalDispatchPanicIsContainedAndTriggersDegraded(t *testing.T) {
	rig := nha026Setup(t)
	driver := &nha026ExternalDriver{entered: make(chan struct{}), panicAfterEntry: true}
	pilot := &nha026Pilot{host: rig.host, driver: driver, started: make(chan struct{})}
	supervisor := nha026Supervisor(t, rig.host, []integrations.PilotSpec{nha026Spec(t, rig.instances[0], func() integrations.Pilot { return pilot })}, 0, &nha026Timer{waits: make(chan nha026TimerRequest, 1)})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	nha026Done(t, pilot.started)
	response, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || !errors.Is(err, integrations.ErrSupervisedPilotPanic) {
		t.Fatalf("external panic escaped or lost uncertainty: %v %v", response, err)
	}
	nha026Done(t, driver.entered)
	nha026Done(t, supervisor.Done())
	if status, _ := supervisor.Status(rig.instances[0]); status.Phase != integrations.PilotDegraded || status.Tasks != 0 {
		t.Fatalf("external panic status=%+v", status)
	}
	if health, ok := rig.fixture.Health(rig.instances[0]); !ok {
		t.Fatal("health absent")
	} else if status, _, _ := health.Components(); status != integrations.HealthDegraded {
		t.Fatalf("health=%v", status)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}

func TestNHA026ExternalDispatchJoinsOnGlobalStopAndRejectsNextCommand(t *testing.T) {
	rig := nha026Setup(t)
	driver := &nha026ExternalDriver{entered: make(chan struct{}), release: make(chan struct{})}
	pilot := &nha026Pilot{host: rig.host, driver: driver, started: make(chan struct{})}
	supervisor := nha026Supervisor(t, rig.host, []integrations.PilotSpec{nha026Spec(t, rig.instances[0], func() integrations.Pilot { return pilot })}, 0, &nha026Timer{waits: make(chan nha026TimerRequest, 1)})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	nha026Done(t, pilot.started)
	result := make(chan nha026DispatchResult, 1)
	go func() {
		response, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request)
		result <- nha026DispatchResult{response, err}
	}()
	nha026Done(t, driver.entered)
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.Stop(stopCtx); err != nil {
		t.Fatalf("cooperative external command did not join: %v", err)
	}
	outcome := nha026Await(t, result)
	if outcome.response == nil || outcome.response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || outcome.err == nil {
		t.Fatalf("canceled in-flight effect claimed success: %+v", outcome)
	}
	if status, _ := supervisor.Status(rig.instances[0]); status.Tasks != 0 {
		t.Fatalf("external dispatch survived: %+v", status)
	}
	request := proto.Clone(rig.request).(*actions.CommandRequest)
	request.CommandId = "new-after-server-stop"
	if response, err := rig.service.Dispatch(context.Background(), nha022Actor(t), request); response != nil || !errors.Is(err, integrations.ErrAdmissionsClosed) {
		t.Fatalf("shutdown admission = %v %v", response, err)
	}
}

func TestNHA026ExternalNoncooperativeDispatchPreventsOverlappingRetry(t *testing.T) {
	rig := nha026Setup(t)
	driver := &nha026ExternalDriver{entered: make(chan struct{}), release: make(chan struct{}), ignoreCancel: true}
	pilot := &nha026Pilot{host: rig.host, driver: driver, started: make(chan struct{})}
	factoryCalls := make(chan struct{}, 2)
	policy, _ := integrations.NewRetryPolicy(1, time.Millisecond, time.Millisecond, 30*time.Millisecond)
	timer := &nha026Timer{waits: make(chan nha026TimerRequest, 2)}
	supervisor, err := integrations.NewSupervisor(rig.host, []integrations.PilotSpec{nha026Spec(t, rig.instances[0], func() integrations.Pilot { factoryCalls <- struct{}{}; return pilot })}, policy, timer, nha026Jitter{})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	nha026Done(t, pilot.started)
	result := make(chan nha026DispatchResult, 1)
	go func() {
		response, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request)
		result <- nha026DispatchResult{response, err}
	}()
	nha026Done(t, driver.entered)
	if err := rig.host.Revoke(pilot.session); err != nil {
		t.Fatal(err)
	}
	status := nha026AwaitPhase(t, supervisor, rig.instances[0], integrations.PilotDegraded)
	if status.Tasks == 0 {
		t.Fatal("in-flight external command was not counted")
	}
	select {
	case <-factoryCalls:
	default:
		t.Fatal("first attempt was not constructed")
	}
	select {
	case <-factoryCalls:
		t.Fatal("replacement overlapped live command")
	default:
	}
	select {
	case <-timer.waits:
		t.Fatal("retry was scheduled before old command joined")
	default:
	}
	shortStop, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	if err := supervisor.Stop(shortStop); !errors.Is(err, integrations.ErrPilotJoinTimeout) {
		t.Fatalf("Stop failed to report live command: %v", err)
	}
	cancel()
	close(driver.release)
	outcome := nha026Await(t, result)
	if outcome.response == nil || outcome.response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
		t.Fatalf("revoked command certainty: %+v", outcome)
	}
	nha026Done(t, supervisor.Done())
	if status, _ := supervisor.Status(rig.instances[0]); status.Tasks != 0 || status.Attempts != 1 {
		t.Fatalf("post-join status=%+v", status)
	}
	if err := supervisor.Stop(context.Background()); !errors.Is(err, integrations.ErrPilotJoinTimeout) {
		t.Fatalf("timeout lost after join: %v", err)
	}
}

func TestNHA026RawRouteFinalLeaseRefusesShutdownDuringAdmission(t *testing.T) {
	rig := nha026SetupBare(t)
	session, err := rig.host.OpenSession(rig.instances[0])
	if err != nil {
		t.Fatal(err)
	}
	fakeDriver, err := fake.NewActionDriver(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := fakeDriver.Start(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	driver := &nha022Driver{}
	if err := rig.host.AttachCommandDriver(session, driver); err != nil {
		t.Fatal(err)
	}
	paused := &nha022PausingGeneration{entered: make(chan struct{}), release: make(chan struct{})}
	router, err := integrations.NewCommandRouter(rig.host, paused)
	if err != nil {
		t.Fatal(err)
	}
	supervisor := nha026Supervisor(t, rig.host, nil, 0, &nha026Timer{waits: make(chan nha026TimerRequest, 1)})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	result := make(chan nha026DispatchResult, 1)
	go func() {
		response, err := router.Dispatch(context.Background(), nha022Actor(t), rig.request)
		result <- nha026DispatchResult{response, err}
	}()
	nha026Done(t, paused.entered)
	if err := supervisor.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(paused.release)
	outcome := nha026Await(t, result)
	if outcome.response != nil || !errors.Is(outcome.err, integrations.ErrAdmissionsClosed) || driver.count() != 0 {
		t.Fatalf("raw route crossed shutdown fence: %+v sends=%d", outcome, driver.count())
	}
}

func TestNHA026DurableDispatchRefusesShutdownBeforeIntentOrEffect(t *testing.T) {
	rig := nha026SetupBare(t)
	store, _, _ := nha024StoreCluster(t)
	service, err := integrations.NewDurableCommandService(context.Background(), rig.host, &nha022Generation{revision: 7}, store)
	if err != nil {
		t.Fatal(err)
	}
	supervisor := nha026Supervisor(t, rig.host, nil, 0, &nha026Timer{waits: make(chan nha026TimerRequest, 1)})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	response, err := service.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if response != nil || !errors.Is(err, integrations.ErrDurableLedgerUnavailable) {
		t.Fatalf("durable admission after shutdown: %v %v", response, err)
	}
	nha026Done(t, service.Done())
	id, err := domain.ParseCommandID(rig.request.CommandId)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := service.Status(context.Background(), id); err != nil || found {
		t.Fatalf("shutdown persisted a new intent: found=%t err=%v", found, err)
	}
}

type nha026AdvisoryBlock struct {
	release chan struct{}
	done    chan error
	once    sync.Once
}

func nha026HoldAdvisory(t *testing.T, store *storage.Store, key int32) *nha026AdvisoryBlock {
	t.Helper()
	block := &nha026AdvisoryBlock{release: make(chan struct{}), done: make(chan error, 1)}
	locked := make(chan struct{})
	go func() {
		block.done <- store.InTransaction(context.Background(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(context.Background(), `SELECT pg_advisory_xact_lock(9026, $1)`, key); err != nil {
				return err
			}
			close(locked)
			<-block.release
			return nil
		})
	}()
	nha026Done(t, locked)
	t.Cleanup(block.Release)
	return block
}

func (b *nha026AdvisoryBlock) Release() { b.once.Do(func() { close(b.release) }) }

func nha026WaitForCommandUpdateBlocked(t *testing.T, store *storage.Store) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		var waiting bool
		err := store.InTransaction(context.Background(), func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
				WHERE datname=current_database() AND wait_event_type='Lock' AND wait_event='advisory'
				AND query LIKE 'UPDATE public.command_intents SET state=%')`).Scan(&waiting)
		})
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-deadline:
			t.Fatal("command intention update did not reach advisory lock")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestNHA026DurableResultDrainKeepsPostgresUntilFinalWrite(t *testing.T) {
	store, _, _ := nha024StoreCluster(t)
	rig := nha026SetupBare(t)
	service, err := integrations.NewDurableCommandService(context.Background(), rig.host, &nha022Generation{revision: 7}, store)
	if err != nil {
		t.Fatal(err)
	}
	nha024Exec(t, store, `CREATE FUNCTION public.nha026_pause_result() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.state IN ('result', 'indeterminate') THEN PERFORM pg_advisory_xact_lock(9026, 1); END IF;
		RETURN NEW; END $$;
		CREATE TRIGGER nha026_pause_result BEFORE UPDATE ON public.command_intents
		FOR EACH ROW EXECUTE FUNCTION public.nha026_pause_result()`)
	block := nha026HoldAdvisory(t, store, 1)
	driver := &nha022Driver{}
	pilot := &nha026Pilot{host: rig.host, driver: driver, started: make(chan struct{})}
	supervisor := nha026Supervisor(t, rig.host, []integrations.PilotSpec{
		nha026Spec(t, rig.instances[0], func() integrations.Pilot { return pilot }),
	}, 0, &nha026Timer{waits: make(chan nha026TimerRequest, 1)})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	nha026Done(t, pilot.started)
	result := make(chan nha026DispatchResult, 1)
	go func() {
		response, err := service.Dispatch(context.Background(), nha022Actor(t), rig.request)
		result <- nha026DispatchResult{response, err}
	}()
	nha026WaitForCommandUpdateBlocked(t, store)
	if driver.count() != 1 {
		t.Fatalf("pilot has not returned before final SQL: %d", driver.count())
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := supervisor.Stop(stopCtx); !errors.Is(err, integrations.ErrPilotJoinTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded stop with SQL in flight = %v", err)
	}
	select {
	case <-supervisor.Done():
		t.Fatal("supervisor Done preceded final SQL")
	case <-service.Done():
		t.Fatal("durable guard released before final SQL")
	default:
	}
	secondDriver := &nha022Driver{}
	secondRig := nha023SetupBare(t, secondDriver)
	if _, err := integrations.NewDurableCommandService(context.Background(), secondRig.host, secondRig.gen, store); !errors.Is(err, integrations.ErrDurableRuntimeBusy) {
		t.Fatalf("runtime guard lost while SQL blocked: %v", err)
	}
	block.Release()
	if err := nha026Await(t, block.done); err != nil {
		t.Fatal(err)
	}
	response := nha026Await(t, result)
	if response.err != nil || response.response == nil || response.response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("recorded response = %+v", response)
	}
	if err := supervisor.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	nha026Done(t, service.Done())
	id, err := domain.ParseCommandID(rig.request.CommandId)
	if err != nil {
		t.Fatal(err)
	}
	status, found, err := service.Status(context.Background(), id)
	if err != nil || !found || status.State != storage.IntentResult {
		t.Fatalf("final durable result = %+v %t %v", status, found, err)
	}
	second, err := integrations.NewDurableCommandService(context.Background(), secondRig.host, secondRig.gen, store)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close(context.Background())
	if replay, err := second.Dispatch(context.Background(), nha022Actor(t), rig.request); err != nil || replay == nil || replay.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || secondDriver.count() != 0 {
		t.Fatalf("replay after drain = %v %v effects=%d", replay, err, secondDriver.count())
	}
}

func TestNHA026DurableConstructionReservationBlocksServing(t *testing.T) {
	store, _, _ := nha024StoreCluster(t)
	rig := nha026SetupBare(t)
	id, err := domain.ParseCommandID("nha026-recover-pending")
	if err != nil {
		t.Fatal(err)
	}
	actor := nha022Actor(t)
	if _, _, outcome, err := store.AcceptCommandIntent(context.Background(), id, actor, "seed-epoch", []byte{1}); outcome != storage.TransactionCommitted || err != nil {
		t.Fatalf("seed accept: %v %v", outcome, err)
	}
	if moved, outcome, err := store.MoveCommandIntent(context.Background(), id, storage.IntentAccepted, storage.IntentDispatching, nil, storage.IntentNoError); !moved || outcome != storage.TransactionCommitted || err != nil {
		t.Fatalf("seed dispatching: %t %v %v", moved, outcome, err)
	}
	nha024Exec(t, store, `CREATE FUNCTION public.nha026_pause_recovery() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.state = 'indeterminate' THEN PERFORM pg_advisory_xact_lock(9026, 2); END IF;
		RETURN NEW; END $$;
		CREATE TRIGGER nha026_pause_recovery BEFORE UPDATE ON public.command_intents
		FOR EACH ROW EXECUTE FUNCTION public.nha026_pause_recovery()`)
	block := nha026HoldAdvisory(t, store, 2)
	type constructed struct {
		service *integrations.DurableCommandService
		err     error
	}
	constructedResult := make(chan constructed, 1)
	go func() {
		service, err := integrations.NewDurableCommandService(context.Background(), rig.host, &nha022Generation{revision: 7}, store)
		constructedResult <- constructed{service, err}
	}()
	nha026WaitForCommandUpdateBlocked(t, store)
	supervisor := nha026Supervisor(t, rig.host, nil, 0, &nha026Timer{waits: make(chan nha026TimerRequest, 1)})
	if err := supervisor.Start(context.Background()); !errors.Is(err, integrations.ErrSupervisorState) {
		t.Fatalf("Start crossed constructing service: %v", err)
	}
	select {
	case <-supervisor.Done():
		t.Fatal("Done closed before admitted construction completed")
	default:
	}
	if _, err := integrations.NewDurableCommandService(context.Background(), rig.host, &nha022Generation{revision: 7}, store); !errors.Is(err, integrations.ErrDurableRuntimeBusy) {
		t.Fatalf("duplicate constructor during reservation: %v", err)
	}
	block.Release()
	if err := nha026Await(t, block.done); err != nil {
		t.Fatal(err)
	}
	ready := nha026Await(t, constructedResult)
	if ready.err != nil || ready.service == nil {
		t.Fatalf("reserved construction = %v", ready.err)
	}
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	nha026Done(t, ready.service.Done())
	status, found, err := ready.service.Status(context.Background(), id)
	if err != nil || !found || status.State != storage.IntentIndeterminate {
		t.Fatalf("recovered uncertain intent = %+v %t %v", status, found, err)
	}
}

func TestNHA026LateDurableConstructionRejectsBeforeRuntimeGuard(t *testing.T) {
	store, _, _ := nha024StoreCluster(t)
	rig := nha026SetupBare(t)
	supervisor := nha026Supervisor(t, rig.host, nil, 0, &nha026Timer{waits: make(chan nha026TimerRequest, 1)})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := integrations.NewDurableCommandService(context.Background(), rig.host, &nha022Generation{revision: 7}, store); !errors.Is(err, integrations.ErrSupervisorState) {
		t.Fatalf("late constructor = %v", err)
	}
	other := nha023SetupBare(t, &nha022Driver{})
	service, err := integrations.NewDurableCommandService(context.Background(), other.host, other.gen, store)
	if err != nil {
		t.Fatalf("late constructor leaked runtime guard: %v", err)
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}
