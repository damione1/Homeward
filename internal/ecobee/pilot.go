package ecobee

import (
	"context"
	"errors"
	"net"
	"reflect"
	"sync"
	"time"

	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/scope"
	"github.com/mctofu/homekit/client"
)

type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}
type wallClock struct{}

func (wallClock) Now() time.Time                         { return time.Now() }
func (wallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

type Pilot struct {
	settings Settings
	clock    Clock
	commands chan commandJob
	mu       sync.Mutex
	api      *client.AccessoryClient
	host     integrations.SessionHost
	binding  integrations.Binding
	profile  climateProfile
	periodic *profilePoller
	running  bool
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func NewPilotFactory(settings Settings) (integrations.PilotFactory, error) {
	return NewPilotFactoryWithClock(settings, wallClock{})
}
func NewPilotFactoryWithClock(settings Settings, clock Clock) (integrations.PilotFactory, error) {
	if !settings.Valid() || nilLike(clock) {
		return nil, ErrSettings
	}
	amendment, err := scope.LoadEcobeeMVPV1()
	if err != nil {
		return nil, err
	}
	fields := amendment.Fields()
	if len(fields) != int(climateFieldCount) {
		return nil, ErrSettings
	}
	for index, field := range fields {
		rule := climateRules[index]
		if field.HAPType != rule.typ || field.Format != rule.format || field.Unit != rule.unit {
			return nil, ErrSettings
		}
	}
	return func() integrations.Pilot {
		return &Pilot{settings: settings, clock: clock, commands: make(chan commandJob, 32), stop: make(chan struct{}), done: make(chan struct{})}
	}, nil
}
func (p *Pilot) CommandDriver() integrations.CommandDriver { return p }
func (p *Pilot) Start(ctx context.Context, host integrations.SessionHost, tasks integrations.Tasks) error {
	if p == nil || !p.settings.Valid() || nilLike(p.clock) || nilLike(host) || nilLike(tasks) || nilLike(ctx) || p.stop == nil || p.done == nil || p.commands == nil {
		return ErrSettings
	}
	provider, err := host.Secrets()
	if err != nil {
		return err
	}
	value, err := provider.Read(ctx, p.settings.CredentialHandle())
	if err != nil {
		return err
	}
	credential, err := decodeCredential(value, p.settings)
	if err != nil {
		return err
	}
	api := client.NewAccessoryClient(func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != p.settings.Address() {
			return nil, ErrSettings
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}, &credential.controller, &credential.accessory)
	// Start is the only setup phase; all subsequent I/O belongs to one task.
	setup, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	accessories, err := api.Accessories(setup)
	if err != nil {
		api.Close()
		return ErrUnavailable
	}
	profile, err := admitProfile(accessories)
	if err != nil {
		api.Close()
		return err
	}
	if err := admitECB601(accessories); err != nil {
		api.Close()
		return err
	}
	candidate, err := integrations.NewCandidate(p.settings.SourceKey(), []*registry.CapabilityDescription{profile.description()})
	if err != nil {
		api.Close()
		return err
	}
	bindings, err := host.Discover(ctx, []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		api.Close()
		return ErrUnavailable
	}
	periodic, err := newProfilePoller(profile, pollClock{now: p.clock.Now, timeout: context.WithTimeout}, api.Characteristics)
	if err != nil {
		api.Close()
		return err
	}
	p.mu.Lock()
	p.api, p.host, p.profile, p.binding, p.running, p.periodic = api, host, profile, bindings[0], true, periodic
	p.mu.Unlock()
	if err := tasks.Go(p.run); err != nil {
		p.Stop(ctx)
		return err
	}
	return nil
}
func (p *Pilot) Stop(context.Context) error {
	if p == nil {
		return nil
	}
	if p.stop == nil {
		return ErrSettings
	}
	p.stopOnce.Do(func() { close(p.stop) })
	p.mu.Lock()
	api := p.api
	p.running = false
	p.mu.Unlock()
	if api != nil {
		return api.Close()
	}
	return nil
}
func (p *Pilot) run(ctx context.Context) error {
	defer close(p.done)
	defer p.Stop(context.Background())
	nextPoll := p.clock.Now()
	var tick <-chan time.Time
	for {
		// A due poll precedes another queued command. At most one command's bounded
		// PUT+GET (6s) can delay a periodic GET, preventing queue starvation.
		if !p.clock.Now().Before(nextPoll) {
			if err := ctx.Err(); err != nil {
				return nil
			}
			started := p.clock.Now()
			observation, err := p.periodic.poll(ctx)
			if err != nil {
				return err
			}
			if err := p.publish(ctx, observation, domain.Causality{}); err != nil {
				return err
			}
			nextPoll = started.Add(pollInterval)
			delay := nextPoll.Sub(p.clock.Now())
			if delay < 0 {
				delay = 0
			}
			tick = p.clock.After(delay)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-p.stop:
			if ctx.Err() != nil {
				return nil
			}
			return ErrUnavailable
		case job := <-p.commands:
			if job.ctx.Err() != nil {
				job.reply <- job.ctx.Err()
				continue
			}
			err := p.execute(job.ctx, job.command)
			job.reply <- err
			if errors.Is(err, ErrUnavailable) || errors.Is(err, ErrCommandUncertain) {
				return err
			}
		case <-tick:
		}
	}
}

func (p *Pilot) read(ctx context.Context) (climateObservation, error) {
	request, err := p.profile.readRequest()
	if err != nil {
		return climateObservation{}, err
	}
	operation, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	rows, err := p.api.Characteristics(operation, request)
	if err != nil {
		return climateObservation{}, ErrUnavailable
	}
	return p.profile.observe(rows, p.clock.Now())
}
func (p *Pilot) readAndPublish(ctx context.Context, causality domain.Causality) error {
	observation, err := p.read(ctx)
	if err != nil {
		return err
	}
	return p.publish(ctx, observation, causality)
}
func (p *Pilot) publish(ctx context.Context, observation climateObservation, causality domain.Causality) error {
	now := p.clock.Now()
	observedAt := now
	for _, field := range observation.values {
		if !field.at.IsZero() {
			observedAt = field.at
			break
		}
	}
	freshness := domain.FreshnessFresh
	if !now.Before(observedAt.Add(observationMaxAge)) {
		freshness = domain.FreshnessStale
	}
	value, err := observation.domainValue(observedAt)
	availability := domain.AvailabilityAvailable
	if err != nil {
		availability = domain.AvailabilityUnknown
		value = nil
	}
	quality, err := domain.NewQuality(availability, freshness, domain.ProvenanceObserved)
	if err != nil {
		return err
	}
	state, err := integrations.NewStateObservation(Endpoint, quality, value, &observedAt, now, nil)
	if err != nil {
		return err
	}
	// A normal periodic GET carries no command causality. GET command proof is qualified separately.
	if _, correlation, e := causality.Components(); e == nil && correlation != nil {
		state, err = state.WithCausality(causality)
		if err != nil {
			return err
		}
	}
	if err := p.host.Publish(ctx, p.binding, state); err != nil {
		return err
	}
	health := integrations.HealthReady
	detail := "ecobee local polling interval=15s timeout=3s expiry=45s latency<=24s including one PUT/readback"
	if _, kind := observation.activity(now); kind != valuePresent || observation.targetConflict(now) {
		health = integrations.HealthDegraded
		detail = "ecobee activity missing, invalid or inconsistent"
	}
	report, err := integrations.NewHealthReport(health, detail)
	if err != nil {
		return err
	}
	return p.host.ReportHealth(ctx, report)
}

func nilLike(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// PollingPolicy exposes design limits, not provider quotas or push guarantees.
type PollingPolicy struct{ Interval, Timeout, MaximumAge, MaximumObservationLatency time.Duration }

func PollingLimits() PollingPolicy {
	return PollingPolicy{pollInterval, pollTimeout, observationMaxAge, pollInterval + 3*pollTimeout}
}
