package solar

import (
	"context"
	"fmt"
	"github.com/damione1/Homeward/contracts"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	solarwire "github.com/damione1/Homeward/gen/home/solar/v1"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"math"
	"reflect"
	"sync"
	"time"
)

const publicationBudget = 250 * time.Millisecond
const observationCount = domain.SolarEventCount + 1

type BindingDefinition struct {
	alias domain.HAAlias
	key   integrations.SourceKey
	event domain.SolarEventKind
}

func (d BindingDefinition) Alias() domain.HAAlias             { return d.alias }
func (d BindingDefinition) SourceKey() integrations.SourceKey { return d.key }

func InstanceID() (domain.IntegrationInstanceID, error) {
	return domain.ParseIntegrationInstanceID("homeward-sun-v1")
}

// Events precede the aggregate observation. This is an ordered sequence of
// seven observations, not an atomic transaction across seven entities.
func ConfiguredBindings() ([observationCount]BindingDefinition, error) {
	var definitions [observationCount]BindingDefinition
	for i, event := range domain.SolarEventKinds() {
		name, err := event.Name()
		if err != nil {
			return definitions, err
		}
		alias, err := domain.ParseHAAlias("sensor.sun_next_" + name)
		if err != nil {
			return definitions, err
		}
		key, err := integrations.NewSourceKey("computed-sun", "next_"+name)
		if err != nil {
			return definitions, err
		}
		definitions[i] = BindingDefinition{alias: alias, key: key, event: event}
	}
	alias, err := domain.ParseHAAlias("sun.sun")
	if err != nil {
		return definitions, err
	}
	key, err := integrations.NewSourceKey("computed-sun", "sun")
	if err != nil {
		return definitions, err
	}
	definitions[domain.SolarEventCount] = BindingDefinition{alias: alias, key: key}
	return definitions, nil
}

func (d BindingDefinition) description() (*registrywire.CapabilityDescription, error) {
	if _, err := d.alias.Value(); err != nil {
		return nil, err
	}
	if d.event == domain.SolarEventUnspecified {
		return &registrywire.CapabilityDescription{SchemaId: contracts.SolarSchema, Major: 1, DisplayName: "Local sun", Endpoint: "value", Constraints: &registrywire.CapabilityDescription_Solar{Solar: &solarwire.SolarConstraints{}}}, nil
	}
	kind, err := contracts.WireSolarEventKind(d.event)
	if err != nil {
		return nil, err
	}
	name, _ := d.event.Name()
	return &registrywire.CapabilityDescription{SchemaId: contracts.SolarEventSchema, Major: 1, DisplayName: "Next solar " + name, Endpoint: "value", Constraints: &registrywire.CapabilityDescription_SolarEvent{SolarEvent: &solarwire.SolarEventConstraints{Event: kind}}}, nil
}

