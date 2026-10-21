package integrations

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/observability"
)

var (
	ErrSupervisorState  = errors.New("supervisor lifecycle state does not permit this operation")
	ErrPilotJoinTimeout = errors.New("supervised work did not join within the shutdown bound")
	ErrPilotTaskLimit   = errors.New("pilot task limit reached")
	ErrPilotFactory     = errors.New("pilot factory failed")
)

// Pilot owns only work registered with Tasks. Start may set up subscriptions and
// return; a registered task or session loss then determines its lifetime.
// Stop must cooperate with its context. Go cannot kill an arbitrary goroutine.
type Pilot interface {
	Start(context.Context, SessionHost, Tasks) error
	Stop(context.Context) error
}

// A supervised pilot may expose its own session-bound command driver. The
// supervisor attaches that exact instance before starting any pilot work.
type SupervisedCommandPilot interface{ CommandDriver() CommandDriver }

type Task func(context.Context) error

type Tasks interface{ Go(Task) error }

// pilotTaskView is the only task capability handed to a pilot. taskGroup also
// owns internal admission, failure, and closure methods that must stay private
// to the supervisor and command router.
type pilotTaskView struct{ group *taskGroup }

func (v pilotTaskView) Go(task Task) error {
	if v.group == nil {
		return ErrInvalidInput
	}
	return v.group.Go(task)
}

type PilotFactory func() Pilot

type PilotSpec struct {
	instance domain.IntegrationInstanceID
	factory  PilotFactory
}

func NewPilotSpec(instance domain.IntegrationInstanceID, factory PilotFactory) (PilotSpec, error) {
	if _, err := instance.Value(); err != nil || factory == nil {
		return PilotSpec{}, ErrInvalidInput
	}
	return PilotSpec{instance, factory}, nil
}

// Instance exposes the admitted identity without exposing or invoking the
// trusted pilot factory during server composition.
func (p PilotSpec) Instance() (domain.IntegrationInstanceID, error) {
	if _, err := p.instance.Value(); err != nil || p.factory == nil {
		return domain.IntegrationInstanceID{}, ErrInvalidInput
	}
	return p.instance, nil
}

type RetryPolicy struct {
	maxRestarts                int
	base, maximum, joinTimeout time.Duration
}

func NewRetryPolicy(maxRestarts int, base, maximum, joinTimeout time.Duration) (RetryPolicy, error) {
	if maxRestarts < 0 || maxRestarts > 16 || base <= 0 || maximum < base || joinTimeout <= 0 {
		return RetryPolicy{}, ErrInvalidInput
	}
	return RetryPolicy{maxRestarts, base, maximum, joinTimeout}, nil
}

func DefaultRetryPolicy() RetryPolicy {
	p, _ := NewRetryPolicy(3, 250*time.Millisecond, 10*time.Second, 5*time.Second)
	return p
}

// Timer and Jitter are injectable so tests can open retry barriers without
// sleeping. Jitter returns an offset in [0, limit].
type Timer interface {
	After(time.Duration) <-chan time.Time
}
type Jitter interface {
	Offset(time.Duration) time.Duration
}
type RealTimer struct{}

func (RealTimer) After(d time.Duration) <-chan time.Time { return time.After(d) }

type RandomJitter struct{}

func (RandomJitter) Offset(limit time.Duration) time.Duration {
	if limit <= 0 {
		return 0
	}
	if limit == time.Duration(1<<63-1) {
		return time.Duration(rand.Uint64() >> 1)
	}
	return time.Duration(rand.Int64N(int64(limit) + 1))
}

type PilotPhase uint8

const (
	PilotUnspecified PilotPhase = iota
	PilotStarting
	PilotRunning
	PilotRetrying
	PilotDegraded
	PilotStopped
)

type SupervisorPhase uint8

const (
	SupervisorUnspecified SupervisorPhase = iota
	SupervisorReady
	SupervisorRunning
	SupervisorStopping
	SupervisorStopped
)

type PilotStatus struct {
	Phase    PilotPhase
	Attempts int
	Tasks    int
}

type pilotRun struct {
	spec     PilotSpec
	phase    PilotPhase
	attempts int
	tasks    int
}

type TaskLimits struct{ perPilot, global int }

