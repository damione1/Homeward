package remotecalendar

import (
	"context"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
	"sync"
	"time"
)

const nominalPoll = 24 * time.Hour
const localRenewal = 12 * time.Hour
const clockGuardInterval = time.Second

type cadence struct {
	poll, renew, expiry             time.Time
	pollMono, renewMono, expiryMono time.Duration
	armed, window                   bool
}

func (c *cadence) pollSucceeded(now time.Time, mono time.Duration, until time.Time) {
	delay := nominalPoll
	if d := until.Sub(now); d > 0 && d < delay {
		delay = d
	}
	c.poll = now.Add(delay)
	c.pollMono = mono + delay
	c.expiry = until
	c.expiryMono = mono + max(time.Duration(0), until.Sub(now))
	c.armed = true
}
func (c *cadence) windowSucceeded(now time.Time, mono time.Duration) {
	c.renew = now.Add(localRenewal)
	c.renewMono = mono + localRenewal
	c.window = true
}
func (c *cadence) expired(now time.Time, mono time.Duration) bool {
	return c.armed && (!now.Before(c.expiry) || mono >= c.expiryMono)
}
func (c *cadence) due(now time.Time, mono time.Duration, fresh bool) intent {
	if c.armed && (!now.Before(c.poll) || mono >= c.pollMono) {
		return intentRefresh
	}
	if fresh && c.window && (!now.Before(c.renew) || mono >= c.renewMono) {
		return intentRenewWindow
	}
	return intentNone
}
func (c *cadence) deferPoll(now time.Time, mono time.Duration) {
	c.poll = now.Add(nominalPoll)
	c.pollMono = mono + nominalPoll
	c.armed = true
}

func (c *cadence) nextDelay(now time.Time, mono time.Duration, fresh bool) time.Duration {
	delay := clockGuardInterval
	include := func(at time.Time, elapsed time.Duration) {
		if d := at.Sub(now); d < delay {
			delay = d
		}
		if d := elapsed - mono; d < delay {
			delay = d
		}
	}
	if c.armed {
		include(c.poll, c.pollMono)
	}
	if fresh && c.armed {
		include(c.expiry, c.expiryMono)
	}
	if fresh && c.window {
		include(c.renew, c.renewMono)
	}
	return max(time.Duration(0), delay)
}

type Pilot struct {
	mu      sync.Mutex
	source  Source
	clock   automationtriggers.Clock
	run     *sourceRun
	started bool
}

