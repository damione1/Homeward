// Package automationruns owns bounded automation and script executions. Command
// authority is supplied explicitly; compiling a plan never confers it.
package automationruns

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/sequencer"
)

var (
	ErrInvalidPlan = errors.New("automation runs: invalid plan")
	ErrCapacity    = errors.New("automation runs: capacity exceeded")
	ErrClosed      = errors.New("automation runs: closed")
	ErrPortPanic   = errors.New("automation runs: port panic")
)

const (
	DefaultMaxRuns      = 10
	MaxLiveRuns         = 64
	MaxPreparing        = 4
	MaxTriggerBytes     = 64 << 10
	MaxEnvironmentBytes = 1 << 20
	MaxMemoryBytes      = 16 << 20
	MaxBindings         = 16
	MaxBranchDepth      = 8
	MaxBranchesPerRun   = 32
	MaxTraces           = 512
	controllerCharge    = 1 << 20
	bindingCharge       = 64 << 10
	environmentBase     = 256
	// The preparation reservation covers the port's admitted output, the
	// temporary NewInputs validation copy, and the retained environment copy.
	preparationCharge = 2*MaxTriggerBytes + 2*MaxEnvironmentBytes + 2*environmentBase
)

// ResultReader is intentionally only the NHA-040 observation port.
type ResultReader interface {
	NextResult(context.Context) (automationtriggers.Result, error)
	Done() <-chan struct{}
	Error() error
}

type PreparationOutcome uint8

const (
	PreparationInvalid PreparationOutcome = iota
	PreparationReady
	PreparationConditionFalse
)

type Preparation struct {
	Outcome  PreparationOutcome
	Bindings []template.Binding
}

// Preparer evaluates automation variables and conditions before run admission.
// It must honor cancellation. An error is distinct from a false condition.
type Preparer interface {
	Prepare(context.Context, ir.Automation, Trigger) (Preparation, error)
}

// Executor is the future NHA-042 adapter. It must return after child tasks
// have joined and must honor cancellation; no physical command is exposed here.
type Executor interface {
	Execute(context.Context, ir.Automation, Run) error
}

// ScriptExecutor is required only when the same controller's plan contains
// real scripts. A Script never passes through Execute as a fake Automation.
type ScriptExecutor interface {
	ExecuteScript(context.Context, ir.Script, Run) error
}

type executionKind uint8

const (
	executionInvalid executionKind = iota
	executionAutomation
	executionScript
)

type laneKey struct {
	kind     executionKind
	identity string
}

type Trigger struct {
	automation, triggerKey string
	payload                string
	at                     time.Time
	cursor                 sequencer.Cursor
	entity                 domain.EntityID
	script                 ir.ScriptID
}

func (t Trigger) Automation() string       { return t.automation }
func (t Trigger) TriggerKey() string       { return t.triggerKey }
func (t Trigger) Payload() string          { return t.payload }
func (t Trigger) At() time.Time            { return t.at }
func (t Trigger) Cursor() sequencer.Cursor { return t.cursor }
func (t Trigger) Entity() domain.EntityID  { return t.entity }
func (t Trigger) laneKey() laneKey {
	if id, err := t.script.Value(); err == nil {
		return laneKey{executionScript, id}
	}
	return laneKey{executionAutomation, t.automation}
}

type RunID uint64
type Generation uint64

type Run struct {
	id         RunID
	parent     RunID
	visit      uint64
	generation Generation
	provenance DefinitionProvenance
	image      *programImage
	trigger    Trigger
	env        *Environment
	stack      []ir.ScriptID
	suspend    func()
	path       executionPath
}

func (r Run) ID() RunID                        { return r.id }
func (r Run) ParentID() RunID                  { return r.parent }
func (r Run) Generation() Generation           { return r.generation }
func (r Run) Provenance() DefinitionProvenance { return r.provenance }
func (r Run) Trigger() Trigger                 { return r.trigger }
func (r Run) Environment() *Environment        { return r.env }
func (r Run) ScriptID() ir.ScriptID            { return r.trigger.script }

type automationSpec struct {
	value       ir.Automation
	provenance  DefinitionProvenance
	mode        ir.AutomationMode
	max         int
	maxExceeded ir.MaxExceededMode
	script      ir.Script
}

// Plan keeps the compiled automation as the only mode authority. Operational
// admission is a separate boundary from the broader logic-1 IR max profile.
type planToken struct{ admitted bool }

