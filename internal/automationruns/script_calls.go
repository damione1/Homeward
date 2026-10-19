package automationruns

import (
	"context"
	"errors"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/template"
)

// ScriptHandle observes one controller-owned run. Waiting is observation;
// cancellation and joining are explicit, so an awaiting caller cannot silently
// transfer ownership of a detached run to its own context.
type ScriptHandle struct {
	controller *Controller
	entry      *runEntry
}

// StartupKind identifies the boundary at which a detached caller may resume.
// A queued admission is a real change before execution; an active eager
// segment reaches either a suspension or its terminal. Cancellation before
// execution is deliberately not described as a first state change.
type StartupKind uint8

const (
	StartupInvalid StartupKind = iota
	StartupQueuedChange
	StartupSuspended
	StartupTerminated
	StartupCancelledBeforeExecution
)

type StartupOutcome struct {
	kind StartupKind
	err  error
}

func (s StartupOutcome) Kind() StartupKind { return s.kind }
func (s StartupOutcome) Error() error      { return s.err }
func (entry *runEntry) acknowledgeStartup(kind StartupKind, err error) {
	entry.readyOnce.Do(func() { entry.startup = StartupOutcome{kind, err}; close(entry.ready) })
}
func (h *ScriptHandle) Startup(ctx context.Context) (StartupOutcome, error) {
	if h == nil || h.entry == nil || nilPort(ctx) {
		return StartupOutcome{}, ErrInvalidPlan
	}
	select {
	case <-h.entry.ready:
		return h.entry.startup, nil
	case <-ctx.Done():
		return StartupOutcome{}, ctx.Err()
	}
}

