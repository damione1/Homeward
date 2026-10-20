package nha045

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/contracts"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/reactivetemplate"
)

type timerRecord struct {
	ID         int   `json:"id"`
	DeadlineMS int64 `json:"deadline_ms"`
	Fired      bool  `json:"fired"`
	Stopped    bool  `json:"stopped"`
}
type manualClock struct {
	mu          sync.Mutex
	origin      time.Time
	elapsed     time.Duration
	civilOffset time.Duration
	timers      []*manualTimer
}
type manualTimer struct {
	owner   *manualClock
	record  timerRecord
	channel chan time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.origin.Add(c.elapsed + c.civilOffset)
}
func (c *manualClock) MonoNow() time.Duration { c.mu.Lock(); defer c.mu.Unlock(); return c.elapsed }
func (c *manualClock) NewTimer(d time.Duration) automationtriggers.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &manualTimer{owner: c, record: timerRecord{ID: len(c.timers) + 1, DeadlineMS: (c.elapsed + d).Milliseconds()}, channel: make(chan time.Time, 1)}
	if d < 0 || d > 24*time.Hour || len(c.timers) >= MaxFacts {
		return nil
	}
	c.timers = append(c.timers, t)
	if d == 0 {
		t.record.Fired = true
		t.channel <- c.origin.Add(c.elapsed + c.civilOffset)
	}
	return t
}
func (t *manualTimer) C() <-chan time.Time { return t.channel }
func (t *manualTimer) Stop() bool {
	t.owner.mu.Lock()
	defer t.owner.mu.Unlock()
	was := !t.record.Stopped && !t.record.Fired
	t.record.Stopped = true
	return was
}
func (c *manualClock) advance(at int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if at < c.elapsed.Milliseconds() || at > 86400000 {
		return fail(InvalidFixture, "nonmonotone advance")
	}
	c.elapsed = time.Duration(at) * time.Millisecond
	due := append([]*manualTimer(nil), c.timers...)
	sort.SliceStable(due, func(i, j int) bool { return due[i].record.DeadlineMS < due[j].record.DeadlineMS })
	for _, t := range due {
		if !t.record.Fired && !t.record.Stopped && t.record.DeadlineMS <= at {
			t.record.Fired = true
			t.channel <- c.origin.Add(c.elapsed + c.civilOffset)
		}
	}
	return nil
}
func (c *manualClock) records() []timerRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]timerRecord, 0, len(c.timers))
	for _, t := range c.timers {
		out = append(out, t.record)
	}
	return out
}

type reactiveClock struct{ base *manualClock }

func (c reactiveClock) Now() time.Time                                  { return c.base.Now() }
func (c reactiveClock) NewTimer(d time.Duration) reactivetemplate.Timer { return c.base.NewTimer(d) }

// This is the historical 868 constructor port, not an automation owner or a
// permission policy. Remove this mounting argument when the merged API does.
type baselineAuthority struct{ actor domain.ActorID }

func (a baselineAuthority) Context(ctx context.Context) (context.Context, domain.ActorID, error) {
	return ctx, a.actor, nil
}

type entityBinding struct {
	alias    string
	entity   domain.EntityID
	binding  integrations.Binding
	endpoint string
}
type fixtureResolver struct{ entities []entityBinding }

func (r fixtureResolver) ResolveEntity(alias domain.HAAlias) (domain.EntityID, error) {
	name, e := alias.Value()
	if e != nil {
		return domain.EntityID{}, e
	}
	for _, b := range r.entities {
		if b.alias == name {
			return b.entity, nil
		}
	}
	return domain.EntityID{}, automationtriggers.ErrInvalidPlan
}
func (fixtureResolver) ResolveDevice(ir.HADeviceRef) (domain.DeviceID, error) {
	return domain.DeviceID{}, automationtriggers.ErrInvalidPlan
}
func (fixtureResolver) ResolveCalendar(domain.HAAlias) (automationtriggers.CalendarSourceID, error) {
	return automationtriggers.CalendarSourceID{}, automationtriggers.ErrInvalidPlan
}

type observedAction struct {
	Raw, Entity, Source string
	AtMS                int64
	Failed              bool
	origin              nativeActionOrigin
}
type nativeActionOrigin struct {
	entry                automationruns.TraceOrdinal
	source               ir.Source
	run                  automationruns.RunID
	generation           automationruns.Generation
	call                 uint64
	branches, iterations []int
	valid                bool
}
type actionOccurrence struct {
	index   int
	present bool
}
type observedState struct {
	Entity, State, Raw string
	Brightness         *float64
	AtMS               int64
	ControlBefore      int
	ControlAfter       int
	cause              actionOccurrence
}
type controlMark struct {
	before int
	key    string
}
type runIdentity struct {
	key, raw, object string
	parent, callsite *string
	id               automationruns.RunID
}
type nativeRefusal struct{ decision automationruns.Decision }

func admitNativeRefusal(decision automationruns.Decision) (nativeRefusal, error) {
	refusal := nativeRefusal{decision}
	if _, e := refusal.outcome(); e != nil {
		return nativeRefusal{}, e
	}
	return refusal, nil
}

func (r nativeRefusal) outcome() (Outcome, error) {
	switch r.decision {
	case automationruns.DecisionSingleBusy:
		return SingleBusy, nil
	case automationruns.DecisionMaxExceeded:
		return MaxExceeded, nil
	case automationruns.DecisionInvalid, automationruns.DecisionPreparing,
		automationruns.DecisionAdmitted, automationruns.DecisionQueued,
		automationruns.DecisionRestartPending, automationruns.DecisionPreparationSaturated,
		automationruns.DecisionMemorySaturated, automationruns.DecisionConditionFalse,
		automationruns.DecisionPreparationError, automationruns.DecisionExecutionError,
		automationruns.DecisionFinished, automationruns.DecisionCancelled,
		automationruns.DecisionSuperseded, automationruns.DecisionClosed,
		automationruns.DecisionUnknownAutomation, automationruns.DecisionTriggerMissed,
		automationruns.DecisionTriggerDegraded:
		return OutcomeInvalid, fail(InvalidObservation, "native decision is not an admitted script refusal")
	default:
		return OutcomeInvalid, fail(InvalidObservation, "unknown native refusal decision")
	}
}

