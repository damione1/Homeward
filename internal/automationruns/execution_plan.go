package automationruns

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/reactivetemplate"
)

const (
	MaxPrograms       = 64
	MaxProgramNodes   = 1024
	MaxPlanNodes      = 4096
	MaxProgramBytes   = 2 << 20
	MaxCallDepth      = 8
	MaxNodeVisits     = 100000
	MaxLoopIterations = 10000
	MaxNodeTraces     = 2048
	nodeTraceCharge   = 1 << 20
)

type ExecutionErrorKind uint8

const (
	ExecutionErrorInvalid ExecutionErrorKind = iota
	ExecutionInvalidProgram
	ExecutionWorkBudget
	ExecutionRecursion
	ExecutionWaitTimeout
	ExecutionSourceUnavailable
	ExecutionConditionError
	ExecutionCommandError
)

type ExecutionError struct {
	kind   ExecutionErrorKind
	source ir.Source
	cause  error
}

func executionFailure(kind ExecutionErrorKind, source ir.Source) *ExecutionError {
	return &ExecutionError{kind: kind, source: source}
}
func executionCause(kind ExecutionErrorKind, source ir.Source, cause error) *ExecutionError {
	return &ExecutionError{kind: kind, source: source, cause: cause}
}
func (e *ExecutionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}
func (e *ExecutionError) Kind() ExecutionErrorKind {
	if e == nil {
		return ExecutionErrorInvalid
	}
	return e.kind
}
func (e *ExecutionError) Source() ir.Source {
	if e == nil {
		return ir.Source{}
	}
	return e.source
}
func (e *ExecutionError) Error() string {
	if e == nil {
		return "script execution: invalid error"
	}
	switch e.kind {
	case ExecutionInvalidProgram:
		return "script execution: invalid program"
	case ExecutionWorkBudget:
		return "script execution: work budget exhausted"
	case ExecutionRecursion:
		return "script execution: recursion refused"
	case ExecutionWaitTimeout:
		return "script execution: wait timed out"
	case ExecutionSourceUnavailable:
		return "script execution: source unavailable"
	case ExecutionConditionError:
		return "script execution: condition evaluation failed"
	case ExecutionCommandError:
		return "script execution: command failed"
	case ExecutionErrorInvalid:
		return "script execution: invalid error"
	}
	return "script execution: invalid error"
}

// ExecutionAuthority supplies current sending authority. It does not own a
// household object and supplies no hidden session or household account.
type ExecutionAuthority interface {
	Context(context.Context) (context.Context, domain.ActorID, error)
}

// ConditionSource is implemented by the composed NHA-044 service over its one
// sequencer projection. Evaluation stays in its serialized engine; immutable
// tracked results escape, while large source snapshots do not.
type ConditionSource interface {
	EvaluateCondition(context.Context, template.Program, []template.Binding) (template.TrackedResult, error)
}

type CommandExecution struct {
	run        RunID
	generation Generation
	ordinal    uint64
	source     ir.Source
	actor      domain.ActorID
	suspend    func()
}

func (c CommandExecution) Actor() domain.ActorID { return c.actor }
func (c CommandExecution) Source() ir.Source     { return c.source }

// Suspend marks the port's first asynchronous boundary, before it may block
// for storage, transport or a driver. Synchronous ports simply return; their
// effects stay in the initial interpreter segment. This contract is identical
// for concrete adapters and simulators and confers no command capability.
func (c CommandExecution) Suspend() {
	if c.suspend != nil {
		c.suspend()
	}
}
func (c CommandExecution) CausalKey() (string, error) {
	if c.run == 0 || c.generation == 0 || c.ordinal == 0 {
		return "", ErrInvalidPlan
	}
	return fmt.Sprintf("%d/%d/%d/%s/%s", c.generation, c.run, c.ordinal, c.source.Object(), c.source.Path()), nil
}

type CommandExecutor interface {
	ExecuteCall(context.Context, ir.Call, CommandExecution) error
}

type InterpreterInput struct {
	Plan          Plan
	TriggerWaits  *automationtriggers.Service
	TemplateWaits *reactivetemplate.Service
	Conditions    ConditionSource
	Authority     ExecutionAuthority
	Commands      CommandExecutor
	Clock         automationtriggers.Clock
	Zone          *time.Location
}

type executableCondition struct {
	kind       ir.ConditionKind
	source     ir.Source
	program    template.Program
	triggerIDs []string
}
type executableBranch struct {
	conditions []executableCondition
	sequence   []executableNode
}
type executableNode struct {
	kind                ir.ActionKind
	source              ir.Source
	variables           []template.Binding
	conditions          []executableCondition
	branches            []executableBranch
	children, otherwise []executableNode
	count               int
	delay               time.Duration
	waitProgram         template.Program
	timeout             time.Duration
	hasTimeout          bool
	continueOnTimeout   bool
	call                ir.Call
}
type executableProgram struct {
	nodes        []executableNode
	conditions   []executableCondition
	variables    []template.Binding
	bindingNames []string
}
type runWork struct {
	visits, loops atomic.Int64
	ordinal       atomic.Uint64
}