func (h *ScriptHandle) ID() RunID {
	if h == nil || h.entry == nil {
		return 0
	}
	return h.entry.run.id
}
func (h *ScriptHandle) Done() <-chan struct{} {
	if h == nil || h.entry == nil {
		return nil
	}
	return h.entry.done
}
func (h *ScriptHandle) Result(ctx context.Context) error {
	if h == nil || h.entry == nil || nilPort(ctx) {
		return ErrInvalidPlan
	}
	select {
	case <-h.entry.done:
		return h.entry.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *ScriptHandle) Cancel() <-chan struct{} {
	if h == nil || h.controller == nil || h.entry == nil {
		return nil
	}
	h.controller.cancelScript(h.entry)
	return h.entry.done
}

// InvokeScript shares the existing preparation and run budgets, lane machine,
// restart joins, and arena ownership. There is no script-specific scheduler.
func (c *Controller) InvokeScript(ctx context.Context, id ir.ScriptID, parameters []template.Binding, parent Run, mode ir.ScriptCallMode) (*ScriptHandle, error) {
	if c == nil || nilPort(ctx) || ctx.Err() != nil {
		return nil, ErrInvalidPlan
	}
	if mode != ir.ScriptCallDirect && mode != ir.ScriptCallDetached {
		return nil, ErrInvalidPlan
	}
	if _, err := id.Value(); err != nil {
		return nil, ErrInvalidPlan
	}
	c.mu.Lock()
	spec, present := c.plan.scripts[id]
	switch {
	case !present:
		c.mu.Unlock()
		return nil, executionFailure(ExecutionInvalidProgram, ir.Source{})
	case !c.started || c.closed || c.ctx.Err() != nil:
		c.mu.Unlock()
		return nil, ErrClosed
	case parent.id != 0 && (parent.generation != c.generation || parent.env == nil || parent.env.arena == nil || parent.env.arena.pool != &c.pool):
		c.mu.Unlock()
		return nil, ErrInvalidPlan
	case c.preparing >= MaxPreparing:
		c.mu.Unlock()
		return nil, ErrCapacity
	case !c.pool.adjust(0, preparationCharge):
		c.mu.Unlock()
		return nil, ErrCapacity
	}
	c.preparing++
	c.wg.Add(1)
	c.mu.Unlock()
	reserved := true
	defer func() {
		if reserved {
			c.pool.adjust(preparationCharge, 0)
		}
		c.mu.Lock()
		c.preparing--
		c.mu.Unlock()
		c.wg.Done()
	}()
	if err := admittedBindings(parameters); err != nil {
		return nil, err
	}
	stack := parent.stack
	if mode == ir.ScriptCallDetached {
		stack = nil
	}
	if len(stack) >= MaxCallDepth {
		return nil, executionFailure(ExecutionRecursion, spec.script.Source())
	}
	if mode == ir.ScriptCallDirect && (spec.mode == ir.AutomationQueued || spec.mode == ir.AutomationRestart) {
		for _, previous := range stack {
			if previous == id {
				return nil, executionFailure(ExecutionRecursion, spec.script.Source())
			}
		}
	}
	stack = append(append([]ir.ScriptID(nil), stack...), id)
	bindings, err := scriptInitialBindings(spec.script, parameters)
	if err != nil {
		return nil, err
	}
	if interpreter, actual := c.executor.(*Interpreter); actual {
		if err := fitProgramBindings(interpreter.scripts[id], bindings); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	arena, environment, err := newRunArena(&c.pool, bindings, 0)
	if err != nil {
		return nil, err
	}
	reserved = false
	entry, decision := c.admit(spec, Trigger{script: id}, arena, environment, stack)
	if entry == nil {
		return nil, &AdmissionError{decision: decision}
	}
	return &ScriptHandle{controller: c, entry: entry}, nil
}

type AdmissionError struct{ decision Decision }

func (e *AdmissionError) Decision() Decision {
	if e == nil {
		return DecisionInvalid
	}
	return e.decision
}
func (e *AdmissionError) Error() string { return "script run admission refused" }

func scriptInitialBindings(script ir.Script, parameters []template.Binding) ([]template.Binding, error) {
	variables := script.Variables()
	if len(variables) > MaxBindings {
		return nil, ErrCapacity
	}
	result := make([]template.Binding, 0, MaxBindings)
	positions := make(map[string]int, MaxBindings)
	for _, variable := range variables {
		value, err := literalValue(variable.Value())
		if err != nil {
			return nil, err
		}
		positions[variable.Name()] = len(result)
		result = append(result, template.Binding{Name: variable.Name(), Value: value})
	}
	for _, parameter := range parameters {
		if index, present := positions[parameter.Name]; present {
			result[index] = parameter
		} else {
			if len(result) == MaxBindings {
				return nil, ErrCapacity
			}
			positions[parameter.Name] = len(result)
			result = append(result, parameter)
		}
	}
	if err := admittedBindings(result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Controller) cancelScript(entry *runEntry) {
	if c == nil || entry == nil {
		return
	}
	c.mu.Lock()
	lane := c.lanes[entry.run.trigger.laneKey()]
	removed := false
	if lane != nil {
		if lane.replacement == entry {
			lane.replacement, removed = nil, true
		} else {
			for index, queued := range lane.queue {
				if queued != entry {
					continue
				}
				copy(lane.queue[index:], lane.queue[index+1:])
				lane.queue[len(lane.queue)-1] = nil
				lane.queue, removed = lane.queue[:len(lane.queue)-1], true
				break
			}
		}
	}
	if removed {
		c.live--
		c.recordLocked(Trace{Decision: DecisionCancelled, Script: entry.run.trigger.script, RunID: entry.run.id, Generation: entry.run.generation})
	}
	c.mu.Unlock()
	entry.cancel()
	if removed {
		entry.err = context.Canceled
		entry.arena.close()
		entry.acknowledgeStartup(StartupCancelledBeforeExecution, context.Canceled)
		close(entry.done)
	}
}

// StopScript captures the current set before cancellation. Runs admitted after
// this call remain independent, as in HA's async_stop snapshot semantics.
func (c *Controller) StopScript(ctx context.Context, id ir.ScriptID) error {
	return c.stopScript(ctx, id, 0)
}

func (c *Controller) stopScript(ctx context.Context, id ir.ScriptID, caller RunID) error {
	if c == nil || nilPort(ctx) {
		return ErrInvalidPlan
	}
	name, err := id.Value()
	if err != nil {
		return ErrInvalidPlan
	}
	c.mu.Lock()
	lane := c.lanes[laneKey{executionScript, name}]
	if lane == nil {
		c.mu.Unlock()
		return ErrInvalidPlan
	}
	entries := make([]*runEntry, 0, len(lane.active)+len(lane.queue)+1)
	for _, entry := range lane.active {
		entries = append(entries, entry)
	}
	entries = append(entries, lane.queue...)
	if lane.replacement != nil {
		entries = append(entries, lane.replacement)
	}
	c.mu.Unlock()
	self := false
	for _, entry := range entries {
		if caller != 0 && entry.run.id == caller {
			self = true
		}
		c.cancelScript(entry)
	}
	joinCtx := ctx
	if self {
		joinCtx = context.WithoutCancel(ctx)
	}
	for _, entry := range entries {
		if self && entry.run.id == caller {
			continue
		}
		select {
		case <-entry.done:
		case <-joinCtx.Done():
			return joinCtx.Err()
		}
	}
	return ctx.Err()
}

func (i *Interpreter) callScripts(ctx context.Context, node executableNode, run Run) error {
	mode := node.call.ScriptMode()
	if mode == ir.ScriptCallStop {
		for _, id := range node.call.Scripts() {
			if err := i.controller.stopScript(ctx, id, run.id); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
	if mode != ir.ScriptCallDirect && mode != ir.ScriptCallDetached {
		return ErrInvalidPlan
	}
	parameters, err := compileVariables(node.call.ScriptVariables())
	if err != nil {
		return err
	}
	for _, id := range node.call.Scripts() {
		handle, err := i.controller.InvokeScript(ctx, id, parameters, run, mode)
		if err != nil {
			// HA's single/max admission logs and returns without executing a
			// child; it does not turn a refused script call into a service error.
			var admission *AdmissionError
			if errors.As(err, &admission) && (admission.Decision() == DecisionSingleBusy || admission.Decision() == DecisionMaxExceeded) {
				continue
			}
			return err
		}
		if mode == ir.ScriptCallDirect {
			// Propagate the child's actual initial segment before declaring
			// the parent's suspension. A child that terminates synchronously
			// is a terminal call, whereas queued/suspended children are waits.
			startup, err := handle.Startup(ctx)
			if err != nil {
				<-handle.Cancel()
				return err
			}
			switch startup.Kind() {
			case StartupQueuedChange, StartupSuspended:
				if run.suspend != nil {
					run.suspend()
				}
			case StartupTerminated:
			case StartupCancelledBeforeExecution:
				return startup.Error()
			case StartupInvalid:
				<-handle.Cancel()
				return ErrInvalidPlan
			default:
				<-handle.Cancel()
				return ErrInvalidPlan
			}
			select {
			case <-handle.Done():
				if err := handle.Result(context.Background()); err != nil {
					return err
				}
			case <-ctx.Done():
				// Cancellation is cooperative and structured: do not release
				// the parent's arena/ports until the actual child has joined.
				<-handle.Cancel()
				return ctx.Err()
			}
		} else {
			// A detached service call completes once its child's initial
			// segment is acknowledged. The parent can continue its own eager
			// segment; it is not declared suspended merely for this handshake.
			startup, err := handle.Startup(ctx)
			if err != nil {
				return err
			}
			switch startup.Kind() {
			case StartupQueuedChange, StartupSuspended, StartupTerminated:
				// A later child failure belongs to its own terminal trace.
			case StartupCancelledBeforeExecution:
				return startup.Error()
			case StartupInvalid:
				return ErrInvalidPlan
			default:
				return ErrInvalidPlan
			}
		}
	}
	return nil
}
