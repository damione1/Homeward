package automationruns

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/automationtriggers"
)

var nextGeneration atomic.Uint64

type Decision uint8

const (
	DecisionInvalid Decision = iota
	DecisionPreparing
	DecisionAdmitted
	DecisionQueued
	DecisionRestartPending
	DecisionSingleBusy
	DecisionMaxExceeded
	DecisionPreparationSaturated
	DecisionMemorySaturated
	DecisionConditionFalse
	DecisionPreparationError
	DecisionExecutionError
	DecisionFinished
	DecisionCancelled
	DecisionSuperseded
	DecisionClosed
	DecisionUnknownAutomation
	DecisionTriggerMissed
	DecisionTriggerDegraded
)

type Trace struct {
	Decision    Decision
	MaxExceeded ir.MaxExceededMode
	ordinal     TraceOrdinal
	Automation  string
	Script      ir.ScriptID
	TriggerKey  string
	RunID       RunID
	Generation  Generation
	Error       string
}

func effectiveMaxExceeded(mode ir.MaxExceededMode) ir.MaxExceededMode {
	if mode == ir.MaxExceededUnspecified {
		return ir.MaxExceededWarning
	}
	return mode
}

type runEntry struct {
	run       Run
	arena     *runArena
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	started   bool
	ready     chan struct{}
	readyOnce sync.Once
	err       error
	startup   StartupOutcome
}

type lane struct {
	active      map[RunID]*runEntry
	queue       []*runEntry
	replacement *runEntry
}

type Controller struct {
	mu              sync.Mutex
	plan            Plan
	preparer        Preparer
	executor        Executor
	reader          ResultReader
	pool            memoryPool
	ctx             context.Context
	cancel          context.CancelFunc
	done            chan struct{}
	doneOnce        sync.Once
	wg              sync.WaitGroup
	lanes           map[laneKey]*lane
	traces          []Trace
	traceOrdinal    atomic.Uint64
	evicted         uint64
	next            RunID
	generation      Generation
	preparing, live int
	started, closed bool
	err             error
}

