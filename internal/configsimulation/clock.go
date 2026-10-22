package configsimulation

import (
	"context"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"sync"
	"time"
)

// Only sequential programs are admitted. A timer is therefore the unique next
// asynchronous boundary; advancing it cannot race another branch's clock.
type fixtureClock struct {
	mu             sync.Mutex
	start          time.Time
	elapsed, limit time.Duration
	exhausted      bool
	cancel         context.CancelFunc
}

func (c *fixtureClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.start.Add(c.elapsed)
}
func (c *fixtureClock) MonoNow() time.Duration { c.mu.Lock(); defer c.mu.Unlock(); return c.elapsed }
func (c *fixtureClock) NewTimer(delay time.Duration) automationtriggers.Timer {
	c.mu.Lock()
	if delay < 0 || delay > c.limit-c.elapsed {
		c.exhausted = true
		c.cancel()
	} else {
		c.elapsed += delay
	}
	at := c.start.Add(c.elapsed)
	c.mu.Unlock()
	timer := &fixtureTimer{events: make(chan time.Time, 1)}
	timer.events <- at
	return timer
}

type fixtureTimer struct{ events chan time.Time }

func (t *fixtureTimer) C() <-chan time.Time { return t.events }
func (*fixtureTimer) Stop() bool            { return false }
