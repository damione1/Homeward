package ecobee

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mctofu/homekit/client"
)

type manualDeadline struct {
	context.Context
	deadline time.Time
	expired  atomic.Bool
}

func (d *manualDeadline) Deadline() (time.Time, bool) { return d.deadline, true }

func (d *manualDeadline) Err() error {
	if d.expired.Load() {
		return context.DeadlineExceeded
	}
	return d.Context.Err()
}

type manualTimer struct {
	ctx    *manualDeadline
	cancel context.CancelFunc
}

type manualPollClock struct {
	mu       sync.Mutex
	at       time.Time
	timers   []manualTimer
	duration time.Duration
}

func newManualPollClock() *manualPollClock {
	return &manualPollClock{at: time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)}
}

func (c *manualPollClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *manualPollClock) timeout(parent context.Context, duration time.Duration) (context.Context, context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	base, cancel := context.WithCancel(parent)
	d := &manualDeadline{Context: base, deadline: c.at.Add(duration)}
	c.timers = append(c.timers, manualTimer{ctx: d, cancel: cancel})
	c.duration = duration
	return d, cancel
}

func (c *manualPollClock) advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(duration)
	for _, timer := range c.timers {
		if !c.at.Before(timer.ctx.deadline) && timer.ctx.Context.Err() == nil {
			timer.ctx.expired.Store(true)
			timer.cancel()
		}
	}
}

func (c *manualPollClock) clock() pollClock { return pollClock{now: c.now, timeout: c.timeout} }

func TestNHA136PollingIntervalAndFreshness(t *testing.T) {
	p := admittedTestProfile(t)
	clock := newManualPollClock()
	var calls int
	readErr := error(nil)
	poller, err := newProfilePoller(p, clock.clock(), func(_ context.Context, req *client.CharacteristicsReadRequest) ([]*client.CharacteristicReadResponse, error) {
		calls++
		assertLiteralReadIDs(t, req)
		return observationRows(), readErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := poller.poll(context.Background()); err != nil || calls != 1 || clock.duration != 3*time.Second {
		t.Fatal("initial grouped cycle/3-second timeout missing:", err)
	}
	clock.advance(15*time.Second - time.Nanosecond)
	if _, err := poller.poll(context.Background()); !errors.Is(err, errPollNotDue) || calls != 1 {
		t.Fatal("poll admitted sooner than 15 seconds")
	}
	clock.advance(time.Nanosecond)
	readErr = errors.New("synthetic read unavailable")
	if _, err := poller.poll(context.Background()); !errors.Is(err, readErr) || calls != 2 {
		t.Fatal("cycle due at 15 seconds was hidden/retried")
	}
	clock.advance(30 * time.Second)
	if activity, kind := poller.snapshot().activity(clock.now()); activity != activityUnknown || kind != valueStale {
		t.Fatal("failed read refreshed the last successful observation into valid idle")
	}
	readErr = nil
	if _, err := poller.poll(context.Background()); err != nil || calls != 3 {
		t.Fatal("fresh successful cycle did not recover:", err)
	}
	if activity, kind := poller.snapshot().activity(clock.now()); activity != activityIdle || kind != valuePresent {
		t.Fatal("recovered grouped read remained stale")
	}
}

func TestNHA136PollingOneInFlightAndTimeout(t *testing.T) {
	p := admittedTestProfile(t)
	clock := newManualPollClock()
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, done, joined := make(chan struct{}), make(chan error, 1), make(chan struct{})
	var calls atomic.Int32
	poller, err := newProfilePoller(p, clock.clock(), func(ctx context.Context, req *client.CharacteristicsReadRequest) ([]*client.CharacteristicReadResponse, error) {
		calls.Add(1)
		deadline, ok := ctx.Deadline()
		if !ok || deadline.Sub(clock.now()) != 3*time.Second {
			t.Error("reader did not receive one group with a 3-second deadline")
		}
		assertLiteralReadIDs(t, req)
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { defer close(joined); _, err := poller.poll(parent); done <- err }()
	t.Cleanup(func() {
		cancel()
		waitNHA136PollSignal(t, joined, "synthetic timeout poll joined")
	})
	// This wait is only a bounded synchronization guard, not a wall-clock poll.
	waitNHA136PollSignal(t, entered, "synthetic grouped read entered")
	if _, err := poller.poll(parent); !errors.Is(err, errPollInFlight) || calls.Load() != 1 {
		t.Fatal("overlapping poll admitted")
	}
	clock.advance(3*time.Second - time.Nanosecond)
	select {
	case <-done:
		t.Fatal("grouped read timed out before 3 seconds")
	default:
	}
	clock.advance(time.Nanosecond)
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
			t.Fatal("timeout was hidden or retried:", err)
		}
	case <-time.After(nha136PollJoinTimeout):
		t.Fatal("simulated timeout did not join the poll")
	}
	if _, kind := poller.snapshot().activity(clock.now()); kind != valueAbsent {
		t.Fatal("failed initial read manufactured a present zero")
	}
	if _, err := poller.poll(parent); !errors.Is(err, errPollNotDue) {
		t.Fatal("timeout bypassed the 15-second cadence")
	}
	wall := wallPollClock()
	ctx, stop := wall.timeout(context.Background(), pollTimeout)
	defer stop()
	deadline, ok := ctx.Deadline()
	if remaining := deadline.Sub(wall.now()); !ok || remaining <= 0 || remaining > pollTimeout {
		t.Fatal("real clock does not use a bounded context deadline")
	}
}

func TestNHA136PublishedPollingLatencyBudgetIncludesOneCommand(t *testing.T) {
	policy := PollingLimits()
	if policy.Interval != 15*time.Second || policy.Timeout != 3*time.Second || policy.MaximumAge != 45*time.Second || policy.MaximumObservationLatency != 24*time.Second {
		t.Fatalf("polling policy drift: %+v", policy)
	}
	if policy.MaximumObservationLatency >= policy.MaximumAge {
		t.Fatal("normal polling can outlive freshness")
	}
}

// This bounds fixture synchronization only; provider deadlines use the manual clock.
const nha136PollJoinTimeout = 5 * time.Second

func waitNHA136PollSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(nha136PollJoinTimeout):
		t.Fatal(description + " did not complete within fixture synchronization budget")
	}
}
