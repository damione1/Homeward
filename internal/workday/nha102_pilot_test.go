package workday

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
)

type nha102Clock struct {
	mu     sync.Mutex
	wall   time.Time
	tick   time.Duration
	timers []*nha102Timer
	armed  chan struct{}
}
type nha102Timer struct {
	clock   *nha102Clock
	due     time.Duration
	ch      chan time.Time
	stopped bool
}

func (c *nha102Clock) Now() time.Time         { c.mu.Lock(); defer c.mu.Unlock(); return c.wall }
func (c *nha102Clock) MonoNow() time.Duration { c.mu.Lock(); defer c.mu.Unlock(); return c.tick }
func (c *nha102Clock) NewTimer(delay time.Duration) automationtriggers.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &nha102Timer{clock: c, due: c.tick + delay, ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, timer)
	select {
	case c.armed <- struct{}{}:
	default:
	}
	return timer
}
func (t *nha102Timer) C() <-chan time.Time { return t.ch }
func (t *nha102Timer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}
func (c *nha102Clock) move(wall, elapsed time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(wall)
	c.tick += elapsed
	for _, timer := range c.timers {
		if !timer.stopped && timer.due <= c.tick {
			select {
			case timer.ch <- c.wall:
			default:
			}
		}
	}
}
func (c *nha102Clock) wake() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, timer := range c.timers {
		if !timer.stopped {
			select {
			case timer.ch <- c.wall:
			default:
			}
		}
	}
}

type nha102Tasks struct {
	ctx  context.Context
	done chan error
}

func (tasks *nha102Tasks) Go(task integrations.Task) error {
	go func() { tasks.done <- task(tasks.ctx) }()
	return nil
}

type nha102Host struct {
	integrations.SessionHost
	published chan integrations.StateObservation
}

func (host *nha102Host) Publish(ctx context.Context, binding integrations.Binding, observation integrations.StateObservation) error {
	if err := host.SessionHost.Publish(ctx, binding, observation); err != nil {
		return err
	}
	host.published <- observation
	return nil
}
func nha102Published(t *testing.T, channel <-chan integrations.StateObservation, want bool) {
	t.Helper()
	select {
	case observation := <-channel:
		_, quality, value, captured, _, _, _, err := observation.Components()
		if err != nil || value == nil || captured == nil {
			t.Fatalf("missing present Workday observation: %v", err)
		}
		got, present := value.Switch()
		if !present || got != want {
			t.Fatalf("Workday observation got (%t,%t), want present %t", got, present, want)
		}
		availability, freshness, provenance, err := quality.Components()
		if err != nil || availability != domain.AvailabilityAvailable || freshness != domain.FreshnessFresh || provenance != domain.ProvenanceObserved {
			t.Fatalf("Workday state was not a fresh available observation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Workday observation timed out")
	}
}
func nha102Armed(t *testing.T, c *nha102Clock) {
	t.Helper()
	select {
	case <-c.armed:
	case <-time.After(3 * time.Second):
		t.Fatal("Workday timer not armed")
	}
}

func TestNHA102PilotClockJumpsDuplicateWakeAndStop(t *testing.T) {
	clock := &nha102Clock{wall: time.Date(2026, 12, 25, 12, 0, 0, 0, time.UTC), armed: make(chan struct{}, 32)}
	manager, _, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := InstanceID()
	session, err := manager.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	host := &nha102Host{SessionHost: session, published: make(chan integrations.StateObservation, 16)}
	ctx, cancel := context.WithCancel(context.Background())
	tasks := &nha102Tasks{ctx: ctx, done: make(chan error, 1)}
	factory, err := NewPilotFactory(nha102Settings(t, "America/Toronto"), clock)
	if err != nil {
		t.Fatal(err)
	}
	pilot := factory()
	if err := pilot.Start(ctx, host, tasks); err != nil {
		t.Fatal(err)
	}
	joined := false
	defer func() {
		_ = pilot.Stop(context.Background())
		cancel()
		if !joined {
			select {
			case err := <-tasks.done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(3 * time.Second):
				t.Error("pilot task did not join")
			}
		}
	}()
	nha102Published(t, host.published, true) // Friday Christmas
	nha102Armed(t, clock)
	clock.wake() // stale/repeated wake with unchanged local date
	nha102Armed(t, clock)
	select {
	case <-host.published:
		t.Fatal("duplicate wake republished Workday")
	default:
	}
	clock.move(17*time.Hour, 17*time.Hour) // Saturday local midnight
	nha102Published(t, host.published, false)
	nha102Armed(t, clock)
	clock.move(55*time.Hour, time.Minute) // Monday; skipped Sunday is not replayed
	nha102Published(t, host.published, true)
	nha102Armed(t, clock)
	select {
	case <-host.published:
		t.Fatal("skipped dates replayed")
	default:
	}
	clock.move(-48*time.Hour, time.Minute) // Saturday again after wall-clock rollback
	nha102Published(t, host.published, false)
	nha102Armed(t, clock)
	if err := pilot.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-tasks.done:
		joined = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pilot task did not join after Stop")
	}
	clock.move(72*time.Hour, 72*time.Hour)
	select {
	case <-host.published:
		t.Fatal("pilot published after Stop")
	default:
	}
	clock.mu.Lock()
	defer clock.mu.Unlock()
	for _, timer := range clock.timers {
		if !timer.stopped {
			t.Fatal("timer remained armed after Stop")
		}
	}
}
