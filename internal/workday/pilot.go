package workday

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/damione1/Homeward/contracts"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

type BindingDefinition struct {
	alias domain.HAAlias
	key   integrations.SourceKey
}

func (d BindingDefinition) Alias() domain.HAAlias             { return d.alias }
func (d BindingDefinition) SourceKey() integrations.SourceKey { return d.key }

func InstanceID() (domain.IntegrationInstanceID, error) {
	return domain.ParseIntegrationInstanceID("homeward-workday-v1")
}

// The two aliases share one instance and source device. Only the binary
// source is discovered as a Host capability; the calendar is read-only.
func ConfiguredBindings() ([2]BindingDefinition, error) {
	var definitions [2]BindingDefinition
	for i, item := range []struct{ alias, entity string }{
		{"binary_sensor.workday_sensor", "binary"},
		{"calendar.workday_sensor_calendar", "calendar"},
	} {
		alias, err := domain.ParseHAAlias(item.alias)
		if err != nil {
			return definitions, err
		}
		key, err := integrations.NewSourceKey("computed-workday", item.entity)
		if err != nil {
			return definitions, err
		}
		definitions[i] = BindingDefinition{alias: alias, key: key}
	}
	return definitions, nil
}

type Pilot struct {
	settings Settings
	clock    automationtriggers.Clock
	mu       sync.Mutex
	started  bool
	cancel   context.CancelFunc
	timer    automationtriggers.Timer
}

func NewPilotFactory(settings Settings, clock automationtriggers.Clock) (integrations.PilotFactory, error) {
	if !settings.Valid() || nilInterface(clock) {
		return nil, failure(InvalidProfile)
	}
	return func() integrations.Pilot { return &Pilot{settings: settings, clock: clock} }, nil
}

func (p *Pilot) Start(ctx context.Context, host integrations.SessionHost, tasks integrations.Tasks) error {
	if p == nil || !p.settings.Valid() || nilInterface(p.clock) || nilInterface(host) || nilInterface(tasks) {
		return failure(InvalidProfile)
	}
	if err := validContext(ctx); err != nil {
		return err
	}
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return failure(InvalidProfile)
	}
	p.started = true
	generation, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.mu.Unlock()
	definitions, err := ConfiguredBindings()
	if err != nil {
		cancel()
		return err
	}
	description := &registrywire.CapabilityDescription{
		SchemaId: contracts.BinarySchema, Major: 1, DisplayName: "Workday",
		Endpoint: "value", Constraints: &registrywire.CapabilityDescription_Binary{Binary: &registrywire.BinaryConstraints{}},
	}
	candidate, err := integrations.NewCandidate(definitions[0].key, []*registrywire.CapabilityDescription{description})
	if err != nil {
		cancel()
		return err
	}
	bindings, err := host.Discover(generation, []integrations.Candidate{candidate})
	if err != nil {
		cancel()
		return err
	}
	instance, _ := InstanceID()
	if len(bindings) != 1 {
		cancel()
		return failure(InvalidIdentity)
	}
	boundInstance, boundKey, err := bindings[0].Source()
	if err != nil || boundInstance != instance || boundKey != definitions[0].key {
		cancel()
		return failure(InvalidIdentity)
	}
	if err := tasks.Go(func(taskCtx context.Context) error {
		defer cancel()
		return p.run(generation, taskCtx, host, bindings[0])
	}); err != nil {
		cancel()
		return err
	}
	return nil
}

func (p *Pilot) Stop(context.Context) error {
	if p == nil {
		return failure(InvalidProfile)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
	}
	if !nilInterface(p.timer) {
		p.timer.Stop()
		p.timer = nil
	}
	return nil
}

// nextBoundary finds the first change of civil date using the pinned zone.
// Stepping through a bounded 72h horizon also handles midnight transitions
// whose UTC offset changes; no fixed 24h duration is used as a local day.
func nextBoundary(zone calendars.Zone, now time.Time, today calendars.CivilDate) (time.Time, error) {
	start := now.UTC().Truncate(time.Second)
	for minute := 1; minute <= 72*60; minute++ {
		probe := start.Add(time.Duration(minute) * time.Minute)
		date, err := zone.LocalDate(probe)
		if err != nil {
			return time.Time{}, failure(InvalidDate)
		}
		if date == today {
			continue
		}
		lo := probe.Add(-time.Minute)
		hi := probe
		for hi.Sub(lo) > time.Second {
			mid := lo.Add(hi.Sub(lo) / 2).Truncate(time.Second)
			if mid.Equal(lo) {
				mid = lo.Add(time.Second)
			}
			candidate, err := zone.LocalDate(mid)
			if err != nil {
				return time.Time{}, failure(InvalidDate)
			}
			if candidate == today {
				lo = mid
			} else {
				hi = mid
			}
		}
		return hi, nil
	}
	return time.Time{}, failure(InvalidDate)
}

func (p *Pilot) run(ctx, taskCtx context.Context, host integrations.SessionHost, binding integrations.Binding) error {
	zone, err := p.settings.Zone()
	if err != nil {
		return err
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		return err
	}
	var last calendars.CivilDate
	var sequence uint64
	var boundary time.Time
	for {
		if ctx.Err() != nil || taskCtx.Err() != nil {
			return nil
		}
		now := p.clock.Now().UTC()
		date, err := zone.LocalDate(now)
		if err != nil {
			return failure(InvalidDate)
		}
		changed := date != last
		if changed {
			isWorkday, err := p.settings.IsWorkday(date)
			if err != nil {
				return err
			}
			if sequence == math.MaxUint64 {
				return failure(InvalidDate)
			}
			sequence++
			value := domain.NewSwitchValue(isWorkday)
			observation, err := integrations.NewStateObservation("value", quality, &value, &now, now, &sequence)
			if err != nil {
				return err
			}
			if ctx.Err() != nil || taskCtx.Err() != nil {
				return nil
			}
			if err := host.Publish(ctx, binding, observation); err != nil {
				if ctx.Err() != nil || taskCtx.Err() != nil {
					return nil
				}
				return err
			}
			last = date
		}
		if !boundary.After(now) || changed {
			boundary, err = nextBoundary(zone, now, date)
			if err != nil {
				return err
			}
		}
		// Short polls detect wall-clock jumps, while the exact boundary still
		// drives the normal transition. A repeated timer wake sees last==date.
		delay := boundary.Sub(p.clock.Now().UTC())
		if delay <= 0 {
			continue
		}
		if delay > time.Minute {
			delay = time.Minute
		}
		p.mu.Lock()
		if ctx.Err() != nil || taskCtx.Err() != nil {
			p.mu.Unlock()
			return nil
		}
		timer := p.clock.NewTimer(delay)
		if nilInterface(timer) {
			p.mu.Unlock()
			return failure(InvalidContext)
		}
		p.timer = timer
		p.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-taskCtx.Done():
		case <-timer.C():
		}
		timer.Stop()
		p.mu.Lock()
		p.timer = nil
		p.mu.Unlock()
	}
}