func NewTaskLimits(perPilot, global int) (TaskLimits, error) {
	if perPilot < 1 || perPilot > 256 || global < perPilot || global > 4096 {
		return TaskLimits{}, ErrInvalidInput
	}
	return TaskLimits{perPilot, global}, nil
}

func DefaultTaskLimits() TaskLimits {
	l, _ := NewTaskLimits(64, 4096)
	return l
}

type Supervisor struct {
	journal     *observability.Journal
	mu          sync.Mutex
	host        *Manager
	policy      RetryPolicy
	limits      TaskLimits
	timer       Timer
	jitter      Jitter
	phase       SupervisorPhase
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	watchStop   chan struct{}
	watchDone   chan struct{}
	durable     *DurableCommandService
	drainDone   chan struct{}
	pilots      map[domain.IntegrationInstanceID]*pilotRun
	tasksActive int
	failures    []error
}

func NewSupervisor(host *Manager, specs []PilotSpec, policy RetryPolicy, timer Timer, jitter Jitter) (*Supervisor, error) {
	return NewSupervisorWithLimits(host, specs, policy, DefaultTaskLimits(), timer, jitter)
}

func NewSupervisorWithLimits(host *Manager, specs []PilotSpec, policy RetryPolicy, limits TaskLimits, timer Timer, jitter Jitter) (*Supervisor, error) {
	if host == nil || host.active == nil || policy.base <= 0 || policy.maximum < policy.base || policy.joinTimeout <= 0 || policy.maxRestarts < 0 || policy.maxRestarts > 16 || limits.perPilot < 1 || limits.perPilot > 256 || limits.global < limits.perPilot || limits.global > 4096 || nilInterface(timer) || nilInterface(jitter) || len(specs) > 1024 {
		return nil, ErrInvalidInput
	}
	runs := make(map[domain.IntegrationInstanceID]*pilotRun, len(specs))
	for _, spec := range specs {
		if _, err := spec.instance.Value(); err != nil || spec.factory == nil {
			return nil, ErrInvalidInput
		}
		if _, exists := runs[spec.instance]; exists {
			return nil, ErrInvalidInput
		}
		runs[spec.instance] = &pilotRun{spec: spec, phase: PilotStopped}
	}
	s := &Supervisor{host: host, policy: policy, limits: limits, timer: timer, jitter: jitter, phase: SupervisorReady, pilots: runs, done: make(chan struct{}), watchStop: make(chan struct{}), watchDone: make(chan struct{})}
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.supervisor != nil || host.admission != admissionOpen {
		return nil, ErrSupervisorState
	}
	s.journal = host.journal
	host.supervisor = s
	return s, nil
}

func (s *Supervisor) Done() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.done
}