func NewController(plan Plan, reader ResultReader, preparer Preparer, executor Executor) (*Controller, error) {
	if !plan.Valid() || nilPort(preparer) || nilPort(executor) ||
		(len(plan.items) != 0 && (nilPort(reader) || reader.Done() == nil)) {
		return nil, ErrInvalidPlan
	}
	if len(plan.scripts) != 0 {
		port, ok := executor.(ScriptExecutor)
		if !ok || nilPort(port) {
			return nil, ErrInvalidPlan
		}
	}
	lanes := make(map[laneKey]*lane, len(plan.items)+len(plan.scripts))
	for id := range plan.items {
		lanes[laneKey{executionAutomation, id}] = &lane{active: make(map[RunID]*runEntry)}
	}
	for id := range plan.scripts {
		name, _ := id.Value()
		lanes[laneKey{executionScript, name}] = &lane{active: make(map[RunID]*runEntry)}
	}
	c := &Controller{plan: plan, reader: reader, preparer: preparer, executor: executor,
		lanes: lanes, traces: make([]Trace, 0, MaxTraces), done: make(chan struct{}), generation: Generation(nextGeneration.Add(1))}
	if !c.pool.adjust(0, controllerCharge) {
		return nil, ErrCapacity
	}
	if interpreter, ok := executor.(*Interpreter); ok {
		preparation, same := preparer.(*Interpreter)
		if !same || preparation != interpreter {
			return nil, ErrInvalidPlan
		}
		if err := interpreter.bind(c); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *Controller) Start(ctx context.Context) error {
	if c == nil || nilPort(ctx) {
		return ErrInvalidPlan
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	if c.started {
		c.mu.Unlock()
		return ErrInvalidPlan
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.started = true
	c.wg.Add(1)
	c.mu.Unlock()
	if !nilPort(c.reader) {
		go c.readLoop()
	} else {
		go func() { defer c.wg.Done(); <-c.ctx.Done() }()
	}
	go func() { c.wg.Wait(); c.doneOnce.Do(func() { close(c.done) }) }()
	return nil
}

func (c *Controller) recordLocked(trace Trace) {
	if len(trace.Error) > 256 {
		trace.Error = trace.Error[:256]
	}
	// Error() may return a view into a much larger string. Own the bounded
	// prefix so the trace ring cannot retain that backing allocation.
	trace.Error = strings.Clone(trace.Error)
	if len(c.traces) == MaxTraces {
		copy(c.traces, c.traces[1:])
		trace.ordinal = c.nextTraceOrdinal()
		c.traces[len(c.traces)-1] = trace
		c.evicted++
		return
	}
	trace.ordinal = c.nextTraceOrdinal()
	c.traces = append(c.traces, trace)
}

func (c *Controller) Traces() ([]Trace, uint64) {
	if c == nil {
		return nil, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Trace(nil), c.traces...), c.evicted
}

func (c *Controller) Error() error {
	if c == nil {
		return ErrInvalidPlan
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Controller) Done() <-chan struct{} {
	if c == nil {
		return nil
	}
	return c.done
}

func (c *Controller) readLoop() {
	defer c.wg.Done()
	defer c.BeginClose()
	for {
		result, err := c.reader.NextResult(c.ctx)
		if err != nil {
			if c.ctx.Err() == nil {
				readerErr := c.reader.Error()
				c.mu.Lock()
				c.err = errors.Join(err, readerErr)
				c.mu.Unlock()
				c.BeginClose()
			}
			return
		}
		switch result.Kind() {
		case automationtriggers.ResultFired:
			c.beginPrepare(result)
		case automationtriggers.ResultMissed, automationtriggers.ResultDegraded:
			c.mu.Lock()
			decision := DecisionTriggerMissed
			if result.Kind() == automationtriggers.ResultDegraded {
				decision = DecisionTriggerDegraded
			}
			c.recordLocked(Trace{Decision: decision, Automation: result.Automation(), TriggerKey: result.TriggerKey()})
			c.mu.Unlock()
		default:
			c.mu.Lock()
			c.err = ErrInvalidPlan
			c.mu.Unlock()
			c.BeginClose()
			return
		}
	}
}

// beginPrepare reserves its worst-case copied payload and environment before
// reading the result's payload or calling a potentially blocking port.
func (c *Controller) beginPrepare(result automationtriggers.Result) {
	id, key := result.Automation(), result.TriggerKey()
	c.mu.Lock()
	spec, known := c.plan.items[id]
	switch {
	case c.closed:
		c.recordLocked(Trace{Decision: DecisionClosed, Automation: id, TriggerKey: key})
	case !known:
		c.recordLocked(Trace{Decision: DecisionUnknownAutomation, Automation: id, TriggerKey: key})
	case c.preparing == MaxPreparing:
		c.recordLocked(Trace{Decision: DecisionPreparationSaturated, Automation: id, TriggerKey: key})
	case !c.pool.adjust(0, preparationCharge):
		c.recordLocked(Trace{Decision: DecisionMemorySaturated, Automation: id, TriggerKey: key})
	default:
		c.preparing++
		c.wg.Add(1)
		c.recordLocked(Trace{Decision: DecisionPreparing, Automation: id, TriggerKey: key})
		c.mu.Unlock()
		go c.prepare(spec, result)
		return
	}
	c.mu.Unlock()
}

func (c *Controller) prepare(spec automationSpec, result automationtriggers.Result) {
	defer c.wg.Done()
	reserved := true
	defer func() {
		if reserved {
			c.pool.adjust(preparationCharge, 0)
		}
		c.mu.Lock()
		c.preparing--
		c.mu.Unlock()
	}()
	trigger := Trigger{automation: result.Automation(), triggerKey: result.TriggerKey(),
		payload: string(result.Payload()), at: result.At(), cursor: result.Cursor(), entity: result.Entity()}
	if len(trigger.payload) > MaxTriggerBytes {
		c.traceOutcome(DecisionMemorySaturated, trigger, nil)
		return
	}
	prepared, err := c.callPrepare(spec, trigger)
	if err != nil {
		c.traceOutcome(DecisionPreparationError, trigger, err)
		return
	}
	if c.ctx.Err() != nil {
		c.traceOutcome(DecisionClosed, trigger, c.ctx.Err())
		return
	}
	switch prepared.Outcome {
	case PreparationConditionFalse:
		if len(prepared.Bindings) != 0 {
			c.traceOutcome(DecisionPreparationError, trigger, ErrInvalidPlan)
		} else {
			c.traceOutcome(DecisionConditionFalse, trigger, nil)
		}
		return
	case PreparationReady:
	default:
		c.traceOutcome(DecisionPreparationError, trigger, ErrInvalidPlan)
		return
	}
	arena, env, err := newRunArena(&c.pool, prepared.Bindings, len(trigger.payload))
	if err != nil {
		c.traceOutcome(DecisionPreparationError, trigger, err)
		return
	}
	reserved = false
	c.admit(spec, trigger, arena, env)
}

func (c *Controller) traceOutcome(decision Decision, trigger Trigger, err error) {
	trace := Trace{Decision: decision, Automation: trigger.automation, TriggerKey: trigger.triggerKey}
	if err != nil {
		trace.Error = err.Error()
	}
	c.mu.Lock()
	c.recordLocked(trace)
	c.mu.Unlock()
}

func (c *Controller) callPrepare(spec automationSpec, trigger Trigger) (prepared Preparation, err error) {
	defer func() {
		if recover() != nil {
			prepared, err = Preparation{}, ErrPortPanic
		}
	}()
	return c.preparer.Prepare(c.ctx, spec.value, trigger)
}

func (c *Controller) callExecute(spec automationSpec, entry *runEntry) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrPortPanic
		}
	}()
	if spec.script.Valid() {
		port, ok := c.executor.(ScriptExecutor)
		if !ok || nilPort(port) {
			return ErrInvalidPlan
		}
		return port.ExecuteScript(entry.ctx, spec.script, entry.run)
	}
	return c.executor.Execute(entry.ctx, spec.value, entry.run)
}

func (c *Controller) activateLocked(l *lane, entry *runEntry) {
	entry.started = true
	l.active[entry.run.id] = entry
	c.wg.Add(1)
}

func (c *Controller) admit(spec automationSpec, trigger Trigger, arena *runArena, env *Environment, stacks ...[]ir.ScriptID) (*runEntry, Decision) {
	var start *runEntry
	var cancel []*runEntry
	var discard *runEntry
	c.mu.Lock()
	l := c.lanes[trigger.laneKey()]
	decision := DecisionAdmitted
	if c.closed || c.ctx.Err() != nil || l == nil {
		decision = DecisionClosed
	} else if c.live == MaxLiveRuns {
		decision = DecisionMemorySaturated
	} else {
		switch spec.mode {
		case ir.AutomationModeInvalid: // defensive zero fence
			decision = DecisionClosed
		case ir.AutomationSingle:
			if len(l.active) != 0 {
				decision = DecisionSingleBusy
			}
		case ir.AutomationQueued:
			if len(l.active)+len(l.queue) >= spec.max {
				decision = DecisionMaxExceeded
			}
		case ir.AutomationParallel:
			if len(l.active) >= spec.max {
				decision = DecisionMaxExceeded
			}
		case ir.AutomationRestart: // old run is fenced after new candidate is reserved
		default:
			decision = DecisionClosed
		}
	}
	if decision != DecisionAdmitted {
		trace := Trace{Decision: decision, Automation: trigger.automation, Script: trigger.script, TriggerKey: trigger.triggerKey}
		if decision == DecisionSingleBusy || decision == DecisionMaxExceeded {
			trace.MaxExceeded = effectiveMaxExceeded(spec.maxExceeded)
		}
		c.recordLocked(trace)
		c.mu.Unlock()
		arena.close()
		return nil, decision
	}
	c.next++
	id := c.next
	runCtx, runCancel := context.WithCancel(c.ctx)
	entry := &runEntry{run: Run{id: id, generation: c.generation, trigger: trigger, env: env},
		arena: arena, ctx: runCtx, cancel: runCancel, done: make(chan struct{}), ready: make(chan struct{})}
	entry.run.suspend = func() { entry.acknowledgeStartup(StartupSuspended, nil) }
	if len(stacks) != 0 {
		entry.run.stack = append([]ir.ScriptID(nil), stacks[0]...)
	}
	c.live++
	switch spec.mode {
	case ir.AutomationSingle, ir.AutomationParallel:
		c.activateLocked(l, entry)
		start = entry
	case ir.AutomationQueued:
		if len(l.active) == 0 {
			c.activateLocked(l, entry)
			start = entry
		} else {
			l.queue = append(l.queue, entry)
			decision = DecisionQueued
		}
	case ir.AutomationRestart:
		if l.replacement != nil {
			discard = l.replacement
			l.replacement = nil
			c.live--
			c.recordLocked(Trace{Decision: DecisionSuperseded, Automation: trigger.automation, Script: trigger.script, RunID: discard.run.id})
		}
		if len(l.active) == 0 {
			c.activateLocked(l, entry)
			start = entry
		} else {
			l.replacement = entry
			decision = DecisionRestartPending
			for _, active := range l.active {
				cancel = append(cancel, active)
			}
		}
	}
	c.recordLocked(Trace{Decision: decision, Automation: trigger.automation, Script: trigger.script, TriggerKey: trigger.triggerKey, RunID: id, Generation: c.generation})
	if spec.mode == ir.AutomationQueued && start == nil {
		entry.acknowledgeStartup(StartupQueuedChange, nil)
	}
	c.mu.Unlock()
	for _, active := range cancel {
		active.cancel()
	}
	if discard != nil {
		discard.cancel()
		discard.err = ErrClosed
		discard.acknowledgeStartup(StartupCancelledBeforeExecution, ErrClosed)
		discard.arena.close()
		close(discard.done)
	}
	if start != nil {
		go c.execute(spec, start)
	}
	return entry, decision
}

func (c *Controller) execute(spec automationSpec, entry *runEntry) {
	defer c.wg.Done()
	var err error
	if entry.ctx.Err() == nil {
		err = c.callExecute(spec, entry)
		entry.acknowledgeStartup(StartupTerminated, err)
	} else {
		err = entry.ctx.Err()
		entry.acknowledgeStartup(StartupCancelledBeforeExecution, entry.ctx.Err())
	}
	c.finish(spec, entry, err)
}

func (c *Controller) finish(spec automationSpec, entry *runEntry, err error) {
	var next *runEntry
	var errorText string
	if err != nil {
		errorText = err.Error()
	}
	decision := DecisionFinished
	if entry.ctx.Err() != nil {
		decision = DecisionCancelled
	} else if err != nil {
		decision = DecisionExecutionError
	}
	c.mu.Lock()
	l := c.lanes[entry.run.trigger.laneKey()]
	delete(l.active, entry.run.id)
	c.live--
	trace := Trace{Decision: decision, Automation: entry.run.trigger.automation, Script: entry.run.trigger.script, TriggerKey: entry.run.trigger.triggerKey, RunID: entry.run.id, Generation: entry.run.generation}
	trace.Error = errorText
	c.recordLocked(trace)
	entry.err = err
	entry.acknowledgeStartup(StartupTerminated, entry.err)
	if !c.closed && c.ctx.Err() == nil && len(l.active) == 0 {
		if spec.mode == ir.AutomationQueued && len(l.queue) != 0 {
			next, l.queue = l.queue[0], l.queue[1:]
		} else if spec.mode == ir.AutomationRestart && l.replacement != nil {
			next, l.replacement = l.replacement, nil
		}
		if next != nil {
			c.activateLocked(l, next)
			c.recordLocked(Trace{Decision: DecisionAdmitted, Automation: next.run.trigger.automation, Script: next.run.trigger.script, RunID: next.run.id, Generation: next.run.generation})
		}
	}
	c.mu.Unlock()
	entry.cancel()
	entry.arena.close()
	close(entry.done)
	if next != nil {
		go c.execute(spec, next)
	}
}

func (c *Controller) CloseAdmissions() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
}

