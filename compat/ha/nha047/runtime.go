package nha047

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/nha045"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/contracts"
	registryv1 "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/reactivetemplate"
	"github.com/damione1/Homeward/internal/solar"
)

// The clock drives delivered engines; it does not evaluate HA constructions.
type qualificationClock struct {
	mu      sync.Mutex
	origin  time.Time
	elapsed time.Duration
	timers  []*qualificationTimer
}
type qualificationTimer struct {
	clock          *qualificationClock
	at             time.Duration
	ch             chan time.Time
	fired, stopped bool
}

func (c *qualificationClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.origin.Add(c.elapsed)
}
func (c *qualificationClock) MonoNow() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.elapsed
}
func (c *qualificationClock) NewTimer(duration time.Duration) automationtriggers.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	if duration < 0 || duration > 24*time.Hour || len(c.timers) >= MaxFacts {
		return nil
	}
	timer := &qualificationTimer{clock: c, at: c.elapsed + duration, ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, timer)
	if duration == 0 {
		timer.fired = true
		timer.ch <- c.origin.Add(c.elapsed)
	}
	return timer
}
func (t *qualificationTimer) C() <-chan time.Time { return t.ch }
func (t *qualificationTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	active := !t.stopped && !t.fired
	t.stopped = true
	return active
}
func (c *qualificationClock) advance(at time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if at < c.elapsed || at > 24*time.Hour {
		return ErrInvalidInput
	}
	c.elapsed = at
	for _, timer := range c.timers {
		if !timer.fired && !timer.stopped && timer.at <= at {
			timer.fired = true
			timer.ch <- c.origin.Add(at)
		}
	}
	return nil
}
func (c *qualificationClock) registered() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers) > 0
}
func (c *qualificationClock) terminal() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, timer := range c.timers {
		if !timer.fired && !timer.stopped {
			return false
		}
	}
	return true
}

type qualificationReactiveClock struct{ clock *qualificationClock }

func (c qualificationReactiveClock) Now() time.Time { return c.clock.Now() }
func (c qualificationReactiveClock) NewTimer(d time.Duration) reactivetemplate.Timer {
	return c.clock.NewTimer(d)
}

type qualificationResolver map[domain.HAAlias]domain.EntityID

func (r qualificationResolver) ResolveEntity(id domain.HAAlias) (domain.EntityID, error) {
	value, ok := r[id]
	if !ok {
		return domain.EntityID{}, automationtriggers.ErrInvalidPlan
	}
	return value, nil
}
func (qualificationResolver) ResolveDevice(ir.HADeviceRef) (domain.DeviceID, error) {
	return domain.DeviceID{}, automationtriggers.ErrInvalidPlan
}
func (qualificationResolver) ResolveCalendar(domain.HAAlias) (automationtriggers.CalendarSourceID, error) {
	return automationtriggers.CalendarSourceID{}, automationtriggers.ErrInvalidPlan
}

type demandObserver struct {
	mu    sync.Mutex
	facts []Demand
}

func (o *demandObserver) ExecuteCall(ctx context.Context, call ir.Call, execution automationruns.CommandExecution) error {
	if ctx == nil || ctx.Err() != nil || len(call.Targets()) != 1 {
		return ErrInvalidCapture
	}
	if _, err := execution.CausalKey(); err != nil {
		return err
	}
	target, err := call.Targets()[0].Alias().Value()
	if err != nil {
		return err
	}
	var demand Demand
	switch call.Kind() {
	case ir.CallSwitchTurnOn:
		demand, err = NewDemand(SwitchOn, target, 0, 0, "")
	case ir.CallClimateSetTemperature:
		value := call.Temperature()
		if !value.Present() {
			return ErrInvalidCapture
		}
		demand, err = NewDemand(ClimateTemperature, target, value.Value(), 0, "")
	default:
		return ErrUnavailable
	}
	if err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.facts) >= MaxFacts {
		return ErrInvalidCapture
	}
	o.facts = append(o.facts, demand)
	return nil
}
func (o *demandObserver) snapshot() []Demand {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]Demand(nil), o.facts...)
}

// consumed is selected by the caller once. This exact snapshot reaches the
// delivered compiler and, later, EncodeCapture; the loader never reselects it.
func compileConsumed(c Case, consumed parser.SourceFile, catalogue ir.Catalogue) (ir.Automation, error) {
	result, err := ir.CompileSources(c.Object(), consumed.Path(), []parser.SourceFile{consumed}, catalogue)
	if err != nil {
		return ir.Automation{}, err
	}
	accepted := result.Accepted()
	if len(accepted) != 1 || len(result.Rejected()) != 0 {
		return ir.Automation{}, ErrInvalidInput
	}
	return accepted[0], nil
}