func (s *Supervisor) Phase() SupervisorPhase {
	if s == nil {
		return SupervisorUnspecified
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase
}

func (s *Supervisor) Status(instance domain.IntegrationInstanceID) (PilotStatus, bool) {
	if s == nil {
		return PilotStatus{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.pilots[instance]
	if !ok {
		return PilotStatus{}, false
	}
	return PilotStatus{run.phase, run.attempts, run.tasks}, true
}

func (s *Supervisor) set(run *pilotRun, phase PilotPhase) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if run.phase != phase {
		code := obs.LogCode_LOG_CODE_UNSPECIFIED
		switch phase {
		case PilotStarting:
			code = obs.LogCode_LOG_CODE_PILOT_STARTING
		case PilotRunning:
			code = obs.LogCode_LOG_CODE_PILOT_RUNNING
		case PilotRetrying:
			code = obs.LogCode_LOG_CODE_PILOT_RETRYING
		case PilotDegraded:
			code = obs.LogCode_LOG_CODE_PILOT_DEGRADED
		case PilotStopped:
			code = obs.LogCode_LOG_CODE_PILOT_STOPPED
		}
		if code != 0 {
			level := obs.LogLevel_LOG_LEVEL_INFO
			if phase == PilotRetrying || phase == PilotDegraded {
				level = obs.LogLevel_LOG_LEVEL_WARNING
			}
			s.emitObservation(code, level)
		}
	}
	run.phase = phase
}

func (s *Supervisor) Start(ctx context.Context) error {
	if s == nil || ctx == nil {
		return ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.phase != SupervisorReady {
		s.mu.Unlock()
		return ErrSupervisorState
	}
	durable, err := s.host.startServing(s)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.durable = durable
	if durable != nil {
		s.drainDone = make(chan struct{})
	}
	s.ctx, s.cancel = context.WithCancel(context.WithoutCancel(ctx))
	s.phase = SupervisorRunning
	go func() {
		defer close(s.watchDone)
		select {
		case <-ctx.Done():
			s.beginStop()
		case <-s.watchStop:
		}
	}()
	var wg sync.WaitGroup
	for _, run := range s.pilots {
		wg.Add(1)
		go func(r *pilotRun) { defer wg.Done(); s.run(r) }(run)
	}
	go func() {
		wg.Wait()
		if s.drainDone != nil {
			<-s.drainDone
		}
		close(s.done)
	}()
	s.mu.Unlock()
	return nil
}

// Stop first closes the real Host command admissions, then cancels child
// contexts. A deadline error leaves the supervisor stopping and never starts
// another attempt over a worker that may still be alive.
func (s *Supervisor) Stop(ctx context.Context) error {
	if s == nil || ctx == nil {
		return ErrInvalidInput
	}
	if err := s.beginStop(); err != nil {
		return err
	}
	select {
	case <-s.done:
		<-s.watchDone
		s.mu.Lock()
		s.phase = SupervisorStopped
		err := errors.Join(s.failures...)
		s.mu.Unlock()
		return err
	default:
	}
	select {
	case <-s.done:
		<-s.watchDone
		s.mu.Lock()
		s.phase = SupervisorStopped
		err := errors.Join(s.failures...)
		s.mu.Unlock()
		return err
	case <-ctx.Done():
		return errors.Join(ErrPilotJoinTimeout, ctx.Err())
	}
}

func (s *Supervisor) beginStop() error {
	s.mu.Lock()
	if s.phase == SupervisorReady || s.phase == SupervisorUnspecified {
		s.mu.Unlock()
		return ErrSupervisorState
	}
	if s.phase == SupervisorRunning {
		s.phase = SupervisorStopping
		if err := s.host.CloseAdmissions(); err != nil {
			s.failures = append(s.failures, err)
		}
		close(s.watchStop)
		s.cancel()
		durable := s.durable
		s.mu.Unlock()
		if durable != nil {
			go s.drainDurable(durable)
		}
		return nil
	}
	s.mu.Unlock()
	return nil
}

func (s *Supervisor) drainDurable(service *DurableCommandService) {
	// Close rejects new durable admissions, then waits for the whole dispatch,
	// including SQL result persistence. A timed-out close retains its guard
	// until dispatch ends. Done marks the end of the release attempt; Close
	// reports whether PostgreSQL actually confirmed the release.
	ctx, cancel := context.WithTimeout(context.Background(), s.policy.joinTimeout)
	err := service.Close(ctx)
	requestErr := ctx.Err()
	cancel()
	// The runtime's own bounded SQL query may wrap context.DeadlineExceeded.
	// Only the request context expiring means this drain call timed out.
	timedOut := requestErr != nil && err == requestErr
	if timedOut {
		s.record(errors.Join(ErrPilotJoinTimeout, err))
	} else if err != nil {
		s.record(err)
	}
	<-service.Done()
	if timedOut {
		if releaseErr := service.Close(context.Background()); releaseErr != nil {
			s.record(releaseErr)
		}
	}
	close(s.drainDone)
}

func (s *Supervisor) record(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.failures = append(s.failures, err)
	s.mu.Unlock()
}

func (s *Supervisor) run(run *pilotRun) {
	for attempt := 0; ; attempt++ {
		if s.ctx.Err() != nil {
			s.set(run, PilotStopped)
			return
		}
		s.mu.Lock()
		run.attempts++
		run.phase = PilotStarting
		s.emitObservation(obs.LogCode_LOG_CODE_PILOT_STARTING, obs.LogLevel_LOG_LEVEL_INFO)
		s.mu.Unlock()
		pilot, factoryErr := constructPilot(run.spec.factory)
		if factoryErr == nil && s.journal != nil {
			factoryErr = composePilotObservation(pilot, s.host.observation, s.journal)
		}
		if factoryErr != nil {
			s.emitObservation(obs.LogCode_LOG_CODE_PILOT_FACTORY_FAILED, obs.LogLevel_LOG_LEVEL_ERROR)
			if err := s.host.ReportPilotDegraded(run.spec.instance); err != nil {
				s.record(err)
			}
			s.set(run, PilotDegraded)
			return
		}
		session, err := s.host.OpenSession(run.spec.instance)
		if err != nil {
			if s.ctx.Err() == nil {
				s.record(err)
			}
			s.set(run, PilotDegraded)
			return
		}
		child, cancel := context.WithCancel(s.ctx)
		group := newTaskGroup(child, s, run)
		if err := s.host.manageSession(session, group, cancel); err != nil {
			cancel()
			if !errors.Is(err, ErrInactiveSession) {
				s.record(err)
			}
			s.set(run, PilotDegraded)
			return
		}
		if driver, driverErr := supervisedCommandDriver(pilot); driverErr != nil || driver != nil {
			if driverErr == nil {
				driverErr = s.host.AttachCommandDriver(session, driver)
			}
			if driverErr != nil {
				cancel()
				_ = s.host.RevokeWithHealth(session, HealthDegraded, "command driver attachment failed")
				s.record(driverErr)
				s.set(run, PilotDegraded)
				return
			}
		}
		started := make(chan error, 1)
		startErr := group.Go(func(ctx context.Context) error {
			err := pilot.Start(ctx, session, pilotTaskView{group: group})
			started <- err
			return err
		})
		failed := startErr != nil
		if !failed {
			select {
			case err := <-started:
				failed = err != nil
				if !failed {
					select {
					case <-group.fail:
						failed = true
					default:
						s.set(run, PilotRunning)
					}
				}
			case <-group.fail:
				failed = true
			case <-session.Done():
				failed = true
			case <-s.ctx.Done():
			}
		}
		if !failed && s.ctx.Err() == nil {
			select {
			case <-group.fail:
				failed = true
			case <-session.Done():
				failed = true
			case <-s.ctx.Done():
			}
		}
		publicationFailed := false
		if failed && s.ctx.Err() == nil {
			revokeErr := s.host.RevokeWithHealth(session, HealthDegraded, "pilot session lost")
			if errors.Is(revokeErr, ErrInactiveSession) {
				revokeErr = session.RetirementError()
			}
			if revokeErr != nil {
				publicationFailed = true
				s.record(revokeErr)
			}
		} else if s.ctx.Err() != nil {
			if err := s.host.RevokeWithHealth(session, HealthUnavailable, "server stopping"); err != nil && !errors.Is(err, ErrInactiveSession) {
				s.record(err)
			}
		}
		cancel()
		group.Close()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), s.policy.joinTimeout)
		stopped := make(chan error, 1)
		s.mu.Lock()
		run.tasks++
		s.mu.Unlock()
		go func() {
			var stopErr error
			defer func() {
				if recover() != nil {
					stopErr = ErrSupervisorState
				}
				s.mu.Lock()
				run.tasks--
				s.mu.Unlock()
				stopped <- stopErr
			}()
			stopErr = pilot.Stop(stopCtx)
		}()
		joined := make(chan struct{})
		var stopErr error
		go func() { <-group.Done(); stopErr = <-stopped; close(joined) }()
		select {
		case <-joined:
		case <-time.After(s.policy.joinTimeout):
			s.set(run, PilotDegraded)
			s.record(ErrPilotJoinTimeout)
			stopCancel()
			<-joined // no overlap with a noncooperative former worker
			if stopErr != nil {
				s.record(stopErr)
			}
			s.set(run, PilotDegraded)
			return
		}
		stopCancel()
		if stopErr != nil {
			s.record(stopErr)
		}
		if s.ctx.Err() != nil {
			s.set(run, PilotStopped)
			return
		}
		if publicationFailed || stopErr != nil || attempt >= s.policy.maxRestarts {
			s.set(run, PilotDegraded)
			return
		}
		s.set(run, PilotRetrying)
		delay := s.delay(attempt)
		select {
		case <-s.timer.After(delay):
		case <-s.ctx.Done():
			s.set(run, PilotStopped)
			return
		}
	}
}

func constructPilot(factory PilotFactory) (pilot Pilot, err error) {
	defer func() {
		if recover() != nil {
			pilot, err = nil, ErrPilotFactory
		}
	}()
	pilot = factory()
	if nilInterface(pilot) {
		return nil, ErrPilotFactory
	}
	return pilot, nil
}

func composePilotObservation(pilot Pilot, cells *observability.Cells, journal *observability.Journal) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrPilotFactory
		}
	}()
	observed, ok := pilot.(PilotObservationComposition)
	if !ok {
		return nil
	}
	if nilInterface(observed) {
		return ErrPilotFactory
	}
	if err := observed.AttachObservation(cells, journal); err != nil {
		return ErrPilotFactory
	}
	return nil
}

