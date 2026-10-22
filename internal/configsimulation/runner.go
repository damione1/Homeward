package configsimulation

import (
	"context"
	"crypto/sha256"
	"reflect"
	"sync"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	hascene "github.com/damione1/Homeward/compat/ha/scene"
	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/configplans"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/sequencer"
)

// Run constructs every runtime dependency locally. The caller supplies values,
// never an implementation capable of connecting to an integration or a driver.
func Run(ctx context.Context, input Input) (report Report, resultErr error) {
	if !simulationContext(ctx) || !input.Candidate.Valid() || !input.Snapshot.valid || input.Zone == nil || input.VirtualLimit <= 0 || input.VirtualLimit > MaxVirtualDuration {
		return Report{}, ErrInvalid
	}
	if _, err := template.NewInputs(input.Snapshot.states, nil, input.Now, input.Zone); err != nil {
		return Report{}, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	report = Report{status: Completed}
	if input.Candidate.Family() == configplans.Scene {
		return runScene(ctx, input, report)
	}
	var automations []ir.Automation
	var scripts []ir.Script
	var actions []ir.Action
	var rejected []ir.Diagnostic
	files := input.Candidate.Files()
	switch input.Candidate.Family() {
	case configplans.Script:
		id, err := ir.ParseScriptID(input.Candidate.Identity())
		if err != nil {
			return failed(report, InvalidSource, Position{}), nil
		}
		catalogue, err := input.Snapshot.catalogue.WithScripts([]ir.ScriptID{id})
		if err != nil {
			return failed(report, InvalidSource, Position{}), nil
		}
		result, err := ir.CompileScriptSources(input.Candidate.Identity(), input.Candidate.Entry(), files, catalogue)
		if err != nil {
			return failed(report, InvalidSource, Position{}), nil
		}
		scripts, rejected = result.Accepted(), result.Rejected()
		if len(scripts) == 1 {
			actions = scripts[0].Actions()
		}
	case configplans.Automation:
		result, err := ir.CompileSources(input.Candidate.Identity(), input.Candidate.Entry(), files, input.Snapshot.catalogue)
		if err != nil {
			return failed(report, InvalidSource, Position{}), nil
		}
		automations, rejected = result.Accepted(), result.Rejected()
		if len(automations) == 1 {
			actions = automations[0].Actions()
			// Only one civil-time trigger is advanced. We do not synthesize an
			// admitted Result or claim state/MQTT/calendar trigger coverage.
			triggers := automations[0].Triggers()
			if len(triggers) != 1 || triggers[0].Kind() != ir.TriggerTime {
				return failed(report, UnsupportedExtension, sourcePosition(automations[0].Source())), nil
			}
		}
	case configplans.Operational, configplans.Declarative1, configplans.Declarative2, configplans.Authorization, configplans.Ownership, configplans.Dashboard, configplans.FamilyInvalid:
		return failed(report, UnsupportedExtension, Position{}), nil
	default:
		return Report{}, ErrInvalid
	}
	if len(rejected) != 0 {
		for _, diagnostic := range rejected {
			code := InvalidSource
			switch diagnostic.Code() {
			case ir.CodeUnknownField, ir.CodeUnsupportedForm, ir.CodeUnknownService, ir.CodeUnsupportedTemplate:
				code = UnsupportedExtension
			case ir.CodeUnknownReference, ir.CodeExcludedReference, ir.CodeMissingCapability:
				code = MissingFixture
			case ir.CodeInvalid, ir.CodeInvalidInput, ir.CodeInvalidValue:
			default:
			}
			report.diagnostics = append(report.diagnostics, Diagnostic{Code: code, Position: sourcePosition(diagnostic.Source())})
		}
		report.status = Failed
		return report, nil
	}
	if len(scripts)+len(automations) != 1 {
		return failed(report, InvalidSource, Position{}), nil
	}
	if diagnostic, present := unsupported(actions); present {
		return failed(report, diagnostic.Code, diagnostic.Position), nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	clock := &fixtureClock{start: input.Now.UTC(), limit: input.VirtualLimit, cancel: cancel}
	// Internal budget cancellation can race a successful flowStop or a
	// context.Canceled result. Its public failure is one canonical diagnostic.
	// Later cleanup defers run first, so even early failures are joined here.
	defer func() {
		clock.mu.Lock()
		defer clock.mu.Unlock()
		if resultErr == nil && clock.exhausted {
			report.status = Failed
			report.elapsed = clock.elapsed
			report.diagnostics = []Diagnostic{{Code: CapacityExceeded}}
		}
	}()
	conditions := &fixtureConditions{snapshot: input.Snapshot, clock: clock, zone: input.Zone}
	commands := &commandRecorder{snapshot: input.Snapshot, clock: clock}
	plan, err := automationruns.NewPlanWithScripts(automations, scripts)
	if err != nil {
		return failed(report, CapacityExceeded, Position{}), nil
	}
	triggerPlan, err := automationtriggers.NewPlan(automationtriggers.PlanInput{Automations: automations, Scripts: scripts, Zone: input.Zone.String(), Resolver: fixtureResolver{}})
	if err != nil {
		return failed(report, UnsupportedWait, Position{}), nil
	}
	sequence, err := sequencer.New(clock, 1024)
	if err != nil {
		return Report{}, ErrInvalid
	}
	if _, err := sequence.Start(); err != nil {
		return Report{}, ErrInvalid
	}
	// Script event waits have no service timers. For an automation this façade
	// fires the first time trigger only, leaving later recurrences unexercised.
	triggerClock := &firstTriggerClock{base: clock}
	triggers, err := automationtriggers.NewService(triggerPlan, sequence, triggerClock)
	if err != nil {
		return failed(report, ExecutionFailed, Position{}), nil
	}
	if err := triggers.Start(runCtx); err != nil {
		return failed(report, ExecutionFailed, Position{}), nil
	}
	defer func() { _ = triggers.Close() }()
	interpreter, err := automationruns.NewInterpreter(automationruns.InterpreterInput{Plan: plan, Conditions: conditions, Commands: commands, Clock: clock, Zone: input.Zone, TriggerWaits: triggers, TriggerPlan: triggerPlan})
	if err != nil {
		return failed(report, UnsupportedWait, Position{}), nil
	}
	var reader automationruns.ResultReader
	if len(automations) != 0 {
		reader = triggers
	}
	controller, err := automationruns.NewController(plan, reader, interpreter, interpreter)
	if err != nil {
		return failed(report, ExecutionFailed, Position{}), nil
	}
	defer func() { _ = controller.Close(context.Background()) }()
	observer := newTerminalObserver(actions)
	if err := controller.AttachHistoryObservationSink(observer); err != nil {
		return failed(report, ExecutionFailed, Position{}), nil
	}
	if err := controller.Start(runCtx); err != nil {
		return failed(report, ExecutionFailed, Position{}), nil
	}
	if len(scripts) != 0 {
		handle, err := controller.InvokeScript(runCtx, scripts[0].ID(), nil, automationruns.Run{}, ir.ScriptCallDirect)
		if err != nil {
			return failed(report, ExecutionFailed, Position{}), nil
		}
		if err := handle.Result(runCtx); err != nil {
			report.status = Failed
			report.diagnostics = append(report.diagnostics, Diagnostic{Code: ExecutionFailed})
		}
	} else {
		select {
		case decision := <-observer.done:
			if decision != automationruns.DecisionFinished && decision != automationruns.DecisionConditionFalse {
				report = failed(report, ExecutionFailed, Position{})
			}
		case <-runCtx.Done():
			report = failed(report, ExecutionFailed, Position{})
		}
	}
	// Close joins the run, all observation callbacks and image retirement.
	// The observer owns its bounded trace copies independently of that image.
	if err := controller.Close(context.Background()); err != nil {
		report = failed(report, ExecutionFailed, Position{})
	}
	nodes, incomplete := observer.nodeSnapshot()
	report.nodes = nodes
	if incomplete {
		report = failed(report, CapacityExceeded, Position{})
	}
	commands.mu.Lock()
	report.intents = append([]Intent(nil), commands.intents...)
	report.diagnostics = append(report.diagnostics, commands.diagnostics...)
	commands.mu.Unlock()
	clock.mu.Lock()
	report.elapsed = clock.elapsed
	clock.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	return report, nil
}

func simulationContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	value := reflect.ValueOf(ctx)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !value.IsNil()
	default:
		return true
	}
}