type automationQualification struct {
	clock       *qualificationClock
	host        *integrations.Manager
	session     *integrations.Session
	fixture     *fake.Fixture
	triggers    *automationtriggers.Service
	reactive    *reactivetemplate.Service
	controller  *automationruns.Controller
	interpreter *automationruns.Interpreter
	commands    demandObserver
	closed      bool
	deadline    time.Duration
}

func (r *automationQualification) close() error {
	if r == nil || r.closed {
		return nil
	}
	r.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var errs []error
	if r.controller != nil {
		errs = append(errs, r.controller.Close(ctx))
	}
	if r.triggers != nil {
		errs = append(errs, r.triggers.Close())
	}
	if r.reactive != nil {
		errs = append(errs, r.reactive.Close(ctx))
	}
	if r.host != nil && r.session != nil {
		errs = append(errs, r.host.Revoke(r.session))
	}
	if r.clock != nil && !r.clock.terminal() {
		errs = append(errs, ErrInvalidCapture)
	}
	return errors.Join(errs...)
}

func newAutomationQualification(ctx context.Context, c Case, consumed parser.SourceFile) (r *automationQualification, err error) {
	zone, err := time.LoadLocation("America/Toronto")
	if err != nil {
		return nil, err
	}
	r = &automationQualification{clock: &qualificationClock{origin: time.Date(2024, 3, 11, 0, 0, 0, 0, zone)}}
	defer func() {
		if err != nil {
			err = errors.Join(err, r.close())
		}
	}()
	r.host, r.fixture, err = fake.NewCommandFixtureWithClock(r.clock)
	if err != nil {
		return r, err
	}
	instance, err := domain.ParseIntegrationInstanceID("nha047-synthetic")
	if err != nil {
		return r, err
	}
	r.session, err = r.host.OpenSession(instance)
	if err != nil {
		return r, err
	}
	resolver := qualificationResolver{}
	var entities []ir.Entity
	var records []integrations.RegistryRecord
	for _, spec := range []struct {
		id string
		on bool
	}{{"switch.nha047_value", false}, {"switch.nha047_season", c.Season()}, {"switch.nha047_workday", c.Workday()}} {
		key, e := integrations.NewSourceKey("nha047", spec.id)
		if e != nil {
			return r, e
		}
		description := &registryv1.CapabilityDescription{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "value", DisplayName: spec.id, Constraints: &registryv1.CapabilityDescription_Switch{Switch: &registryv1.SwitchConstraints{SetOn: true}}}
		candidate, e := integrations.NewCandidate(key, []*registryv1.CapabilityDescription{description})
		if e != nil {
			return r, e
		}
		bindings, e := r.session.Discover(ctx, []integrations.Candidate{candidate})
		if e != nil || len(bindings) != 1 {
			return r, errors.Join(e, ErrInvalidCapture)
		}
		device, id, e := bindings[0].IDs()
		if e != nil {
			return r, e
		}
		alias, e := domain.ParseHAAlias(spec.id)
		if e != nil {
			return r, e
		}
		record, e := integrations.NewRegistryRecord(device, id, instance, key, alias, alias, []domain.HAAlias{alias}, false)
		if e != nil {
			return r, e
		}
		records = append(records, record)
		resolver[alias] = id
		entity, e := ir.NewEntity(spec.id, ir.ReferencePresent, ir.SupportObserveState, ir.SupportSwitchTurnOn)
		if e != nil {
			return r, e
		}
		entities = append(entities, entity)
		quality, e := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		if e != nil {
			return r, e
		}
		value := domain.NewSwitchValue(spec.on)
		at := r.clock.Now()
		observation, e := integrations.NewStateObservation("value", quality, &value, &at, at, nil)
		if e != nil {
			return r, e
		}
		if e = r.session.Publish(ctx, bindings[0], observation); e != nil {
			return r, e
		}
	}
	if err = r.fixture.PublishRegistry(ctx, records); err != nil {
		return r, err
	}
	climate, err := ir.NewEntity("climate.nha047_value", ir.ReferencePresent, ir.SupportClimateSetpoint)
	if err != nil {
		return r, err
	}
	entities = append(entities, climate)
	catalogue, err := ir.NewCatalogue(entities, nil, nil)
	if err != nil {
		return r, err
	}
	automation, err := compileConsumed(c, consumed, catalogue)
	if err != nil {
		return r, err
	}
	triggerInput := automationtriggers.PlanInput{Automations: []ir.Automation{automation}, Zone: zone.String(), Resolver: resolver}
	if c.Scenario() == SunSwitch {
		zero := 0.0
		settings, e := solar.ParseSettings(solar.Input{Latitude: &zero, Longitude: &zero, ElevationMeters: &zero, Timezone: zone.String()})
		if e != nil {
			return r, e
		}
		triggerInput, err = solar.ConfigureTriggerInput(settings, triggerInput)
		if err != nil {
			return r, err
		}
		sunset, e := triggerInput.SunSource.NextSunset(ctx, r.clock.Now(), triggerInput.Zone, triggerInput.SolarPosition)
		if e != nil {
			return r, e
		}
		at, availability, e := sunset.Components()
		if e != nil || availability != automationtriggers.SunPresent {
			return r, ErrUnavailable
		}
		r.deadline = at.Add(-time.Hour).Sub(r.clock.Now())
	}
	triggerPlan, err := automationtriggers.NewPlan(triggerInput)
	if err != nil {
		return r, err
	}
	r.triggers, err = automationtriggers.NewService(triggerPlan, r.fixture.Sequencer(), r.clock)
	if err != nil {
		return r, err
	}
	if err = r.triggers.Start(ctx); err != nil {
		return r, err
	}
	reactivePlan, err := reactivetemplate.NewPlan(nil, nil, zone)
	if err != nil {
		return r, err
	}
	reactivePlan, err = reactivePlan.WithScriptWaits(zone)
	if err != nil {
		return r, err
	}
	r.reactive, err = reactivetemplate.Start(ctx, r.fixture.Sequencer(), reactivePlan, qualificationReactiveClock{r.clock})
	if err != nil {
		return r, err
	}
	plan, err := automationruns.NewPlan([]ir.Automation{automation})
	if err != nil {
		return r, err
	}
	r.interpreter, err = automationruns.NewInterpreter(automationruns.InterpreterInput{Plan: plan, TriggerWaits: r.triggers, TemplateWaits: r.reactive, Conditions: r.reactive, Commands: &r.commands, Clock: r.clock, Zone: zone})
	if err != nil {
		return r, err
	}
	r.controller, err = automationruns.NewController(plan, r.triggers, r.interpreter, r.interpreter)
	if err != nil {
		return r, err
	}
	err = r.controller.Start(ctx)
	return r, err
}

