package ecobee

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mctofu/homekit/client"
)

const (
	pollInterval      = 15 * time.Second
	pollTimeout       = 3 * time.Second
	observationMaxAge = 45 * time.Second
)

var (
	errPollInFlight = errors.New("qualification poll already in flight")
	errPollNotDue   = errors.New("qualification poll not due")
)

// The standard context timer is used in a real run. Tests replace both clock
// functions so a timeout or 45-second expiry needs no wall-clock sleep.
type pollClock struct {
	now     func() time.Time
	timeout func(context.Context, time.Duration) (context.Context, context.CancelFunc)
}

func wallPollClock() pollClock {
	return pollClock{now: time.Now, timeout: context.WithTimeout}
}

type groupedRead func(context.Context, *client.CharacteristicsReadRequest) ([]*client.CharacteristicReadResponse, error)

type qualificationPoller struct {
	profile climateProfile
	clock   pollClock
	read    groupedRead
	mu      sync.Mutex
	busy    bool
	next    time.Time
	latest  climateObservation
}

func newQualificationPoller(profile climateProfile, clock pollClock, read groupedRead) (*qualificationPoller, error) {
	if !profile.admitted || clock.now == nil || clock.timeout == nil || read == nil {
		return nil, fmt.Errorf("poller requires an admitted profile, clock and reader")
	}
	return &qualificationPoller{profile: profile, clock: clock, read: read}, nil
}

// poll performs exactly one grouped GET, with no retry or hidden reconnect.
// The reader must honor its context, as AccessoryClient.Characteristics does.
// A failed/partial cycle does not refresh older values or manufacture idle.
func (p *qualificationPoller) poll(ctx context.Context) (climateObservation, error) {
	p.mu.Lock()
	if p.busy {
		p.mu.Unlock()
		return climateObservation{}, errPollInFlight
	}
	started := p.clock.now()
	if started.Before(p.next) {
		p.mu.Unlock()
		return climateObservation{}, errPollNotDue
	}
	if err := ctx.Err(); err != nil {
		p.mu.Unlock()
		return climateObservation{}, err
	}
	p.busy, p.next = true, started.Add(pollInterval)
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.busy = false; p.mu.Unlock() }()

	request, err := p.profile.readRequest()
	if err != nil {
		return climateObservation{}, err
	}
	readCtx, cancel := p.clock.timeout(ctx, pollTimeout)
	defer cancel()
	rows, err := p.read(readCtx, request)
	if readCtx.Err() != nil {
		return climateObservation{}, readCtx.Err()
	}
	if err != nil {
		return climateObservation{}, err
	}
	observation, err := p.profile.observe(rows, p.clock.now())
	if err != nil {
		return climateObservation{}, err
	}
	p.mu.Lock()
	p.latest = observation
	p.mu.Unlock()
	return observation, nil
}

func (p *qualificationPoller) snapshot() climateObservation {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.latest
}
