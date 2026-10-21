package configdocuments

import (
	"context"
	"sync"
	"time"

	"github.com/damione1/Homeward/internal/configactivation"
)

const watcherPeriod = 250 * time.Millisecond
const watcherStability = 500 * time.Millisecond
const watcherRetry = 2 * time.Second

// A stability window has constant storage regardless of filesystem activity.
// Digest AND observed date must remain equal. mtime alone never detects edits.
type stableObservation struct {
	observation Observation
	since       time.Time
	present     bool
}

func (s *stableObservation) admit(observation Observation, now time.Time) bool {
	if !observation.present || observation.pending || now.IsZero() {
		*s = stableObservation{}
		return false
	}
	if !s.present || s.observation.hash != observation.hash || !s.observation.updated.Equal(observation.updated) || now.Before(s.since) {
		*s = stableObservation{observation: observation, since: now, present: true}
		return false
	}
	return now.Sub(s.since) >= watcherStability
}

// Watcher borrows the exact Service149; it neither writes files nor owns a
// second compiler, principal, ledger or revision. One joined goroutine performs
// all observations and activation attempts; events never spawn more work.
type Watcher struct {
	service         *Service
	clock           WatcherClock
	mu              sync.Mutex
	started, closed bool
	cancel          context.CancelFunc
	done            chan struct{}
	finish          sync.Once
}

func NewWatcher(service *Service) (*Watcher, error) {
	if service == nil || !service.admitted || service.workspace == nil || service.owner == nil {
		return nil, &Error{kind: FailureInvalid}
	}
	return &Watcher{service: service, clock: RealWatcherClock{}, done: make(chan struct{})}, nil
}
func (w *Watcher) Start(ctx context.Context) error {
	if w == nil || w.service == nil || w.done == nil || !validWatcherClock(w.clock) || !usableContext(ctx) || ctx.Err() != nil {
		return &Error{kind: FailureInvalid}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started || w.closed {
		return &Error{kind: FailureInvalid}
	}
	runCtx, cancel := context.WithCancel(ctx)
	w.cancel, w.started = cancel, true
	go func() { defer w.finish.Do(func() { close(w.done) }); w.run(runCtx) }()
	return nil
}
func (w *Watcher) BeginClose() {
	if w == nil || w.done == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	if w.cancel != nil {
		w.cancel()
	}
	if !w.started {
		w.finish.Do(func() { close(w.done) })
	}
}
func (w *Watcher) Done() <-chan struct{} {
	if w == nil || w.done == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return w.done
}
func (w *Watcher) Close(ctx context.Context) error {
	if !usableContext(ctx) {
		return &Error{kind: FailureInvalid}
	}
	w.BeginClose()
	select {
	case <-w.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (w *Watcher) run(ctx context.Context) {
	var stable stableObservation
	var last Observation
	var observed, available bool
	var completed Observation
	var retryAt time.Time
	var changes <-chan struct{}
	var stopChanges func()
	defer func() {
		if stopChanges != nil {
			stopChanges()
		}
	}()
	for {
		if ctx.Err() != nil {
			return
		}
		if changes == nil {
			// Register BEFORE reading any observation/authority. An unresolved
			// owner is retried after the same bounded wait, never another owner.
			if signal, stop, err := w.service.owner.SubscribeConfigChanges(ctx); err == nil {
				changes, stopChanges = signal, stop
				completed = Observation{}
			}
		}
		select {
		case _, open := <-changes:
			// Coalesced publication notifications invalidate all prior terminal
			// decisions; the next Refresh reads the actual current SQL cut.
			completed = Observation{}
			if !open {
				stopChanges()
				stopChanges = nil
				changes = nil
			}
		default:
		}
		now := w.clock.Now()
		observation, observeErr := w.service.workspace.Observe()
		changed := !observed || available != (observeErr == nil) || last != observation
		if changed {
			last, observed, available = observation, true, observeErr == nil
			_, _ = w.service.Refresh(ctx)
		}
		if observeErr != nil || observation.pending {
			stable = stableObservation{}
			if !now.Before(retryAt) {
				_ = w.service.ReconcileObserved(ctx)
				retryAt = now.Add(watcherRetry)
			}
		} else if stable.admit(observation, now) && completed != observation && !now.Before(retryAt) {
			current, err := w.service.Refresh(ctx)
			if err != nil {
				_ = w.service.ReconcileObserved(ctx)
				retryAt = w.clock.Now().Add(watcherRetry)
			} else if current.observation != observation {
				stable = stableObservation{}
			} else if current.dirty == Clean || current.dirty == DirtyRejected {
				completed = observation
			} else {
				expected, expectedErr := NewExpectedDocument(operationalID(), current.document.Ref().Revision(), current.document.SourceSHA256(), observation.hash)
				if expectedErr == nil {
					result, activationErr := w.service.ActivateObserved(ctx, expected)
					switch result.Outcome() {
					case configactivation.Published, configactivation.RestartRequired:
						completed = observation
					case configactivation.RejectedBeforeCommit:
						if activationDiagnostic(observation, result, activationErr) == CandidateRejected {
							completed = observation
						} else {
							retryAt = w.clock.Now().Add(watcherRetry)
						}
					case configactivation.CommitUnknown, configactivation.CommittedReconciling:
						_ = w.service.ReconcileObserved(ctx)
						retryAt = w.clock.Now().Add(watcherRetry)
					case configactivation.NotCommitted, configactivation.OutcomeInvalid:
						retryAt = w.clock.Now().Add(watcherRetry)
					default:
						retryAt = w.clock.Now().Add(watcherRetry)
					}
				}
			}
		}
		if w.clock.Wait(ctx, watcherPeriod) != nil {
			return
		}
	}
}