func supervisedCommandDriver(pilot Pilot) (driver CommandDriver, err error) {
	provider, ok := pilot.(SupervisedCommandPilot)
	if !ok {
		return nil, nil
	}
	defer func() {
		if recover() != nil {
			driver, err = nil, ErrPilotFactory
		}
	}()
	driver = provider.CommandDriver()
	if driver != nil && nilInterface(driver) {
		return nil, ErrPilotFactory
	}
	return driver, nil
}

func (s *Supervisor) delay(attempt int) time.Duration {
	base := s.policy.base
	for i := 0; i < attempt && base < s.policy.maximum; i++ {
		if base > s.policy.maximum/2 {
			base = s.policy.maximum
		} else {
			base *= 2
		}
	}
	if base > s.policy.maximum {
		base = s.policy.maximum
	}
	lower := base / 2
	offset := s.jitter.Offset(base - lower)
	if offset < 0 || offset > base-lower {
		return base
	}
	return lower + offset
}

type taskGroup struct {
	mu      sync.Mutex
	ctx     context.Context
	owner   *Supervisor
	run     *pilotRun
	closed  bool
	count   int
	drained chan struct{}
	fail    chan struct{}
	once    sync.Once
}

func newTaskGroup(ctx context.Context, owner *Supervisor, run *pilotRun) *taskGroup {
	return &taskGroup{ctx: ctx, owner: owner, run: run, drained: make(chan struct{}), fail: make(chan struct{})}
}
func (g *taskGroup) Go(task Task) error {
	if task == nil {
		g.Fail()
		return ErrInvalidInput
	}
	if err := g.reserve(true); err != nil {
		return err
	}
	go func() {
		defer func() {
			if recover() != nil {
				g.Fail()
			}
			g.release()
		}()
		if task(g.ctx) != nil {
			g.Fail()
		}
	}()
	return nil
}