func failed(report Report, code DiagnosticCode, at Position) Report {
	report.status = Failed
	report.diagnostics = append(report.diagnostics, Diagnostic{Code: code, Position: at})
	return report
}

func unsupported(actions []ir.Action) (Diagnostic, bool) {
	for _, action := range actions {
		code := DiagnosticInvalid
		switch action.Kind() {
		case ir.ActionParallel:
			code = UnsupportedConcurrency
		case ir.ActionWaitTemplate:
			code = UnsupportedWait
		case ir.ActionWait:
			if _, hasTimeout := action.TimeoutMillis(); !hasTimeout {
				code = UnsupportedWait
			}
			for _, wait := range action.Triggers() {
				if wait.Kind() != ir.TriggerEvent {
					code = UnsupportedWait
				}
			}
		case ir.ActionCall:
			if action.Call().Kind() == ir.CallScript || action.Call().Kind() == ir.CallSceneTurnOn {
				code = UnsupportedExtension
			}
		case ir.ActionVariables, ir.ActionSequence, ir.ActionChoose, ir.ActionIf, ir.ActionRepeat, ir.ActionStop, ir.ActionDelay:
		case ir.ActionInvalid:
			code = InvalidSource
		default:
			code = InvalidSource
		}
		if code != DiagnosticInvalid {
			return Diagnostic{Code: code, Position: sourcePosition(action.Source())}, true
		}
		if diagnostic, present := unsupported(action.Children()); present {
			return diagnostic, true
		}
		if diagnostic, present := unsupported(action.Otherwise()); present {
			return diagnostic, true
		}
		for _, branch := range action.Branches() {
			if diagnostic, present := unsupported(branch.Actions()); present {
				return diagnostic, true
			}
		}
	}
	return Diagnostic{}, false
}