type rig struct {
	f            Fixture
	clock        *manualClock
	host         *integrations.Manager
	fixture      *fake.Fixture
	session      *integrations.Session
	entities     []entityBinding
	triggers     *automationtriggers.Service
	reactive     *reactivetemplate.Service
	controller   *automationruns.Controller
	interpreter  *automationruns.Interpreter
	scripts      map[string]ir.Script
	automations  []ir.Automation
	handles      map[string]*automationruns.ScriptHandle
	refusals     map[runKey]nativeRefusal
	identities   []runIdentity
	mu           sync.Mutex
	actions      []observedAction
	states       []observedState
	cancelled    []string
	controlMarks []controlMark
	phaseLog     []phase
	closed       bool
}

func rawID(g automationruns.Generation, id automationruns.RunID) string {
	return fmt.Sprintf("%d/%d", g, id)
}
func (r *rig) ExecuteCall(ctx context.Context, call ir.Call, execution automationruns.CommandExecution) error {
	if r == nil || ctx == nil || ctx.Err() != nil || call.Kind() != ir.CallSwitchTurnOn {
		return fail(InvalidObservation, "invalid synthetic command")
	}
	causal, e := execution.CausalKey()
	if e != nil {
		return e
	}
	parts := strings.SplitN(causal, "/", 5)
	if len(parts) != 5 {
		return fail(InvalidObservation, "causal key format changed")
	}
	for _, part := range parts[:3] {
		n, e := strconv.ParseUint(part, 10, 64)
		if e != nil || n == 0 {
			return fail(InvalidObservation, "invalid causal identity")
		}
	}
	if len(call.Targets()) != 1 {
		return fail(InvalidObservation, "045 action requires one exact target per native call")
	}
	origin, e := r.actionOrigin(execution, parts)
	if e != nil {
		return e
	}
	for _, target := range call.Targets() {
		entity, e := target.Alias().Value()
		if e != nil || !syntheticEntity(entity) {
			return fail(InvalidObservation, "non-synthetic target")
		}
		failed := entity == "switch.nha045_fail"
		r.mu.Lock()
		if len(r.actions) >= MaxFacts {
			r.mu.Unlock()
			return fail(CaptureOverflow, "actions full")
		}
		r.actions = append(r.actions, observedAction{Raw: parts[0] + "/" + parts[1], Entity: entity, Source: execution.Source().Path(), AtMS: r.clock.MonoNow().Milliseconds(), Failed: failed, origin: origin})
		occurrence := actionOccurrence{len(r.actions) - 1, true}
		if failed {
			r.mu.Unlock()
			return errors.New("nha045 synthetic command failure")
		}
		e = r.publishLocked(ctx, entity, "on", nil, parts[0]+"/"+parts[1], occurrence)
		r.mu.Unlock()
		if e != nil {
			return e
		}
	}
	return nil
}
func (r *rig) publish(ctx context.Context, alias, state string, brightness *float64, raw string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.publishLocked(ctx, alias, state, brightness, raw, actionOccurrence{})
}
func (r *rig) publishLocked(ctx context.Context, alias, state string, brightness *float64, raw string, cause actionOccurrence) error {
	before := 0
	if r.controller != nil {
		traces, loss := r.controller.Traces()
		if loss != 0 {
			return fail(CaptureOverflow, "input cut lost native control events")
		}
		before = len(traces)
	}
	var binding *entityBinding
	for i := range r.entities {
		if r.entities[i].alias == alias {
			binding = &r.entities[i]
			break
		}
	}
	if binding == nil {
		return fail(InvalidFixture, "unregistered state entity")
	}
	availability := domain.AvailabilityAvailable
	if state == "unknown" {
		availability = domain.AvailabilityUnknown
	}
	quality, e := domain.NewQuality(availability, domain.FreshnessFresh, domain.ProvenanceObserved)
	if e != nil {
		return e
	}
	var value *domain.StateValue
	if state != "unknown" {
		v := domain.NewSwitchValue(state == "on")
		if strings.HasPrefix(alias, "light.") {
			light, e := domain.NewLightValue(state == "on", brightness, nil)
			if e != nil {
				return e
			}
			v, e = domain.NewLightStateValue(light)
			if e != nil {
				return e
			}
		}
		value = &v
	}
	at := r.clock.Now()
	obs, e := integrations.NewStateObservation(binding.endpoint, quality, value, &at, at, nil)
	if e != nil {
		return e
	}
	if e = r.session.Publish(ctx, binding.binding, obs); e != nil {
		return e
	}
	actual, ok := r.fixture.State(binding.entity)
	if !ok {
		return fail(InvalidObservation, "Host state absent after publication")
	}
	actualState := "unknown"
	var actualBrightness *float64
	if v, ok := actual.CurrentValue(); ok {
		switch kind, e := v.Kind(); {
		case e != nil:
			return e
		case kind == domain.ValueSwitch:
			on, _ := v.Switch()
			actualState = "off"
			if on {
				actualState = "on"
			}
		case kind == domain.ValueLight:
			light, _ := v.Light()
			on, b, _, e := light.Components()
			if e != nil {
				return e
			}
			actualBrightness = b
			actualState = "off"
			if on {
				actualState = "on"
			}
		default:
			return fail(InvalidObservation, "unexpected state kind")
		}
	}
	for i := len(r.states) - 1; i >= 0; i-- {
		old := r.states[i]
		if old.Entity != alias {
			continue
		}
		sameBrightness := old.Brightness == nil && actualBrightness == nil || old.Brightness != nil && actualBrightness != nil && *old.Brightness == *actualBrightness
		if old.State == actualState && sameBrightness {
			return nil
		}
		break
	}
	after := 0
	if r.controller != nil {
		traces, loss := r.controller.Traces()
		if loss != 0 {
			return fail(CaptureOverflow, "input cut lost native control events")
		}
		after = len(traces)
	}
	if len(r.states) >= MaxFacts {
		return fail(CaptureOverflow, "states full")
	}
	r.states = append(r.states, observedState{Entity: alias, State: actualState, Raw: raw, Brightness: actualBrightness, AtMS: r.clock.MonoNow().Milliseconds(), ControlBefore: before, ControlAfter: after, cause: cause})
	return nil
}
func buildRig(ctx context.Context, f Fixture) (r *rig, err error) {
	if !f.valid || ctx == nil {
		return nil, fail(InvalidFixture, "invalid runtime input")
	}
	r = &rig{f: f, clock: &manualClock{origin: f.origin}, scripts: make(map[string]ir.Script), handles: make(map[string]*automationruns.ScriptHandle), refusals: make(map[runKey]nativeRefusal)}
	defer func() {
		if err != nil {
			err = errors.Join(err, r.close())
		}
	}()
	r.host, r.fixture, err = fake.NewCommandFixtureWithClock(r.clock)
	if err != nil {
		return r, err
	}
	instance, err := domain.ParseIntegrationInstanceID("nha045-synthetic")
	if err != nil {
		return r, err
	}
	r.session, err = r.host.OpenSession(instance)
	if err != nil {
		return r, err
	}
	var records []integrations.RegistryRecord
	var irEntities []ir.Entity
	for _, entity := range f.wire.Entities {
		key, e := integrations.NewSourceKey("nha045", entity.ID)
		if e != nil {
			return r, e
		}
		description := &registry.CapabilityDescription{SchemaId: contracts.SwitchSchema, Major: 1, DisplayName: entity.ID, Endpoint: "value", Constraints: &registry.CapabilityDescription_Switch{Switch: &registry.SwitchConstraints{}}}
		if strings.HasPrefix(entity.ID, "light.") {
			description = &registry.CapabilityDescription{SchemaId: contracts.LightSchema, Major: 1, DisplayName: entity.ID, Endpoint: "value", Constraints: &registry.CapabilityDescription_Light{Light: &registry.LightConstraints{BrightnessPercent: &registry.NumberBounds{Minimum: 0, Maximum: 100}}}}
		}
		candidate, e := integrations.NewCandidate(key, []*registry.CapabilityDescription{description})
		if e != nil {
			return r, e
		}
		bindings, e := r.session.Discover(ctx, []integrations.Candidate{candidate})
		if e != nil || len(bindings) != 1 {
			return r, errors.Join(e, fail(InvalidObservation, "discovery cardinality"))
		}
		device, id, e := bindings[0].IDs()
		if e != nil {
			return r, e
		}
		alias, e := domain.ParseHAAlias(entity.ID)
		if e != nil {
			return r, e
		}
		record, e := integrations.NewRegistryRecord(device, id, instance, key, alias, alias, []domain.HAAlias{alias}, false)
		if e != nil {
			return r, e
		}
		records = append(records, record)
		r.entities = append(r.entities, entityBinding{entity.ID, id, bindings[0], "value"})
		supports := []ir.DeclaredSupport{ir.SupportObserveState}
		if strings.HasPrefix(entity.ID, "switch.") {
			supports = append(supports, ir.SupportSwitchTurnOn)
		}
		ie, e := ir.NewEntity(entity.ID, ir.ReferencePresent, supports...)
		if e != nil {
			return r, e
		}
		irEntities = append(irEntities, ie)
	}
	if err = r.fixture.PublishRegistry(ctx, records); err != nil {
		return r, err
	}
	for _, entity := range f.wire.Entities {
		if err = r.publish(ctx, entity.ID, entity.State, entity.Brightness, ""); err != nil {
			return r, err
		}
	}
	event, err := ir.NewEvent("nha045_release", ir.ReferencePresent)
	if err != nil {
		return r, err
	}
	catalogue, err := ir.NewCatalogue(irEntities, nil, []ir.Event{event})
	if err != nil {
		return r, err
	}
	var scriptIDs []ir.ScriptID
	for _, o := range f.wire.Objects {
		if strings.HasPrefix(o.ID, "script.") {
			id, e := ir.ParseScriptID(o.ID)
			if e != nil {
				return r, e
			}
			scriptIDs = append(scriptIDs, id)
		}
	}
	catalogue, err = catalogue.WithScripts(scriptIDs)
	if err != nil {
		return r, err
	}
	var programs []ir.Script
	for _, o := range f.wire.Objects {
		source, e := parser.NewSourceFile(o.ID+".yaml", []byte(o.YAML))
		if e != nil {
			return r, e
		}
		if strings.HasPrefix(o.ID, "script.") {
			result, e := ir.CompileScriptSources(o.ID, o.ID+".yaml", []parser.SourceFile{source}, catalogue)
			if e != nil || len(result.Accepted()) != 1 || len(result.Rejected()) != 0 {
				return r, errors.Join(e, fail(InvalidFixture, fmt.Sprintf("script compile %s: %+v", o.ID, result.Rejected())))
			}
			program := result.Accepted()[0]
			r.scripts[o.ID] = program
			programs = append(programs, program)
		} else {
			result, e := ir.CompileSources(o.ID, o.ID+".yaml", []parser.SourceFile{source}, catalogue)
			if e != nil || len(result.Accepted()) != 1 || len(result.Rejected()) != 0 {
				return r, errors.Join(e, fail(InvalidFixture, fmt.Sprintf("automation compile %s: %+v", o.ID, result.Rejected())))
			}
			r.automations = append(r.automations, result.Accepted()[0])
		}
	}
	triggerPlan, err := automationtriggers.NewPlan(automationtriggers.PlanInput{Automations: r.automations, Scripts: programs, Zone: f.wire.Zone, Resolver: fixtureResolver{r.entities}})
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
	reactivePlan, err := reactivetemplate.NewPlan(nil, nil, f.zone)
	if err != nil {
		return r, err
	}
	reactivePlan, err = reactivePlan.WithScriptWaits(f.zone)
	if err != nil {
		return r, err
	}
	r.reactive, err = reactivetemplate.Start(ctx, r.fixture.Sequencer(), reactivePlan, reactiveClock{r.clock})
	if err != nil {
		return r, err
	}
	plan, err := automationruns.NewPlanWithScripts(r.automations, programs)
	if err != nil {
		return r, err
	}
	actor, err := domain.ParseActorID("nha045-baseline-constructor-port")
	if err != nil {
		return r, err
	}
	r.interpreter, err = automationruns.NewInterpreter(automationruns.InterpreterInput{Plan: plan, TriggerWaits: r.triggers, TemplateWaits: r.reactive, Conditions: r.reactive, Authority: baselineAuthority{actor}, Commands: r, Clock: r.clock, Zone: f.zone})
	if err != nil {
		return r, err
	}
	var reader automationruns.ResultReader
	if len(r.automations) > 0 {
		reader = r.triggers
	}
	r.controller, err = automationruns.NewController(plan, reader, r.interpreter, r.interpreter)
	if err != nil {
		return r, err
	}
	err = r.controller.Start(ctx)
	return r, err
}
func (r *rig) close() error {
	if r == nil || r.closed {
		return nil
	}
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
	r.closed = true
	if e := errors.Join(errs...); e != nil {
		return fail(CleanupFailed, e.Error())
	}
	return nil
}
func await(ctx context.Context, predicate func() bool) error {
	if missingContext(ctx) || predicate == nil {
		return fail(InvalidFixture, "nil barrier")
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for !predicate() {
		select {
		case <-ctx.Done():
			return fail(HarnessTimeout, ctx.Err().Error())
		case <-deadline.C:
			return fail(HarnessTimeout, "observed phase barrier not reached")
		default:
			runtime.Gosched()
		}
	}
	return nil
}
func missingContext(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	v := reflect.ValueOf(ctx)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}
func (r *rig) flush(ctx context.Context) error {
	// Existing 040 registration drains the admitted journal prefix under its
	// mutex; no extra NextResult reader consumes the controller's stream.
	for _, script := range r.scripts {
		if waits := script.Waits(); len(waits) > 0 {
			handle, e := r.triggers.ObserveWait(ctx, waits[0].Source())
			if e != nil {
				return e
			}
			select {
			case <-handle.Close():
			case <-ctx.Done():
				return ctx.Err()
			}
			break
		}
	}
	literal, e := template.Compile("{{ true }}")
	if e != nil {
		return e
	}
	_, e = r.reactive.EvaluateCondition(ctx, literal, nil)
	return e
}
func (r *rig) runIdentity(id automationruns.RunID) (runIdentity, bool) {
	for _, v := range r.identities {
		if v.id == id {
			return v, true
		}
	}
	return runIdentity{}, false
}
func (r *rig) syncIdentities() error {
	traces, evicted := r.controller.Traces()
	if evicted != 0 {
		return fail(CaptureOverflow, "controller traces evicted")
	}
	nodes, evicted := r.interpreter.NodeTraces()
	if evicted != 0 {
		return fail(CaptureOverflow, "node traces evicted")
	}
	for _, trace := range traces {
		if trace.RunID == 0 {
			continue
		}
		if _, present := r.runIdentity(trace.RunID); present {
			continue
		}
		object := ""
		if id, e := trace.Script.Value(); e == nil {
			object = id
		} else {
			object = "automation." + trace.Automation
		}
		raw := rawID(trace.Generation, trace.RunID)
		if trace.Generation == 0 {
			for _, n := range nodes {
				if n.RunID() == trace.RunID {
					raw = rawID(n.Generation(), n.RunID())
					break
				}
			}
		}
		if object == "script.nha045_child" {
			if len(r.handles) != 1 {
				return fail(InvalidObservation, "ambiguous child attribution")
			}
			parent := ""
			for key := range r.handles {
				parent = key
			}
			for _, known := range r.identities {
				if known.object == object {
					return fail(InvalidObservation, "multiple child runs outside corpus")
				}
			}
			r.identities = append(r.identities, runIdentity{parent + "/child/1", raw, object, ptr(parent), ptr("child/1"), trace.RunID})
		} else if strings.HasPrefix(object, "automation.") {
			for _, known := range r.identities {
				if known.object == object {
					return fail(InvalidObservation, "multiple automation roots outside corpus")
				}
			}
			r.identities = append(r.identities, runIdentity{"auto/1", raw, object, nil, nil, trace.RunID})
		}
	}
	return nil
}
func (r *rig) step(ctx context.Context, p phase) error {
	if _, e := p.boundary(); e != nil {
		return fail(InvalidFixture, e.Error())
	}
	_, advances := p.operation.(advancePhase)
	if !advances && r.clock.MonoNow().Milliseconds() != p.atMS {
		return fail(InvalidFixture, "phase lacks an explicit matching clock advance")
	}
	switch v := p.operation.(type) {
	case advancePhase:
		previous := r.clock.records()
		finished := func() int {
			traces, _ := r.controller.Traces()
			count := 0
			for _, tr := range traces {
				switch tr.Decision {
				case automationruns.DecisionFinished, automationruns.DecisionCancelled, automationruns.DecisionExecutionError:
					count++
				}
			}
			return count
		}
		before := finished()
		if e := r.clock.advance(p.atMS); e != nil {
			return e
		}
		after := r.clock.records()
		due := 0
		for index, timer := range after {
			if timer.Fired && (index >= len(previous) || !previous[index].Fired) {
				due++
			}
		}
		// Each due timer in this deliberately bounded corpus leads to a terminal
		// run (delay, aborting wait or state-for). Observe that actual terminal;
		// never advance past a callback whose effects have not been collected.
		if due > 0 {
			if e := await(ctx, func() bool { return finished() >= before+due }); e != nil {
				return e
			}
		}
	case invokePhase:
		program := r.scripts[v.object.value]
		handle, e := r.controller.InvokeScript(ctx, program.ID(), nil, automationruns.Run{}, ir.ScriptCallDetached)
		if e != nil {
			var refused *automationruns.AdmissionError
			if !errors.As(e, &refused) {
				return e
			}
			refusal, admissionError := admitNativeRefusal(refused.Decision())
			if admissionError != nil {
				return admissionError
			}
			if len(r.refusals) >= MaxPhases {
				return fail(CaptureOverflow, "native refusals full")
			}
			r.refusals[v.run] = refusal
			r.handles[v.run.value] = nil
			break
		}
		r.handles[v.run.value] = handle
		traces, _ := r.controller.Traces()
		found := false
		for _, trace := range traces {
			if trace.RunID == handle.ID() && trace.Generation != 0 {
				r.identities = append(r.identities, runIdentity{v.run.value, rawID(trace.Generation, trace.RunID), v.object.value, nil, nil, handle.ID()})
				found = true
				break
			}
		}
		if !found {
			return fail(InvalidObservation, "admitted handle has no raw generation")
		}
	case startupPhase:
		if h := r.handles[v.run.value]; h != nil {
			barrier, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			_, e := h.Startup(barrier)
			if e != nil {
				return e
			}
			if r.scripts[v.object.value].Mode() == ir.AutomationQueued && p.atMS >= 5000 {
				if e = await(ctx, func() bool {
					for _, timer := range r.clock.records() {
						if timer.DeadlineMS == p.atMS+5000 && !timer.Fired && !timer.Stopped {
							return true
						}
					}
					return false
				}); e != nil {
					return e
				}
			}
		}
	case waitingPhase:
		if e := await(ctx, func() bool {
			nodes, _ := r.interpreter.NodeTraces()
			for _, n := range nodes {
				if n.Source().Object() == v.object.value && n.Stage() == automationruns.NodeWaiting {
					return true
				}
			}
			return false
		}); e != nil {
			return e
		}
	case terminalPhase:
		if h := r.handles[v.run.value]; h != nil {
			if e := await(ctx, func() bool {
				select {
				case <-h.Done():
					return true
				default:
					return false
				}
			}); e != nil {
				return e
			}
		}
	case runTerminalPhase:
		if e := await(ctx, func() bool {
			if e := r.syncIdentities(); e != nil {
				return false
			}
			traces, _ := r.controller.Traces()
			for _, id := range r.identities {
				if id.object != v.object.value {
					continue
				}
				for _, trace := range traces {
					if trace.RunID == id.id && (trace.Decision == automationruns.DecisionFinished || trace.Decision == automationruns.DecisionCancelled || trace.Decision == automationruns.DecisionExecutionError) {
						return true
					}
				}
			}
			return false
		}); e != nil {
			return e
		}
	case cancelPhase:
		if h := r.handles[v.run.value]; h != nil {
			traces, _ := r.controller.Traces()
			r.controlMarks = append(r.controlMarks, controlMark{len(traces), v.run.value + "/cancel"})
			r.cancelled = append(r.cancelled, v.run.value)
			h.Cancel()
		}
	case publishPhase:
		entity, state, brightness, e := publicationInputs(v.state)
		if e != nil {
			return fail(InvalidFixture, e.Error())
		}
		if e := r.publish(ctx, entity, state, brightness, ""); e != nil {
			return e
		}
		if e := r.flush(ctx); e != nil {
			return e
		}
	case emitPhase:
		event, e := integrations.NewDeviceEvent("nha045_release", r.clock.Now(), nil)
		if e != nil {
			return e
		}
		if len(r.entities) == 0 {
			return fail(InvalidFixture, "event has no Host binding")
		}
		if e = r.session.Emit(ctx, r.entities[0].binding, event); e != nil {
			return e
		}
		if e = r.flush(ctx); e != nil {
			return e
		}
	case actionsPhase:
		if e := await(ctx, func() bool { r.mu.Lock(); defer r.mu.Unlock(); return len(r.actions) >= v.count }); e != nil {
			return e
		}
	case nil:
		return fail(InvalidFixture, "absent phase")
	default:
		return fail(InvalidFixture, "unhandled phase")
	}
	r.phaseLog = append(r.phaseLog, p)
	return r.syncIdentities()
}

type rawNode struct {
	Ordinal    uint64 `json:"ordinal"`
	Run        string `json:"run"`
	Object     string `json:"object"`
	Path       string `json:"path"`
	Kind       uint8  `json:"kind"`
	Stage      uint8  `json:"stage"`
	Failure    uint8  `json:"failure"`
	Branches   []int  `json:"branches"`
	Iterations []int  `json:"iterations"`
}
type cleanupEvidence struct {
	ControllerDone bool `json:"controller_done"`
	TriggersDone   bool `json:"triggers_done"`
	TemplatesDone  bool `json:"templates_done"`
	SessionDone    bool `json:"session_done"`
	TimersTerminal bool `json:"timers_terminal"`
}
type rawCapture struct {
	Producer string              `json:"producer"`
	Order    orderProofWire      `json:"order"`
	Phases   []phaseWire         `json:"phases"`
	Timers   []timerRecord       `json:"timers"`
	Nodes    []rawNode           `json:"nodes"`
	Actions  []observedAction    `json:"actions"`
	States   []observedState     `json:"states"`
	Commands []nativeCommandWire `json:"commands"`
	Network  NetworkEvidence     `json:"network"`
	Cleanup  cleanupEvidence     `json:"cleanup"`
}
type positionedFact struct {
	scope, key string
	position   int
}

func (r *rig) capture(network NetworkEvidence) (Capture, error) {
	if e := r.syncIdentities(); e != nil {
		return Capture{}, e
	}
	observation, e := r.observeNative()
	if e != nil {
		return Capture{}, e
	}
	if e = r.businessTerminals(observation); e != nil {
		return Capture{}, e
	}
	if e = r.close(); e != nil {
		return Capture{}, e
	}
	final, e := r.observeNative()
	if e != nil {
		return Capture{}, e
	}
	if e = stableNativePrefix(observation, final); e != nil {
		return Capture{}, e
	}
	return r.captureNative(network, final)
}

// captureNative is shared by the real capture and the native-boundary oracles.
// Both paths require the same owned, joined native evidence before projection.
func (r *rig) captureNative(network NetworkEvidence, observation nativeObservation) (Capture, error) {
	if e := r.qualifyNative(observation, true); e != nil {
		return Capture{}, e
	}
	traces, nodes := observation.decisions, observation.nodes
	controlMarks := append([]controlMark(nil), r.controlMarks...)
	source, e := executableSource()
	if e != nil {
		return Capture{}, e
	}
	w := captureWire{Schema: Schema, Scenario: r.f.ID(), FixtureSHA256: r.f.SHA256(), Runtime: runtimeWire{Name: "go", Version: runtime.Version(), SourceSHA256: []sourceWire{source}}, Runs: []runWire{}, Facts: []factWire{}, Edges: []edgeWire{}, Orders: []streamWire{}, Terminal: ptr(true)}
	phaseEvidence := make([]phaseWire, 0, len(r.phaseLog))
	for _, p := range r.phaseLog {
		boundary, e := p.boundary()
		if e != nil {
			return Capture{}, fail(InvalidObservation, e.Error())
		}
		phaseEvidence = append(phaseEvidence, boundary)
	}
	producer := fmt.Sprintf("go-controller/%p", r.controller)
	order := orderProofWire{Producer: producer, Healthy: true, Joined: true, Points: []orderPointWire{}, StateCauses: []stateCauseWire{}}
	point := func(key string, position automationruns.TraceOrdinal) error {
		value, valid := position.Value()
		if !valid {
			return fail(InvalidObservation, "fact native position absent")
		}
		order.Points = append(order.Points, orderPointWire{key, value})
		return nil
	}
	raw := rawCapture{Producer: producer, Phases: phaseEvidence, Timers: r.clock.records(), Nodes: []rawNode{}, Commands: []nativeCommandWire{}, Network: network}
	byRaw := make(map[string]runIdentity)
	byID := make(map[automationruns.RunID]runIdentity)
	for _, id := range r.identities {
		w.Runs = append(w.Runs, runWire{id.key, id.raw, id.parent, id.callsite, id.object})
		byRaw[id.raw] = id
		byID[id.id] = id
	}
	add := func(kind FactKind, key, raw, object, node string, tick int64, data dataWire) {
		run := (*string)(nil)
		if raw != "" {
			run = ptr(raw)
		}
		w.Facts = append(w.Facts, factWire{key, factName(kind), run, tick, object, node, data})
	}
	terminalTimes := make(map[automationruns.RunID]int64)
	started := make(map[automationruns.RunID]bool)
	waits := make(map[automationruns.RunID]int)
	var positions []positionedFact
	scope := func(id string, branches []int) string {
		value := "node/" + id + "/root"
		for _, branch := range branches {
			value += "/" + strconv.Itoa(branch)
		}
		return value
	}
	for nodeIndex, n := range nodes {
		id, ok := byID[n.RunID()]
		if !ok {
			return Capture{}, fail(InvalidObservation, "unattributed node run")
		}
		at := n.At().Sub(r.f.origin).Milliseconds()
		terminalTimes[n.RunID()] = max(terminalTimes[n.RunID()], at)
		ordinal, valid := n.Ordinal().Value()
		if !valid {
			return Capture{}, fail(InvalidObservation, "raw native node position absent")
		}
		raw.Nodes = append(raw.Nodes, rawNode{Ordinal: ordinal, Run: id.raw, Object: n.Source().Object(), Path: n.Source().Path(), Kind: uint8(n.Kind()), Stage: uint8(n.Stage()), Failure: uint8(n.Failure()), Branches: n.Branches(), Iterations: n.Iterations()})
		if !started[n.RunID()] {
			add(Started, id.key+"/start", id.raw, id.object, "entry", at, dataWire{})
			started[n.RunID()] = true
		}
		value, present, e := n.WaitValue()
		if e != nil {
			return Capture{}, e
		}
		if !present {
			continue
		}
		waits[n.RunID()]++
		outcome := Completed
		switch n.WaitExit() {
		case automationruns.WaitCompleted:
		case automationruns.WaitStoppedByTimeout:
			outcome = TimeoutStopped
		case automationruns.WaitCancelled:
			outcome = Cancelled
		case automationruns.WaitContinuedAfterTimeout:
			return Capture{}, fail(InvalidObservation, "unexpected continuing timeout")
		case automationruns.WaitSourceFailed, automationruns.WaitExitInvalid:
			return Capture{}, fail(InvalidObservation, "invalid wait exit")
		default:
			return Capture{}, fail(InvalidObservation, "unhandled wait exit")
		}
		projected, e := projectWait(value)
		if e != nil {
			return Capture{}, e
		}
		key := fmt.Sprintf("%s/wait/%d", id.key, waits[n.RunID()])
		add(Wait, key, id.raw, id.object, fmt.Sprintf("wait/%d", waits[n.RunID()]), at, dataWire{Outcome: ptr(outcomeName(outcome)), Wait: &projected})
		positions = append(positions, positionedFact{scope(id.key, n.Branches()), key, nodeIndex*2 + 1})
		if e := point(key, n.Ordinal()); e != nil {
			return Capture{}, e
		}
	}
	admissions := make(map[string]bool)
	for _, p := range r.phaseLog {
		invocation, invoked := p.operation.(invokePhase)
		if !invoked {
			continue
		}
		handle := r.handles[invocation.run.value]
		if handle == nil {
			refusal, present := r.refusals[invocation.run]
			if !present {
				return Capture{}, fail(InvalidObservation, "invocation lost its native refusal")
			}
			decision, e := refusal.outcome()
			if e != nil {
				return Capture{}, e
			}
			add(Admission, invocation.run.value+"/admission", "", invocation.object.value, "admission", p.atMS, dataWire{Outcome: ptr(outcomeName(decision))})
			continue
		}
		id := byID[handle.ID()]
		outcome := Accepted
		for _, tr := range traces {
			if tr.RunID == handle.ID() && tr.Decision == automationruns.DecisionQueued {
				outcome = Queued
				break
			}
		}
		add(Admission, id.key+"/admission", id.raw, id.object, "admission", p.atMS, dataWire{Outcome: ptr(outcomeName(outcome))})
		admissions[id.raw] = true
	}
	for _, id := range r.identities {
		if !admissions[id.raw] {
			add(Admission, id.key+"/admission", id.raw, id.object, "admission", terminalTimes[id.id], dataWire{Outcome: ptr(outcomeName(Accepted))})
			if id.parent != nil {
				for _, p := range r.identities {
					if p.key == *id.parent {
						for i := range w.Facts {
							if w.Facts[i].Key == id.key+"/admission" {
								w.Facts[i].AtMS = 0
							}
						}
					}
				}
			}
		}
	}
	r.mu.Lock()
	actions := append([]observedAction(nil), r.actions...)
	states := append([]observedState(nil), r.states...)
	r.mu.Unlock()
	raw.Actions = actions
	raw.States = states
	actionKeys := make([]string, len(actions))
	usedCalls := make(map[nativeCallKey]bool, len(actions))
	ordinals := make(map[string]int)
	for actionIndex, a := range actions {
		id, ok := byRaw[a.Raw]
		if !ok {
			return Capture{}, fail(InvalidObservation, "orphan action")
		}
		slot := id.key + "/action/" + a.Entity
		ordinals[slot]++
		outcome := Completed
		if a.Failed {
			outcome = CommandFailed
		}
		key := fmt.Sprintf("%s/%d", slot, ordinals[slot])
		actionKeys[actionIndex] = key
		add(Action, key, a.Raw, id.object, fmt.Sprintf("%s/%d", a.Entity, ordinals[slot]), a.AtMS, dataWire{Outcome: ptr(outcomeName(outcome)), Entity: ptr(a.Entity), Service: ptr("switch.turn_on")})
		issueIndex, e := actionIssue(a, nodes, usedCalls)
		if e != nil {
			return Capture{}, e
		}
		n := nodes[issueIndex]
		var entryOrdinal uint64
		entryValid := false
		for _, entry := range nodes {
			if entry.NodeTrace.Ordinal() == a.origin.entry {
				entryOrdinal, entryValid = entry.Ordinal().Value()
			}
		}
		issueOrdinal, issueValid := n.Ordinal().Value()
		if !entryValid || !issueValid {
			return Capture{}, fail(InvalidObservation, "command native witness absent")
		}
		raw.Commands = append(raw.Commands, nativeCommandWire{Key: key, Run: a.Raw, Object: a.origin.source.Object(), Source: a.origin.source.Path(), Call: a.origin.call, Entry: entryOrdinal, Issue: issueOrdinal, Branches: append([]int{}, a.origin.branches...), Iterations: append([]int{}, a.origin.iterations...)})
		positions = append(positions, positionedFact{scope(id.key, n.Branches()), key, issueIndex * 2})
		if e := point(key, n.Ordinal()); e != nil {
			return Capture{}, e
		}
	}
	stateOrdinals := make(map[string]int)
	for _, s := range states {
		stateOrdinals[s.Entity]++
		key := fmt.Sprintf("state/%s/%d", s.Entity, stateOrdinals[s.Entity])
		add(State, key, s.Raw, "house", "state", s.AtMS, dataWire{Entity: ptr(s.Entity), State: ptr(s.State), Brightness: s.Brightness})
		if s.Raw != "" {
			if !s.cause.present || s.cause.index < 0 || s.cause.index >= len(actionKeys) {
				return Capture{}, fail(InvalidObservation, "effect lacks its actual command occurrence")
			}
			order.StateCauses = append(order.StateCauses, stateCauseWire{key, actionKeys[s.cause.index]})
		}
		if s.Raw == "" {
			if s.ControlBefore < 0 || s.ControlAfter < s.ControlBefore || s.ControlAfter > len(traces) {
				return Capture{}, fail(InvalidObservation, "input control cuts invalid")
			}
			if r.f.ID() == "09-detached-parent-cancel" && key == "state/switch.nha045_gate/2" {
				for index := s.ControlBefore; index < s.ControlAfter; index++ {
					tr := traces[index]
					if tr.Decision == automationruns.DecisionFinished || tr.Decision == automationruns.DecisionCancelled || tr.Decision == automationruns.DecisionExecutionError {
						return Capture{}, fail(InvalidObservation, "selected input/terminal order is ambiguous inside publication cuts")
					}
				}
			}
			controlMarks = append(controlMarks, controlMark{s.ControlAfter, key})
		}
	}
	terminalIDs := make(map[automationruns.RunID]bool)
	for _, tr := range traces {
		id, ok := byID[tr.RunID]
		if !ok {
			continue
		}
		outcome := OutcomeInvalid
		switch tr.Decision {
		case automationruns.DecisionFinished:
			outcome = Finished
		case automationruns.DecisionCancelled:
			outcome = Cancelled
		case automationruns.DecisionExecutionError:
			synthetic := false
			typed := false
			for _, a := range actions {
				if a.Raw == id.raw && a.Failed && a.Entity == "switch.nha045_fail" {
					synthetic = true
				}
			}
			for _, n := range nodes {
				if n.RunID() == tr.RunID && n.Failure() == automationruns.ExecutionCommandError {
					typed = true
				}
			}
			if !synthetic || !typed {
				return Capture{}, fail(InvalidObservation, "unexpected interpreter execution error: "+tr.Error)
			}
			outcome = CommandFailed
		default:
			continue
		}
		if terminalIDs[tr.RunID] {
			continue
		}
		terminalIDs[tr.RunID] = true
		for _, n := range nodes {
			if n.RunID() == tr.RunID && n.WaitExit() == automationruns.WaitStoppedByTimeout {
				outcome = TimeoutStopped
			}
		}
		add(Terminal, id.key+"/terminal", id.raw, id.object, "terminal", terminalTimes[tr.RunID], dataWire{Outcome: ptr(outcomeName(outcome))})
		if e := point(id.key+"/terminal", tr.Ordinal()); e != nil {
			return Capture{}, e
		}
	}
	for _, id := range r.identities {
		if !terminalIDs[id.id] {
			return Capture{}, fail(InvalidObservation, "capture before actual terminal")
		}
	}
	for _, key := range r.cancelled {
		for _, id := range r.identities {
			if id.key == key {
				at := terminalTimes[id.id]
				add(Cancellation, key+"/cancel", id.raw, id.object, "cancel", at, dataWire{})
			}
		}
	}
	control := streamWire{Scope: "control", Keys: []string{}}
	seenControl := make(map[string]bool)
	appendControl := func(key string) {
		if !seenControl[key] {
			control.Keys = append(control.Keys, key)
			seenControl[key] = true
		}
	}
	for index := 0; index <= len(traces); index++ {
		for _, mark := range controlMarks {
			if mark.before == index {
				appendControl(mark.key)
			}
		}
		if index == len(traces) {
			break
		}
		tr := traces[index]
		id, ok := byID[tr.RunID]
		if !ok {
			continue
		}
		switch tr.Decision {
		case automationruns.DecisionAdmitted:
			appendControl(id.key + "/start")
			if e := point(id.key+"/start", tr.Ordinal()); e != nil {
				return Capture{}, e
			}
		case automationruns.DecisionFinished, automationruns.DecisionCancelled, automationruns.DecisionExecutionError:
			appendControl(id.key + "/terminal")
		}
	}
	w.Orders = append(w.Orders, control)
	sort.SliceStable(positions, func(i, j int) bool { return positions[i].position < positions[j].position })
	streams := make(map[string]int)
	for _, v := range positions {
		index, ok := streams[v.scope]
		if !ok {
			index = len(w.Orders)
			streams[v.scope] = index
			w.Orders = append(w.Orders, streamWire{Scope: v.scope, Keys: []string{}})
		}
		w.Orders[index].Keys = append(w.Orders[index].Keys, v.key)
	}
	raw.Order = order
	rawBytes, e := json.Marshal(raw)
	if e != nil {
		return Capture{}, e
	}
	w.Raw = rawBytes
	admitted, e := admitCapture(w)
	if e != nil {
		return Capture{}, e
	}
	proof, e := decodeOrderProof(w.Raw, admitted)
	if e != nil {
		return Capture{}, e
	}
	w.Edges, e = observedCausalEdges(w, r.f, proof)
	if e != nil {
		return Capture{}, e
	}
	encoded, e := json.Marshal(w)
	if e != nil {
		return Capture{}, e
	}
	return DecodeCapture(encoded)
}
func projectWait(value template.Value) (waitWire, error) {
	completed, present := value.Field("completed")
	if !present {
		return waitWire{}, fail(InvalidObservation, "wait.completed absent")
	}
	boolean, ok := completed.Bool()
	if !ok {
		return waitWire{}, fail(InvalidObservation, "wait.completed not bool")
	}
	out := waitWire{Completed: ptr(boolean)}
	remaining, present := value.Field("remaining")
	if !present {
		return out, fail(InvalidObservation, "wait.remaining absent")
	}
	kind, e := remaining.Kind()
	if e != nil {
		return out, e
	}
	if kind != template.KindNone {
		seconds, ok := remaining.Float()
		if !ok || math.IsNaN(seconds) || math.IsInf(seconds, 0) || math.Abs(seconds*1000-math.Round(seconds*1000)) > 0.000001 {
			return out, fail(InvalidObservation, "wait.remaining resolution")
		}
		out.RemainingMS = ptr(int64(math.Round(seconds * 1000)))
	}
	if trigger, present := value.Field("trigger"); present {
		kind, e := trigger.Kind()
		if e != nil {
			return out, e
		}
		if kind != template.KindNone {
			id, p := trigger.Field("id")
			if !p {
				return out, fail(InvalidObservation, "trigger.id absent")
			}
			idText, ok := id.StringValue()
			if !ok {
				return out, fail(InvalidObservation, "trigger.id type")
			}
			index, p := trigger.Field("idx")
			if !p {
				return out, fail(InvalidObservation, "trigger.idx absent")
			}
			indexText, ok := index.StringValue()
			if !ok {
				return out, fail(InvalidObservation, "trigger.idx type")
			}
			n, e := strconv.Atoi(indexText)
			if e != nil {
				return out, e
			}
			platform, p := trigger.Field("platform")
			if !p {
				return out, fail(InvalidObservation, "trigger.platform absent")
			}
			platformText, ok := platform.StringValue()
			if !ok {
				return out, fail(InvalidObservation, "trigger.platform type")
			}
			out.Trigger = &triggerWire{idText, n, platformText}
		}
	}
	return out, nil
}
func Run(ctx context.Context, f Fixture) (capture Capture, err error) {
	if missingContext(ctx) || !f.valid {
		return Capture{}, fail(InvalidFixture, "nil context or zero fixture")
	}
	network, e := InspectNetwork()
	if e != nil {
		return Capture{}, e
	}
	r, e := buildRig(ctx, f)
	if e != nil {
		return Capture{}, e
	}
	defer func() { err = errors.Join(err, r.close()) }()
	for _, phase := range f.phases {
		if e = r.step(ctx, phase); e != nil {
			return Capture{}, fmt.Errorf("%s: %w", phase.id.value, e)
		}
	}
	if e = errors.Join(r.controller.Error(), r.triggers.Error(), r.reactive.Error()); e != nil {
		return Capture{}, fail(InvalidObservation, "runtime observation source failed: "+e.Error())
	}
	capture, e = r.capture(network)
	if e != nil {
		return Capture{}, e
	}
	if e = r.close(); e != nil {
		return Capture{}, e
	}
	done := func(channel <-chan struct{}) bool {
		if channel == nil {
			return false
		}
		select {
		case <-channel:
			return true
		default:
			return false
		}
	}
	cleanup := cleanupEvidence{done(r.controller.Done()), done(r.triggers.Done()), done(r.reactive.Done()), done(r.session.Done()), true}
	for _, timer := range r.clock.records() {
		if !timer.Fired && !timer.Stopped {
			cleanup.TimersTerminal = false
		}
	}
	if !cleanup.ControllerDone || !cleanup.TriggersDone || !cleanup.TemplatesDone || !cleanup.SessionDone || !cleanup.TimersTerminal {
		return Capture{}, fail(CleanupFailed, "runtime resource terminal evidence incomplete")
	}
	boundary, e := capture.boundary()
	if e != nil {
		return Capture{}, e
	}
	var raw rawCapture
	if e = json.Unmarshal(boundary.Raw, &raw); e != nil {
		return Capture{}, e
	}
	raw.Cleanup = cleanup
	raw.Timers = r.clock.records()
	boundary.Raw, e = json.Marshal(raw)
	if e != nil {
		return Capture{}, e
	}
	encoded, e := json.Marshal(boundary)
	if e != nil {
		return Capture{}, e
	}
	return DecodeCapture(encoded)
}