func nilRuntimeInterface(value any) bool {
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

type Pilot struct {
	settings *Settings
	clock    automationtriggers.Clock
	mu       sync.Mutex
	started  bool
	cancel   context.CancelFunc
	timer    automationtriggers.Timer
}

func (*Pilot) Format(out fmt.State, _ rune) { _, _ = out.Write([]byte("solar.Pilot(<private>)")) }

func NewPilotFactory(settings Settings, clock automationtriggers.Clock) (integrations.PilotFactory, error) {
	if !settings.Valid() || nilRuntimeInterface(clock) {
		return nil, failure(InvalidSettings)
	}
	return func() integrations.Pilot { return &Pilot{settings: &settings, clock: clock} }, nil
}

func (p *Pilot) Start(ctx context.Context, host integrations.SessionHost, tasks integrations.Tasks) error {
	if p == nil || p.settings == nil || !p.settings.Valid() || contextError(ctx) != nil || nilRuntimeInterface(host) || nilRuntimeInterface(tasks) || nilRuntimeInterface(p.clock) {
		return failure(InvalidContext)
	}
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return failure(InvalidSettings)
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
	candidates := make([]integrations.Candidate, 0, observationCount)
	for _, definition := range definitions {
		description, err := definition.description()
		if err != nil {
			cancel()
			return err
		}
		candidate, err := integrations.NewCandidate(definition.key, []*registrywire.CapabilityDescription{description})
		if err != nil {
			cancel()
			return err
		}
		candidates = append(candidates, candidate)
	}
	bindings, err := host.Discover(generation, candidates)
	if err != nil {
		cancel()
		return err
	}
	if len(bindings) != observationCount {
		cancel()
		return failure(InvalidSettings)
	}
	instance, _ := InstanceID()
	seen := make(map[domain.EntityID]bool, observationCount)
	for i, binding := range bindings {
		boundInstance, key, err := binding.Source()
		_, entity, identityErr := binding.IDs()
		if err != nil || identityErr != nil || boundInstance != instance || key != definitions[i].key || seen[entity] {
			cancel()
			return failure(InvalidSettings)
		}
		seen[entity] = true
	}
	calculator, err := NewCalculator(*p.settings)
	if err != nil {
		cancel()
		return err
	}
	if err := tasks.Go(func(taskCtx context.Context) error {
		// The generation derives from Start's supervisor-owned context. Stop
		// cancels it as well; no detached calculation or timer owns work.
		defer cancel()
		return p.run(generation, taskCtx, host, bindings, calculator)
	}); err != nil {
		cancel()
		return err
	}
	return nil
}

func (p *Pilot) Stop(context.Context) error {
	if p == nil {
		return failure(InvalidSettings)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
	}
	if !nilRuntimeInterface(p.timer) {
		p.timer.Stop()
		p.timer = nil
	}
	return nil
}

func futureSnapshot(value domain.SolarValue, captured, deadline, now time.Time) bool {
	if now.Before(captured) || !deadline.After(now) {
		return false
	}
	input, err := value.Components()
	if err != nil {
		return false
	}
	for _, occurrence := range input.Next {
		at, _, kind, err := occurrence.Components()
		if err != nil || kind == domain.SolarOccurrencePresent && !at.After(now) {
			return false
		}
	}
	return true
}

func (p *Pilot) computation(ctx context.Context, calculator *Calculator) (Computation, error) {
	for attempt := 0; attempt < 2; attempt++ {
		now, start := p.clock.Now().UTC(), p.clock.MonoNow()
		computed, err := calculator.Compute(ctx, now)
		if err != nil {
			return Computation{}, err
		}
		elapsed := p.clock.MonoNow() - start
		if elapsed < 0 || elapsed > publicationBudget {
			return Computation{}, failure(BudgetExceeded)
		}
		value, captured, _, deadline, err := computed.Components()
		if err != nil {
			return Computation{}, err
		}
		if futureSnapshot(value, captured, deadline, p.clock.Now().UTC()) {
			return computed, nil
		}
	}
	return Computation{}, failure(CalculationFailed)
}

func (p *Pilot) run(ctx, taskCtx context.Context, host integrations.SessionHost, bindings []integrations.Binding, calculator *Calculator) error {
	var sequence uint64
	for {
		if err := contextError(ctx); err != nil {
			return nil
		}
		if taskCtx.Err() != nil {
			return nil
		}
		computed, err := p.computation(ctx, calculator)
		if err != nil {
			if ctx.Err() != nil || taskCtx.Err() != nil {
				return nil
			}
			return err
		}
		value, captured, _, deadline, err := computed.Components()
		if err != nil {
			return err
		}
		input, _ := value.Components()
		var values [observationCount]domain.StateValue
		for i, kind := range domain.SolarEventKinds() {
			event, err := domain.NewSolarEventValue(kind, input.Next[i])
			if err != nil {
				return err
			}
			values[i], err = domain.NewSolarEventStateValue(event)
			if err != nil {
				return err
			}
		}
		values[domain.SolarEventCount], err = domain.NewSolarStateValue(value)
		if err != nil {
			return err
		}
		if sequence > math.MaxUint64-observationCount {
			return failure(CalculationFailed)
		}
		quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		if err != nil {
			return err
		}
		for i := range values {
			if ctx.Err() != nil || taskCtx.Err() != nil {
				return nil
			}
			now := p.clock.Now().UTC()
			if !futureSnapshot(value, captured, deadline, now) {
				return failure(CalculationFailed)
			}
			sequence++
			observation, err := integrations.NewStateObservation("value", quality, &values[i], &captured, now, &sequence)
			if err != nil {
				return err
			}
			if err := host.Publish(ctx, bindings[i], observation); err != nil {
				return err
			}
		}
		if position, present, err := computed.NextPosition(); err != nil {
			return err
		} else if present && position.Before(deadline) {
			deadline = position
		}
		delay := deadline.Sub(p.clock.Now().UTC())
		if delay <= 0 {
			return failure(CalculationFailed)
		}
		p.mu.Lock()
		if ctx.Err() != nil || taskCtx.Err() != nil {
			p.mu.Unlock()
			return nil
		}
		timer := p.clock.NewTimer(delay)
		if nilRuntimeInterface(timer) {
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