type fixtureConditions struct {
	snapshot Snapshot
	clock    *fixtureClock
	zone     *time.Location
}

func (s *fixtureConditions) EvaluateCondition(ctx context.Context, program template.Program, bindings []template.Binding) (template.TrackedResult, error) {
	inputs, err := template.NewInputsFromReadView(s.snapshot.view, bindings, s.clock.Now(), s.zone)
	if err != nil {
		return template.TrackedResult{}, err
	}
	return template.EvaluateTracked(ctx, program, inputs, template.DefaultLimits())
}

type commandRecorder struct {
	mu          sync.Mutex
	snapshot    Snapshot
	clock       *fixtureClock
	intents     []Intent
	diagnostics []Diagnostic
}

func (r *commandRecorder) ExecuteCall(ctx context.Context, call ir.Call, execution automationruns.CommandExecution) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, target := range call.Targets() {
		if len(r.intents) == MaxIntents {
			r.diagnostics = append(r.diagnostics, Diagnostic{Code: CapacityExceeded, Position: sourcePosition(execution.Source())})
			return errRefused
		}
		found, refusal := false, false
		for _, fixture := range r.snapshot.fixtures {
			if fixture.entity.Alias() == target.Alias() {
				found, refusal = true, fixture.refuse
				break
			}
		}
		outcome := Recorded
		if !found || refusal {
			outcome = Refused
		}
		r.intents = append(r.intents, Intent{call: call, alias: target.Alias(), position: sourcePosition(execution.Source()), outcome: outcome, at: r.clock.Now()})
		if outcome == Refused {
			code := FixtureRefused
			if !found {
				code = MissingFixture
			}
			r.diagnostics = append(r.diagnostics, Diagnostic{Code: code, Position: sourcePosition(target.Source())})
			return errRefused
		}
	}
	return nil
}

type fixtureResolver struct{}

func (fixtureResolver) ResolveEntity(domain.HAAlias) (domain.EntityID, error) {
	return domain.EntityID{}, ErrInvalid
}
func (fixtureResolver) ResolveDevice(ir.HADeviceRef) (domain.DeviceID, error) {
	return domain.DeviceID{}, ErrInvalid
}
func (fixtureResolver) ResolveCalendar(domain.HAAlias) (automationtriggers.CalendarSourceID, error) {
	return automationtriggers.CalendarSourceID{}, ErrInvalid
}

type firstTriggerClock struct {
	mu          sync.Mutex
	base        *fixtureClock
	initialized bool
	deadline    time.Duration
}

func (c *firstTriggerClock) Now() time.Time         { return c.base.Now() }
func (c *firstTriggerClock) MonoNow() time.Duration { return c.base.MonoNow() }
func (c *firstTriggerClock) NewTimer(delay time.Duration) automationtriggers.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.base.MonoNow()
	if !c.initialized {
		c.initialized = true
		c.deadline = now + delay
	}
	// A queued trigger-engine wake can discard a fired timer before it is
	// selected. Rearm that same due occurrence without advancing time twice;
	// only a later recurrence is outside the declared coverage.
	if now > c.deadline || delay > c.deadline-now {
		return &fixtureTimer{events: make(chan time.Time, 1)}
	}
	return c.base.NewTimer(delay)
}

type terminalObserver struct {
	done       chan automationruns.Decision
	mu         sync.Mutex
	sources    map[automationruns.HistorySource]ir.Source
	nodes      []NodeTrace
	incomplete bool
}