type Plan struct {
	token      *planToken
	valid      bool
	provenance DefinitionProvenance
	items      map[string]automationSpec
	scripts    map[ir.ScriptID]automationSpec
}

func (p Plan) Valid() bool {
	return p.valid && p.token != nil && p.token.admitted && p.provenance.Valid()
}

// NewEmptyActivationPlan represents a committed configuration with no new
// automations or scripts while earlier admitted runs finish on retained images.
// It is never an initial Controller plan.
func NewEmptyActivationPlan() Plan {
	return Plan{token: &planToken{admitted: true}, valid: true, provenance: FixtureProvenance(),
		items: make(map[string]automationSpec), scripts: make(map[ir.ScriptID]automationSpec)}
}

func (p Plan) ScriptIDs() []ir.ScriptID {
	if !p.Valid() {
		return nil
	}
	result := make([]ir.ScriptID, 0, len(p.scripts))
	for id := range p.scripts {
		result = append(result, id)
	}
	sort.Slice(result, func(i, j int) bool { a, _ := result[i].Value(); b, _ := result[j].Value(); return a < b })
	return result
}

// AutomationIDs is a sorted caller-owned view for Run composition admission.
func (p Plan) AutomationIDs() []string {
	if !p.Valid() {
		return nil
	}
	ids := make([]string, 0, len(p.items))
	for id := range p.items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func NewPlan(automations []ir.Automation) (Plan, error) {
	return NewPlanWithScripts(automations, nil)
}

func NewPlanWithScripts(automations []ir.Automation, scripts []ir.Script) (Plan, error) {
	if len(automations)+len(scripts) == 0 || len(automations) > automationtriggers.MaxTriggers || len(scripts) > 64 {
		return Plan{}, ErrInvalidPlan
	}
	p := Plan{token: &planToken{admitted: true}, valid: true, provenance: FixtureProvenance(), items: make(map[string]automationSpec, len(automations)), scripts: make(map[ir.ScriptID]automationSpec, len(scripts))}
	for _, a := range automations {
		if !a.Valid() || a.ID() == "" || len(a.ID()) > 256 {
			return Plan{}, ErrInvalidPlan
		}
		if _, duplicate := p.items[a.ID()]; duplicate {
			return Plan{}, ErrInvalidPlan
		}
		switch a.Mode() {
		case ir.AutomationSingle, ir.AutomationRestart, ir.AutomationQueued, ir.AutomationParallel:
		default:
			return Plan{}, ErrInvalidPlan
		}
		switch a.MaxExceeded() {
		case ir.MaxExceededUnspecified, ir.MaxExceededSilent, ir.MaxExceededWarning, ir.MaxExceededError:
		default:
			return Plan{}, ErrInvalidPlan
		}
		max := DefaultMaxRuns
		if declared, present := a.Max(); present {
			max = declared
		}
		if max < 1 {
			return Plan{}, ErrInvalidPlan
		}
		if max > MaxLiveRuns {
			return Plan{}, ErrCapacity
		}
		p.items[a.ID()] = automationSpec{value: a, provenance: p.provenance, mode: a.Mode(), max: max, maxExceeded: a.MaxExceeded()}
	}
	for _, script := range scripts {
		if !script.Valid() {
			return Plan{}, ErrInvalidPlan
		}
		if _, err := script.ID().Value(); err != nil {
			return Plan{}, ErrInvalidPlan
		}
		if _, duplicate := p.scripts[script.ID()]; duplicate {
			return Plan{}, ErrInvalidPlan
		}
		switch script.Mode() {
		case ir.AutomationSingle, ir.AutomationRestart, ir.AutomationQueued, ir.AutomationParallel:
		default:
			return Plan{}, ErrInvalidPlan
		}
		switch script.MaxExceeded() {
		case ir.MaxExceededUnspecified, ir.MaxExceededSilent, ir.MaxExceededWarning, ir.MaxExceededError:
		default:
			return Plan{}, ErrInvalidPlan
		}
		maximum := DefaultMaxRuns
		if value, present := script.Max(); present {
			maximum = value
		}
		if maximum < 1 {
			return Plan{}, ErrInvalidPlan
		}
		if maximum > MaxLiveRuns {
			return Plan{}, ErrCapacity
		}
		p.scripts[script.ID()] = automationSpec{script: script, provenance: p.provenance, mode: script.Mode(), max: maximum, maxExceeded: script.MaxExceeded()}
	}
	return p, nil
}

func nilPort(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return r.IsNil()
	default:
		return false
	}
}