func NewPilot(source Source, clock automationtriggers.Clock) (*Pilot, error) {
	if !source.valid() || nilPort(clock) || clock.Now().IsZero() || clock.MonoNow() < 0 {
		return nil, failure(Invalid)
	}
	return &Pilot{source: source, clock: clock}, nil
}
func NewPilotFactory(source Source, clock automationtriggers.Clock) (integrations.PilotFactory, error) {
	if _, e := NewPilot(source, clock); e != nil {
		return nil, e
	}
	return func() integrations.Pilot { p, _ := NewPilot(source, clock); return p }, nil
}
func (p *Pilot) Start(ctx context.Context, host integrations.SessionHost, tasks integrations.Tasks) error {
	if p == nil || !validContext(ctx) || nilPort(host) || nilPort(tasks) {
		return failure(Invalid)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return failure(Invalid)
	}
	p.started = true
	runctx, cancel := context.WithCancel(ctx)
	port, e := p.source.http.Open(runctx, host)
	if e != nil {
		cancel()
		return failure(Transport)
	}
	r := &sourceRun{source: p.source, clock: p.clock, ctx: runctx, cancel: cancel, http: port, wake: make(chan struct{}, 1), events: make(chan workerEvent, 1), done: make(chan struct{}), tasks: 2, reject: storage.PreserveAdmittedValidators, reason: calendars.RemoteReasonUnavailable, lastWall: p.clock.Now(), lastMono: p.clock.MonoNow()}
	r.cadence.deferPoll(r.lastWall, r.lastMono)
	p.run = r
	if e = tasks.Go(r.workerLoop); e != nil {
		cancel()
		port.Close()
		r.finishTask()
		r.finishTask()
		return failure(Closed)
	}
	if e = tasks.Go(r.controlLoop); e != nil {
		cancel()
		r.finishTask()
		return failure(Closed)
	}
	r.enqueue(intentRefresh)
	return nil
}
func (p *Pilot) Refresh(ctx context.Context) error {
	if p == nil || !validContext(ctx) {
		return failure(Invalid)
	}
	p.mu.Lock()
	r := p.run
	p.mu.Unlock()
	if r == nil {
		return failure(Closed)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.ctx.Err() != nil {
		return failure(Closed)
	}
	r.enqueueLocked(intentRefresh)
	return nil
}
func (p *Pilot) Stop(ctx context.Context) error {
	if p == nil || nilPort(ctx) {
		return failure(Invalid)
	}
	p.mu.Lock()
	r := p.run
	p.mu.Unlock()
	if r == nil {
		return nil
	}
	// Cancel immediately, before waiting for the local publication admission.
	r.shutdown()
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return failure(Closed)
	}
}
func (r *sourceRun) shutdown() {
	r.cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	r.invalidateLocked(calendars.RemoteReasonUnavailable, storage.PreserveAdmittedValidators)
	r.pending = intentStop
}
func (r *sourceRun) finishTask() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tasks--
	if r.tasks == 0 {
		r.commit = nil
		r.known = version{}
		r.pending = intentNone
		for {
			select {
			case e := <-r.events:
				if e.ack != nil {
					select {
					case e.ack <- failure(Closed):
					default:
					}
				}
			default:
				close(r.done)
				return
			}
		}
	}
}
func (r *sourceRun) workerLoop(taskCtx context.Context) error {
	stop := context.AfterFunc(taskCtx, r.cancel)
	defer stop()
	defer r.finishTask()
	defer r.cancel()
	defer r.http.Close()
	for {
		select {
		case <-r.ctx.Done():
			return nil
		case <-r.wake:
		}
		r.mu.Lock()
		job := r.pending
		r.pending = intentNone
		if r.closed {
			r.mu.Unlock()
			return nil
		}
		op, cancel := context.WithCancel(r.ctx)
		r.workerCancel = cancel
		r.mu.Unlock()
		var err error
		switch job {
		case intentRefresh:
			err = r.refresh(op)
		case intentRenewWindow:
			err = r.collect(op, false)
		case intentPersistStale:
			err = r.persistStale(op)
		case intentNone:
		case intentStop:
			cancel()
			return nil
		}
		wasCanceled := op.Err() != nil
		cancel()
		r.mu.Lock()
		r.workerCancel = nil
		r.mu.Unlock()
		if err != nil && !wasCanceled && job != intentPersistStale && r.ctx.Err() == nil {
			reason, disposition := classify(err)
			r.fail(reason, disposition, sourceFailureKind(err))
			r.mu.Lock()
			r.cadence.deferPoll(r.clock.Now(), r.clock.MonoNow())
			r.mu.Unlock()
		}
	}
}
func (r *sourceRun) controlLoop(taskCtx context.Context) error {
	stop := context.AfterFunc(taskCtx, r.cancel)
	defer stop()
	defer r.finishTask()
	defer r.shutdown()
	for {
		r.mu.Lock()
		delay := r.cadence.nextDelay(r.clock.Now(), r.clock.MonoNow(), r.fresh)
		r.mu.Unlock()
		timer := r.clock.NewTimer(delay)
		if nilPort(timer) || timer.C() == nil {
			return failure(Invalid)
		}
		select {
		case <-r.ctx.Done():
			timer.Stop()
			return nil
		case e := <-r.events:
			timer.Stop()
			r.mu.Lock()
			err := r.admitEventLocked(e)
			if err != nil && !r.closed {
				reason := calendars.RemoteReasonPublication
				if x, ok := err.(*Error); ok && x.Kind() == Expired {
					reason = calendars.RemoteReasonExpired
				}
				r.lastFailure = &Error{kind: sourceFailureKind(err)}
				r.invalidateLocked(reason, storage.PreserveAdmittedValidators)
				r.enqueueLocked(intentPersistStale)
			}
			r.mu.Unlock()
			e.ack <- err
		case <-timer.C():
			timer.Stop()
			r.mu.Lock()
			if r.closed {
				r.mu.Unlock()
				return nil
			}
			now, mono := r.clock.Now().UTC(), r.clock.MonoNow()
			wallDelta, monoDelta := now.Sub(r.lastWall), mono-r.lastMono
			jumped := monoDelta < 0 || wallDelta-monoDelta > time.Second || monoDelta-wallDelta > time.Second
			r.lastWall, r.lastMono = now, mono
			if jumped || r.fresh && r.cadence.expired(now, mono) {
				r.lastFailure = &Error{kind: Expired}
				r.invalidateLocked(calendars.RemoteReasonExpired, storage.PreserveAdmittedValidators)
				r.enqueueLocked(intentPersistStale)
			}
			if job := r.cadence.due(now, mono, r.fresh); job != intentNone {
				r.enqueueLocked(job)
				if job == intentRefresh {
					r.cadence.deferPoll(now, mono)
				} else {
					r.cadence.windowSucceeded(now, mono)
				}
			}
			r.mu.Unlock()
		}
	}
}
func (r *sourceRun) admitEventLocked(e workerEvent) error {
	if r.closed || r.ctx.Err() != nil || e.epoch != r.epoch {
		return failure(Closed)
	}
	if e.commit != nil {
		k := e.commit
		store, err := k.observation.StoreObservation()
		if err != nil {
			return failure(Closed)
		}
		source, ok := store.Source().Key()
		current, cok := r.scope.Key()
		id, iok := store.ID()
		entity, _ := r.source.id.Entity()
		rev, rok := k.receipt.Revision()
		digest, dok := k.receipt.Digest()
		if !ok || !cok || !iok || !rok || !dok || source != current || !k.receipt.Matches(entity, source, id) {
			return failure(Invalid)
		}
		now, mono := r.clock.Now().UTC(), r.clock.MonoNow()
		r.cadence.pollSucceeded(k.at, k.mono, k.until)
		if now.Before(k.at) || mono < k.mono || r.cadence.expired(now, mono) {
			return failure(Expired)
		}
		next := version{revision: uint64(rev), digest: digest, scope: source}
		if r.source.publication.kind == publicationEnabled {
			if err := r.http.BeginConditionalPublication(r.ctx, k.observation); err != nil {
				return failure(PublicationFailed)
			}
			if r.cadence.window && r.known != next {
				if err := r.source.publication.port.Degrade(context.WithoutCancel(r.ctx), calendars.RemoteReasonPublication); err != nil {
					return failure(PublicationFailed)
				}
			}
		}
		r.commit = k
		r.known = next
		r.fresh = true
		if r.source.publication.kind == publicationDisabled {
			if err := r.http.ConfirmConditionalAdmitted(r.ctx, k.observation, k.receipt); err != nil {
				return failure(PublicationFailed)
			}
			r.commit = nil
		}
		return nil
	}
	return r.publishLocked(e.window, e.refresh)
}