func newTerminalObserver(actions []ir.Action) *terminalObserver {
	observer := &terminalObserver{done: make(chan automationruns.Decision, 1), sources: make(map[automationruns.HistorySource]ir.Source), nodes: make([]NodeTrace, 0, MaxTraces)}
	var retain func([]ir.Action)
	retain = func(actions []ir.Action) {
		for _, action := range actions {
			source := action.Source()
			at := source.At()
			key := automationruns.HistorySource{Object: source.Object(), File: at.File, Path: source.Path(), Line: at.Line, Column: at.Column}
			observer.sources[key] = source
			retain(action.Children())
			retain(action.Otherwise())
			for _, branch := range action.Branches() {
				retain(branch.Actions())
			}
		}
	}
	retain(actions)
	return observer
}

func (o *terminalObserver) ObserveDecision(value automationruns.DecisionObservation) {
	switch value.Decision {
	case automationruns.DecisionFinished, automationruns.DecisionConditionFalse, automationruns.DecisionExecutionError, automationruns.DecisionPreparationError, automationruns.DecisionCancelled, automationruns.DecisionClosed:
		select {
		case o.done <- value.Decision:
		default:
		}
	}
}
func (o *terminalObserver) ObserveNode(value automationruns.NodeObservation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	source, known := o.sources[value.Source]
	if !known || len(o.nodes) == MaxTraces || value.BranchDepth < 0 || value.BranchDepth > len(value.Branches) || value.IterationDepth < 0 || value.IterationDepth > len(value.Iterations) {
		o.incomplete = true
		return
	}
	o.nodes = append(o.nodes, NodeTrace{source: source, kind: value.Action, stage: value.Stage, at: value.At,
		branches: append([]int(nil), value.Branches[:value.BranchDepth]...), iterations: append([]int(nil), value.Iterations[:value.IterationDepth]...), wait: value.Wait})
}
func (o *terminalObserver) nodeSnapshot() ([]NodeTrace, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]NodeTrace(nil), o.nodes...), o.incomplete
}
func (*terminalObserver) ObserveScriptLink(automationruns.ScriptLinkObservation) {}

func runScene(ctx context.Context, input Input, report Report) (Report, error) {
	files := input.Candidate.Files()
	if len(files) != 1 || files[0].Path() != input.Candidate.Entry() {
		return failed(report, InvalidSource, Position{}), nil
	}
	alias, err := domain.ParseHAAlias(input.Candidate.Identity())
	if err != nil {
		return Report{}, ErrInvalid
	}
	ref, err := hascene.NewSourceRef(alias, files[0].Path(), sha256.Sum256(files[0].Bytes()))
	if err != nil {
		return Report{}, ErrInvalid
	}
	result, err := hascene.Load(ref, files[0].Bytes())
	if err != nil || len(result.Accepted()) != 1 || len(result.Rejected()) != 0 {
		return failed(report, InvalidSource, Position{}), nil
	}
	for _, member := range result.Accepted()[0].Members() {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		if len(report.intents) == MaxIntents {
			return failed(report, CapacityExceeded, Position{}), nil
		}
		at := member.Source()
		position := Position{File: at.File, Line: at.Line, Column: at.Column}
		if _, err := member.Alias().Value(); err != nil {
			report.status = Partial
			report.diagnostics = append(report.diagnostics, Diagnostic{Code: InvalidSource, Position: position})
			continue
		}
		outcome := Recorded
		code := DiagnosticInvalid
		found := false
		for _, fixture := range input.Snapshot.fixtures {
			if fixture.entity.Alias() == member.Alias() {
				found = true
				if fixture.refuse {
					code = FixtureRefused
				}
				break
			}
		}
		if !found {
			code = MissingFixture
		}
		if len(member.Diagnostics()) != 0 {
			code = UnsupportedExtension
		}
		if code != DiagnosticInvalid {
			outcome = Refused
			report.status = Partial
			report.diagnostics = append(report.diagnostics, Diagnostic{Code: code, Position: position})
		}
		report.intents = append(report.intents, Intent{member: member, alias: member.Alias(), position: position, outcome: outcome, at: input.Now.UTC()})
	}
	if len(report.intents) == 0 {
		return failed(report, InvalidSource, Position{}), nil
	}
	if report.status == Partial {
		recorded := false
		for _, intent := range report.intents {
			recorded = recorded || intent.outcome == Recorded
		}
		if !recorded {
			report.status = Failed
		}
	}
	return report, nil
}