func (w *runWork) visit(source ir.Source) error {
	if w.visits.Add(1) > MaxNodeVisits {
		return executionFailure(ExecutionWorkBudget, source)
	}
	return nil
}

func admittedProgramCharge(plan Plan) (int, error) {
	if !plan.Valid() || len(plan.items)+len(plan.scripts) > MaxPrograms {
		return 0, ErrInvalidPlan
	}
	count, charge := 0, 0
	for _, spec := range plan.items {
		nodes, depth, bytes := spec.value.ExecutionSize()
		if nodes > MaxProgramNodes || depth > MaxBranchDepth || nodes > MaxPlanNodes-count || bytes > MaxProgramBytes-charge {
			return 0, ErrCapacity
		}
		count, charge = count+nodes, charge+bytes
	}
	for _, spec := range plan.scripts {
		nodes, depth, bytes := spec.script.ExecutionSize()
		if nodes > MaxProgramNodes || depth > MaxBranchDepth || nodes > MaxPlanNodes-count || bytes > MaxProgramBytes-charge || spec.script.VariableCount() > MaxBindings {
			return 0, ErrCapacity
		}
		count, charge = count+nodes, charge+bytes
	}
	return charge, nil
}

func compileConditions(conditions []ir.Condition) ([]executableCondition, error) {
	result := make([]executableCondition, 0, len(conditions))
	for _, condition := range conditions {
		item := executableCondition{kind: condition.Kind(), source: condition.Source()}
		switch condition.Kind() {
		case ir.ConditionState:
			alias, err := condition.EntityAlias().Value()
			if err != nil {
				return nil, err
			}
			program, err := template.Compile("{{ is_state(" + strconv.Quote(alias) + ", " + strconv.Quote(condition.State()) + ") }}")
			if err != nil {
				return nil, err
			}
			item.program = program
		case ir.ConditionTemplate:
			item.program = condition.Program()
		case ir.ConditionTrigger:
			item.triggerIDs = condition.TriggerIDs()
		case ir.ConditionInvalid:
			return nil, ErrInvalidPlan
		default:
			return nil, ErrInvalidPlan
		}
		result = append(result, item)
	}
	return result, nil
}

func compileNodes(actions []ir.Action, scripts map[ir.ScriptID]automationSpec) ([]executableNode, error) {
	result := make([]executableNode, 0, len(actions))
	for _, action := range actions {
		item := executableNode{kind: action.Kind(), source: action.Source(), count: action.Count(), call: action.Call(),
			delay: time.Duration(action.DelayMillis()) * time.Millisecond, waitProgram: action.WaitProgram(), continueOnTimeout: true}
		var err error
		item.conditions, err = compileConditions(action.Conditions())
		if err != nil {
			return nil, err
		}
		for _, variable := range action.Variables() {
			value, err := literalValue(variable.Value())
			if err != nil {
				return nil, err
			}
			item.variables = append(item.variables, template.Binding{Name: variable.Name(), Value: value})
		}
		if milliseconds, present := action.TimeoutMillis(); present {
			item.timeout, item.hasTimeout = time.Duration(milliseconds)*time.Millisecond, true
		}
		if continuation, present := action.ContinueOnTimeout(); present {
			item.continueOnTimeout = continuation
		}
		item.children, err = compileNodes(action.Children(), scripts)
		if err != nil {
			return nil, err
		}
		item.otherwise, err = compileNodes(action.Otherwise(), scripts)
		if err != nil {
			return nil, err
		}
		for _, branch := range action.Branches() {
			conditions, err := compileConditions(branch.Conditions())
			if err != nil {
				return nil, err
			}
			children, err := compileNodes(branch.Actions(), scripts)
			if err != nil {
				return nil, err
			}
			item.branches = append(item.branches, executableBranch{conditions, children})
		}
		switch action.Kind() {
		case ir.ActionVariables, ir.ActionSequence, ir.ActionChoose, ir.ActionIf, ir.ActionRepeat, ir.ActionParallel, ir.ActionWait, ir.ActionStop, ir.ActionDelay, ir.ActionWaitTemplate:
		case ir.ActionCall:
			if item.call.Kind() == ir.CallScript {
				for _, id := range item.call.Scripts() {
					if _, present := scripts[id]; !present {
						return nil, executionFailure(ExecutionInvalidProgram, item.source)
					}
				}
			}
		case ir.ActionInvalid:
			return nil, ErrInvalidPlan
		default:
			return nil, ErrInvalidPlan
		}
		result = append(result, item)
	}
	return result, nil
}