func (c *Controller) BeginClose() <-chan struct{} {
	if c == nil {
		return nil
	}
	var pending []*runEntry
	c.mu.Lock()
	c.closed = true
	if !c.started {
		c.doneOnce.Do(func() { close(c.done) })
		c.mu.Unlock()
		return c.done
	}
	if c.cancel != nil {
		c.cancel()
	}
	for _, l := range c.lanes {
		for _, active := range l.active {
			active.cancel()
		}
		pending = append(pending, l.queue...)
		l.queue = nil
		if l.replacement != nil {
			pending = append(pending, l.replacement)
			l.replacement = nil
		}
	}
	c.live -= len(pending)
	for _, entry := range pending {
		c.recordLocked(Trace{Decision: DecisionCancelled, Automation: entry.run.trigger.automation, Script: entry.run.trigger.script, RunID: entry.run.id})
	}
	c.mu.Unlock()
	for _, entry := range pending {
		entry.cancel()
		entry.err = ErrClosed
		entry.acknowledgeStartup(StartupCancelledBeforeExecution, ErrClosed)
		entry.arena.close()
		close(entry.done)
	}
	return c.done
}

func (c *Controller) Close(ctx context.Context) error {
	if c == nil || nilPort(ctx) {
		return ErrInvalidPlan
	}
	done := c.BeginClose()
	select {
	case <-done:
		return c.Error()
	case <-ctx.Done():
		return ctx.Err()
	}
}