func awaitQualification(ctx context.Context, condition func() (bool, error)) error {
	watchdog := time.NewTimer(5 * time.Second)
	defer watchdog.Stop()
	for {
		ready, err := condition()
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-watchdog.C:
			return ErrUnavailable
		default:
			runtime.Gosched()
		}
	}
}

func runConsumedAutomation(ctx context.Context, c Case, consumed parser.SourceFile) (facts []Demand, err error) {
	r, err := newAutomationQualification(ctx, c, consumed)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, r.close()) }()
	if err = awaitQualification(ctx, func() (bool, error) { return r.clock.registered(), r.triggers.Error() }); err != nil {
		return nil, err
	}
	deadline := r.deadline
	if c.Scenario() != SunSwitch {
		at, e := time.Parse("15:04:05", c.At())
		if e != nil {
			return nil, ErrInvalidInput
		}
		deadline = time.Duration(at.Hour())*time.Hour + time.Duration(at.Minute())*time.Minute + time.Duration(at.Second())*time.Second
	}
	if err = r.clock.advance(deadline); err != nil {
		return nil, err
	}
	if err = awaitQualification(ctx, func() (bool, error) {
		traces, loss := r.controller.Traces()
		if loss != 0 {
			return false, ErrInvalidCapture
		}
		for _, trace := range traces {
			switch trace.Decision {
			case automationruns.DecisionFinished, automationruns.DecisionConditionFalse:
				return true, nil
			case automationruns.DecisionExecutionError, automationruns.DecisionPreparationError:
				return false, ErrInvalidCapture
			}
		}
		return false, errors.Join(r.controller.Error(), r.triggers.Error(), r.reactive.Error())
	}); err != nil {
		return nil, err
	}
	facts = r.commands.snapshot()
	return facts, nil
}

func Run(ctx context.Context, c Case) (Capture, error) {
	if ctx == nil || ctx.Err() != nil || !c.Valid() || runtime.Version() != GoVersion {
		return Capture{}, ErrInvalidInput
	}
	network, err := nha045.InspectNetwork()
	if err != nil || !network.LoopbackOnly {
		return Capture{}, errors.Join(err, ErrUnavailable)
	}
	input := c.YAML()
	if c.Candidate() != "" {
		input = c.Candidate()
	}
	consumed, err := parser.NewSourceFile("qualification.yaml", []byte(input))
	if err != nil {
		return Capture{}, err
	}
	var facts []Demand
	switch c.Scenario() {
	case LegacyAction, TimeClimate, SunSwitch:
		facts, err = runConsumedAutomation(ctx, c, consumed)
	case SceneProjection:
		facts, err = runConsumedScene(ctx, c, consumed)
	default:
		err = ErrUnavailable
	}
	if err != nil {
		return Capture{}, err
	}
	sources, err := probeSources()
	if err != nil {
		return Capture{}, err
	}
	encoded, err := EncodeCapture(c, "go", runtime.Version(), consumed, sources, facts, network.LoopbackOnly, true)
	if err != nil {
		return Capture{}, err
	}
	return DecodeCapture(encoded)
}
