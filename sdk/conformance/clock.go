package conformance

import (
	"context"
	"sync"
	"time"

	"github.com/damione1/Homeward/internal/integrations"
)

// clock drives the sequencer's receipt time, command admission, action
// progression and supervisor retry from one synthetic timeline.
type clock struct {
	mu     sync.Mutex
	now    time.Time
	mono   time.Duration
	timers []*clockTimer
	armed  chan struct{}
}

type clockTimer struct {
	clock   *clock
	at      time.Duration
	ch      chan time.Time
	stopped bool
}

func newClock(start time.Time) (*clock, error) {
	if start.IsZero() {
		return nil, integrations.ErrInvalidInput
	}
	return &clock{now: start.UTC(), armed: make(chan struct{}, 16)}, nil
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) MonoNow() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mono
}

func (c *clock) timer(d time.Duration) *clockTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &clockTimer{clock: c, at: c.mono + d, ch: make(chan time.Time, 1)}
	if d <= 0 {
		t.ch <- c.now
		t.stopped = true
	} else {
		c.timers = append(c.timers, t)
	}
	select {
	case c.armed <- struct{}{}:
	default:
	}
	return t
}

func (c *clock) NewTimer(d time.Duration) integrations.ActionTimer { return c.timer(d) }
func (c *clock) After(d time.Duration) <-chan time.Time            { return c.timer(d).C() }
func (t *clockTimer) C() <-chan time.Time                          { return t.ch }

func (t *clockTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	for i, candidate := range t.clock.timers {
		if candidate == t {
			t.clock.timers = append(t.clock.timers[:i], t.clock.timers[i+1:]...)
			break
		}
	}
	return was
}

func (c *clock) advance(d time.Duration) error {
	if c == nil || d < 0 {
		return integrations.ErrInvalidInput
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.mono += d
	active := c.timers[:0]
	for _, timer := range c.timers {
		if !timer.stopped && c.mono >= timer.at {
			timer.ch <- c.now
			timer.stopped = true
		}
		if !timer.stopped {
			active = append(active, timer)
		}
	}
	c.timers = active
	return nil
}

func (c *clock) waitArmed(ctx context.Context) error {
	select {
	case <-c.armed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