// AdmitCommand counts a synchronous dispatch started by an external request
// goroutine. The caller must defer release, including on a repanicking driver.
func (g *taskGroup) AdmitCommand() (func(), error) {
	if err := g.reserve(false); err != nil {
		return nil, err
	}
	return g.release, nil
}

func (g *taskGroup) Fail() {
	if g != nil {
		g.once.Do(func() { close(g.fail) })
	}
}

func (g *taskGroup) reserve(failOnLimit bool) error {
	if g == nil || g.owner == nil || g.run == nil {
		return ErrInvalidInput
	}
	g.mu.Lock()
	if g.closed || g.ctx.Err() != nil {
		g.mu.Unlock()
		if failOnLimit {
			g.Fail()
		}
		return ErrSupervisorState
	}
	g.owner.mu.Lock()
	if g.owner.phase != SupervisorRunning || g.ctx.Err() != nil {
		g.owner.mu.Unlock()
		g.mu.Unlock()
		if failOnLimit {
			g.Fail()
		}
		return ErrSupervisorState
	}
	if g.count >= g.owner.limits.perPilot || g.owner.tasksActive >= g.owner.limits.global {
		g.owner.mu.Unlock()
		g.mu.Unlock()
		if failOnLimit {
			g.Fail()
		}
		return ErrPilotTaskLimit
	}
	g.owner.tasksActive++
	g.run.tasks++
	g.owner.mu.Unlock()
	g.count++
	g.mu.Unlock()
	return nil
}
func (g *taskGroup) release() {
	g.mu.Lock()
	g.count--
	g.owner.mu.Lock()
	g.owner.tasksActive--
	g.run.tasks--
	g.owner.mu.Unlock()
	if g.closed && g.count == 0 {
		close(g.drained)
	}
	g.mu.Unlock()
}
func (g *taskGroup) Count() int { g.mu.Lock(); defer g.mu.Unlock(); return g.count }
func (g *taskGroup) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	g.closed = true
	if g.count == 0 {
		close(g.drained)
	}
}
func (g *taskGroup) Done() <-chan struct{} { return g.drained }
