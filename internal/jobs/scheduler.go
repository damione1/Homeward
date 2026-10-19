package jobs

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/damione1/Homeward/internal/storage"
)

// Timer is injectable so tests can prove polling bounds without wall sleeps.
// A timer only wakes a database poll; it never holds a job or acknowledges one.
type Timer interface {
	Wait(context.Context, time.Duration) error
}
type RealTimer struct{}

func (RealTimer) Wait(ctx context.Context, duration time.Duration) error {
	if nilValue(ctx) || duration <= 0 {
		return failure(Invalid)
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type Phase uint8

const (
	PhaseUnspecified Phase = iota
	SchedulerPrepared
	SchedulerRunning
	SchedulerStorageDegraded
	SchedulerClosing
	SchedulerStopped
	SchedulerFailed
)

type SchedulerStatus struct {
	phase  Phase
	reason ErrorKind
}

func (s SchedulerStatus) Phase() Phase      { return s.phase }
func (s SchedulerStatus) Reason() ErrorKind { return s.reason }

// Scheduler owns two bounded workers in one runtime. The composed server owns
// this scheduler; its command handler borrows the same single service NHA-024.
// No in-memory queue, per-job timer, or automatic command retry exists here.
type Scheduler struct {
	mu           sync.Mutex
	repository   *Repository
	executor     ExecutionPort
	clock        Clock
	timer        Timer
	state        Phase
	cancel       context.CancelFunc
	done         chan struct{}
	err          error
	workers      int
	creations    int
	workerErrors [Workers]ErrorKind
}

func NewScheduler(repository *Repository, executor ExecutionPort, clock Clock, timer Timer) (*Scheduler, error) {
	if repository == nil || repository.store == nil || nilValue(executor) || nilValue(clock) || nilValue(timer) {
		return nil, failure(Invalid)
	}
	return &Scheduler{repository: repository, executor: executor, clock: clock, timer: timer, state: SchedulerPrepared, done: make(chan struct{})}, nil
}

func (s *Scheduler) Start(ctx context.Context) error {
	if s == nil || nilValue(ctx) || s.done == nil {
		return failure(Invalid)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != SchedulerPrepared {
		return failure(Closed)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	s.cancel, s.state = cancel, SchedulerRunning
	s.workers = Workers
	for index := range Workers {
		go func() {
			defer s.workerDone()
			s.worker(workerCtx, index)
		}()
	}
	return nil
}

// Schedule is the runtime admission boundary. A failed/closing scheduler never
// accepts new durable work, and its Done includes already admitted SQL writes.
// Repository.Schedule remains the storage primitive, not an operational API.
func (s *Scheduler) Schedule(ctx context.Context, definition Definition, occurrence OccurrenceID, key IdempotencyKey) (CreateResult, storage.TransactionOutcome, error) {
	if s == nil || s.done == nil || nilValue(ctx) || s.repository == nil {
		return CreateUnspecified, storage.TransactionNotCommitted, failure(Invalid)
	}
	s.mu.Lock()
	switch s.state {
	case SchedulerPrepared, SchedulerRunning, SchedulerStorageDegraded:
		// Creation before Start is allowed only to the trusted composition.
	case SchedulerClosing, SchedulerStopped, SchedulerFailed:
		s.mu.Unlock()
		return CreateUnspecified, storage.TransactionNotCommitted, failure(Closed)
	default:
		s.mu.Unlock()
		return CreateUnspecified, storage.TransactionNotCommitted, failure(Invalid)
	}
	if s.creations >= MaxCreations {
		s.mu.Unlock()
		return CreateUnspecified, storage.TransactionNotCommitted, failure(Capacity)
	}
	s.creations++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.creations--
		s.finishLocked()
		s.mu.Unlock()
	}()
	return s.repository.Schedule(ctx, definition, occurrence, key)
}

func (s *Scheduler) workerDone() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workers--
	if s.workers == 0 && (s.state == SchedulerRunning || s.state == SchedulerStorageDegraded) {
		s.state = SchedulerClosing
	}
	s.finishLocked()
}

func (s *Scheduler) finishLocked() {
	if s.state == SchedulerClosing && s.workers == 0 && s.creations == 0 {
		s.state = SchedulerStopped
		if s.err != nil {
			s.state = SchedulerFailed
		}
		close(s.done)
	}
}

func (s *Scheduler) Status() SchedulerStatus {
	if s == nil || s.done == nil {
		return SchedulerStatus{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	reason := ErrorUnspecified
	var jobError *Error
	if errors.As(s.err, &jobError) {
		reason = jobError.Kind()
	}
	if s.err != nil {
		return SchedulerStatus{s.state, reason}
	}
	for _, workerError := range s.workerErrors {
		if workerError != ErrorUnspecified {
			reason = workerError
			break
		}
	}
	return SchedulerStatus{s.state, reason}
}

func (s *Scheduler) storageStatus(index int, reason ErrorKind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workerErrors[index] = reason
	if s.state != SchedulerRunning && s.state != SchedulerStorageDegraded {
		return
	}
	s.state = SchedulerRunning
	for _, workerError := range s.workerErrors {
		if workerError != ErrorUnspecified {
			s.state = SchedulerStorageDegraded
			break
		}
	}
}

func (s *Scheduler) Done() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.done
}
func (s *Scheduler) CloseAdmissions() {
	if s == nil || s.done == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case SchedulerPrepared:
		s.state = SchedulerClosing
	case SchedulerRunning, SchedulerStorageDegraded:
		s.state = SchedulerClosing
		s.cancel()
	case SchedulerClosing, SchedulerStopped, SchedulerFailed:
		return
	default:
		return
	}
	s.finishLocked()
}
func (s *Scheduler) Close(ctx context.Context) error {
	if s == nil || nilValue(ctx) || s.done == nil {
		return failure(Invalid)
	}
	s.CloseAdmissions()
	select {
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.err
	default:
	}
	select {
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Scheduler) fail(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	if s.state == SchedulerRunning || s.state == SchedulerStorageDegraded {
		s.state = SchedulerClosing
	}
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
}

func (s *Scheduler) worker(ctx context.Context, index int) {
	backoff := PollInterval
	for {
		if ctx.Err() != nil {
			return
		}
		wait := PollInterval
		if err := s.poll(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			var jobError *Error
			if !errors.As(err, &jobError) {
				s.fail(err)
				return
			}
			switch jobError.Kind() {
			case Unavailable, CommitUnknown:
				s.storageStatus(index, jobError.Kind())
				wait = backoff
				backoff = nextDBBackoff(backoff)
			case LeaseLost:
				// The old capability is discarded. A later SQL poll observes
				// its new owner or uncertainty; it never re-executes this claim.
				s.storageStatus(index, ErrorUnspecified)
				backoff = PollInterval
			case ErrorUnspecified, Invalid, Conflict, Closed, Corrupt, Capacity:
				s.fail(err)
				return
			default:
				s.fail(err)
				return
			}
		} else {
			s.storageStatus(index, ErrorUnspecified)
			backoff = PollInterval
		}
		// Every poll, including empty and maintenance-only polls, waits. A large
		// due backlog cannot turn into an unbounded CPU/SQL loop.
		if err := s.timer.Wait(ctx, wait); err != nil {
			if ctx.Err() == nil {
				s.fail(err)
			}
			return
		}
	}
}

func nextDBBackoff(current time.Duration) time.Duration {
	// Same bounded doubling rule as the existing Supervisor/cloud/Recorder
	// policies. Local SQL outages have no provider quota or physical retry.
	if current >= MaxDBBackoff/2 {
		return MaxDBBackoff
	}
	return current * 2
}

func (s *Scheduler) poll(ctx context.Context) error {
	claim, result, _, err := s.repository.Claim(ctx, s.clock.Now().UTC())
	if err != nil {
		return err
	}
	switch result {
	case NoWork, OccurrenceSettled:
		return nil
	case ClaimAcquired:
		// Continue below, with one claim and one handler per worker.
	case ClaimUnspecified:
		return failure(Unavailable)
	default:
		return failure(Unavailable)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	state, _, err := s.repository.BeginDispatch(ctx, claim, s.clock.Now().UTC())
	if err != nil {
		return err
	}
	switch state {
	case Expired, Skipped:
		return nil
	case Dispatching:
		// The committed point after which a restart must not resend.
	case StateUnspecified, Pending, Claimed, Succeeded, Rejected, Indeterminate:
		return failure(Unavailable)
	default:
		return failure(Unavailable)
	}
	duration := ExecutionTimeout
	if remaining := claim.record.definition.expires.Sub(s.clock.Now().UTC()); remaining < duration {
		duration = remaining
	}
	executionCtx, cancel := context.WithTimeout(ctx, duration)
	outcome, executionErr := executeSafely(s.executor, executionCtx, Attempt{claim.record})
	cancel()
	state = outcome.state()
	if executionErr != nil && outcome == Completed {
		// A contradictory handler response cannot claim success.
		state = Indeterminate
	}
	// Cancellation cannot erase a possible effect. Finishing uses a live,
	// bounded SQL context and remains fenced by the actual lease/token.
	finishCtx, finishCancel := context.WithTimeout(context.Background(), SQLTimeout)
	defer finishCancel()
	_, finishErr := s.repository.Finish(finishCtx, claim, state, s.clock.Now().UTC())
	if finishErr != nil {
		return finishErr
	}
	// A refused/uncertain execution is a recorded job result, not a scheduler
	// retry or a fatal error that would starve independent jobs.
	return nil
}

func executeSafely(executor ExecutionPort, ctx context.Context, attempt Attempt) (outcome Outcome, err error) {
	defer func() {
		if recover() != nil {
			outcome, err = Uncertain, failure(Invalid)
		}
	}()
	return executor.Execute(ctx, attempt)
}
