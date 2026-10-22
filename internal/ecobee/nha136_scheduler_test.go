package ecobee

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/sdk/conformance"
)

type schedulerTimer struct {
	at time.Time
	ch chan time.Time
}
type schedulerClock struct {
	mu     sync.Mutex
	at     time.Time
	timers []schedulerTimer
}

func (c *schedulerClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *schedulerClock) After(duration time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if duration <= 0 {
		ch <- c.at
	} else {
		c.timers = append(c.timers, schedulerTimer{c.at.Add(duration), ch})
	}
	return ch
}
func (c *schedulerClock) advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(duration)
	pending := c.timers[:0]
	for _, timer := range c.timers {
		if !c.at.Before(timer.at) {
			timer.ch <- c.at
		} else {
			pending = append(pending, timer)
		}
	}
	c.timers = pending
}
func TestNHA136ComposedPilotPrioritizesDuePollAfterOneCommand(t *testing.T) {
	profile, err := conformance.NewClimateFixtureProfile(conformance.FixtureSource{Device: "CA:FE:00:11:22:33", Entity: "thermostat", Endpoint: Endpoint, Schema: contracts.ClimateSchema})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var adapters []*climateTestAdapter
	var starts []time.Time
	failures := conformance.Check(ctx, func(_ conformance.Case, _ string) (conformance.Adapter, error) {
		started := time.Now()
		clock := &schedulerClock{at: started}
		adapter := newClimateAdapterWithClock(t, false, clock)
		adapters = append(adapters, adapter)
		starts = append(starts, started)
		return adapter, nil
	}, conformance.Options{WorkDir: t.TempDir(), Timeout: 15 * time.Second, Profile: &profile})
	if len(failures) != 0 {
		t.Fatal(failures)
	}
	// Core's first command starts at t14 and simulates the full PUT3s+GET3s
	// allowance. Its due t15 poll must run at t20 before the next queued command.
	// This observes actual encrypted GETs from Pilot.run, not a scheduler stub.
	adapter := adapters[0]
	adapter.mu.Lock()
	polls := append([]time.Time(nil), adapter.periodicAt...)
	adapter.mu.Unlock()
	if len(polls) < 2 || !polls[0].Equal(starts[0]) || !polls[1].Equal(starts[0].Add(20*time.Second)) {
		t.Fatalf("actual periodic GET times=%v", polls)
	}
}
