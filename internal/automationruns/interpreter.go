package automationruns

import (
	"context"
	"sync"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/reactivetemplate"
)

// Interpreter is the real NHA-041 preparation/execution port. It binds exactly
// once to the controller that owns its plan, memory pool and child scripts.
type Interpreter struct {
	mu          sync.Mutex
	input       InterpreterInput
	controller  *Controller
	automations map[string]executableProgram
	scripts     map[ir.ScriptID]executableProgram
	traces      []NodeTrace
	evicted     uint64
	traceNext   int
	charge      int
	image       *programImage
	waitPlan    *automationtriggers.PlanRetention
}

func NewInterpreter(input InterpreterInput) (*Interpreter, error) {
	if !input.Plan.Valid() || input.Plan.token == nil || nilPort(input.Conditions) ||
		nilPort(input.Commands) || nilPort(input.Clock) ||
		input.Zone == nil || input.Clock.Now().IsZero() {
		return nil, ErrInvalidPlan
	}
	if _, err := admittedProgramCharge(input.Plan); err != nil {
		return nil, err
	}
	triggerWaits, templateWaits := false, false
	for _, spec := range input.Plan.items {
		triggerWaits = triggerWaits || spec.value.HasAction(ir.ActionWait)
		templateWaits = templateWaits || spec.value.HasAction(ir.ActionWaitTemplate)
	}
	for _, spec := range input.Plan.scripts {
		triggerWaits = triggerWaits || spec.script.HasAction(ir.ActionWait)
		templateWaits = templateWaits || spec.script.HasAction(ir.ActionWaitTemplate)
	}
	if triggerWaits && input.TriggerWaits == nil {
		return nil, ErrInvalidPlan
	}
	if input.TriggerPlan.Valid() && input.TriggerWaits == nil {
		return nil, ErrInvalidPlan
	}
	if templateWaits {
		shared, actual := input.Conditions.(*reactivetemplate.Service)
		if !actual || shared != input.TemplateWaits || !shared.SupportsScriptWaits(input.Zone.String()) {
			return nil, ErrInvalidPlan
		}
	}
	return &Interpreter{input: input}, nil
}

// bind reserves the admitted program and trace images before accessor copies,
// template compilation or maps are constructed. The controller retains them
// for as long as its inspectable interpreter is alive.
func (i *Interpreter) bind(c *Controller) error {
	return i.bindImage(c, c.active)
}

func (i *Interpreter) bindImage(c *Controller, image *programImage) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if c == nil || image == nil || i.controller != nil || image.plan.token != i.input.Plan.token || image.plan.provenance != i.input.Plan.provenance {
		return ErrInvalidPlan
	}
	charge, err := admittedProgramCharge(image.plan)
	if err != nil {
		return err
	}
	if len(image.plan.items)+len(image.plan.scripts) != 0 {
		charge += nodeTraceCharge
	}
	if !c.pool.adjust(0, charge) {
		return ErrCapacity
	}
	retained := false
	var waitPlan *automationtriggers.PlanRetention
	defer func() {
		if !retained {
			i.waitPlan = nil
			if waitPlan != nil {
				_ = waitPlan.Release()
			}
			c.pool.adjust(charge, 0)
		}
	}()
	if i.input.TriggerWaits != nil {
		if i.input.TriggerPlan.Valid() {
			waitPlan, err = i.input.TriggerWaits.RetainPlan(i.input.TriggerPlan)
		} else {
			waitPlan, err = i.input.TriggerWaits.RetainCurrentPlan()
		}
		if err != nil {
			return err
		}
		image.triggerIdentity = waitPlan.Identity()
	}
	i.waitPlan = waitPlan
	automations := make(map[string]executableProgram, len(image.plan.items))
	scripts := make(map[ir.ScriptID]executableProgram, len(image.plan.scripts))
	for id, spec := range image.plan.items {
		conditions, err := compileConditions(spec.value.Conditions())
		if err != nil {
			return err
		}
		nodes, err := compileNodes(spec.value.Actions(), image.plan.scripts)
		if err != nil {
			return err
		}
		if err := i.admitWaitSources(nodes); err != nil {
			return err
		}
		variables, err := compileVariables(spec.value.Variables())
		if err != nil {
			return err
		}
		names, err := programBindingNames(nodes, variables, true)
		if err != nil {
			return err
		}
		automations[id] = executableProgram{nodes: nodes, conditions: conditions, variables: variables, bindingNames: names}
	}
	for id, spec := range image.plan.scripts {
		nodes, err := compileNodes(spec.script.Actions(), image.plan.scripts)
		if err != nil {
			return err
		}
		if err := i.admitWaitSources(nodes); err != nil {
			return err
		}
		variables, err := compileVariables(spec.script.Variables())
		if err != nil {
			return err
		}
		names, err := programBindingNames(nodes, variables, false)
		if err != nil {
			return err
		}
		scripts[id] = executableProgram{nodes: nodes, bindingNames: names}
	}
	i.controller, i.image, i.charge, i.waitPlan, i.automations, i.scripts = c, image, charge, waitPlan, automations, scripts
	if charge != 0 {
		i.traces = make([]NodeTrace, 0, nodeTraceCapacity)
	}
	retained = true
	return nil
}

func (i *Interpreter) releaseImage() {
	i.mu.Lock()
	if i.waitPlan != nil {
		_ = i.waitPlan.Release()
	}
	i.automations, i.scripts, i.traces = nil, nil, nil
	// The first Controller remains the immutable bind witness after release.
	i.image, i.waitPlan, i.charge = nil, nil, 0
	i.traceNext = 0
	i.mu.Unlock()
}

func compileVariables(variables []ir.Variable) ([]template.Binding, error) {
	if len(variables) > MaxBindings {
		return nil, ErrCapacity
	}
	bindings := make([]template.Binding, 0, len(variables))
	for _, variable := range variables {
		value, err := literalValue(variable.Value())
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, template.Binding{Name: variable.Name(), Value: value})
	}
	if err := admittedBindings(bindings); err != nil {
		return nil, err
	}
	return bindings, nil
}

func (i *Interpreter) Prepare(ctx context.Context, automation ir.Automation, trigger Trigger) (Preparation, error) {
	if i == nil || nilPort(ctx) || i.controller == nil || !automation.Valid() {
		return Preparation{}, ErrInvalidPlan
	}
	program, present := i.automations[automation.ID()]
	if !present {
		return Preparation{}, ErrInvalidPlan
	}
	bindings, err := automationBindings(automation, program.variables, trigger)
	if err != nil {
		return Preparation{}, err
	}
	if err := fitProgramBindings(program, bindings); err != nil {
		return Preparation{}, err
	}
	allowed, err := i.evaluateConditions(ctx, program.conditions, bindings, trigger)
	if err != nil {
		return Preparation{}, err
	}
	if !allowed {
		return Preparation{Outcome: PreparationConditionFalse}, nil
	}
	// Controller's preparation reservation covers this owned copy.
	return Preparation{Outcome: PreparationReady, Bindings: bindings}, nil
}

func (i *Interpreter) Execute(ctx context.Context, automation ir.Automation, run Run) error {
	if i == nil || nilPort(ctx) || !automation.Valid() {
		return ErrInvalidPlan
	}
	program, present := i.automations[automation.ID()]
	if !present || run.trigger.automation != automation.ID() {
		return ErrInvalidPlan
	}
	return i.executeProgram(ctx, program, run)
}
func (i *Interpreter) ExecuteScript(ctx context.Context, script ir.Script, run Run) error {
	if i == nil || nilPort(ctx) || !script.Valid() {
		return ErrInvalidPlan
	}
	program, present := i.scripts[script.ID()]
	if !present || run.trigger.script != script.ID() {
		return ErrInvalidPlan
	}
	return i.executeProgram(ctx, program, run)
}
func (i *Interpreter) executeProgram(ctx context.Context, program executableProgram, run Run) error {
	if i.controller == nil || i.image == nil || run.generation != i.image.generation ||
		(run.image != nil && run.image != i.image) || run.env == nil ||
		run.env.arena == nil || run.env.arena.pool != &i.controller.pool {
		return ErrInvalidPlan
	}
	_, err := i.executeNodes(ctx, program.nodes, run, &runWork{})
	return err
}

type nodeFlow uint8

const (
	flowInvalid nodeFlow = iota
	flowContinue
	flowStop
)

func (i *Interpreter) executeNodes(ctx context.Context, nodes []executableNode, run Run, work *runWork) (nodeFlow, error) {
	for _, node := range nodes {
		if err := ctx.Err(); err != nil {
			return flowInvalid, err
		}
		visit, err := work.nextVisit(node.source)
		if err != nil {
			return flowInvalid, err
		}
		run.visit = visit
		i.traceNode(run, node, NodeEntered, nil)
		flow, err := i.executeNode(ctx, node, run, work)
		stage := NodeCompleted
		if flow == flowStop {
			stage = NodeStopped
		}
		if err != nil {
			stage = NodeFailed
		}
		if node.kind != ir.ActionWait && node.kind != ir.ActionWaitTemplate {
			i.traceNode(run, node, stage, err)
		}
		if err != nil || flow == flowStop {
			return flow, err
		}
		if flow != flowContinue {
			return flowInvalid, ErrInvalidPlan
		}
	}
	return flowContinue, nil
}

func (i *Interpreter) nodeConditions(ctx context.Context, conditions []executableCondition, run Run) (bool, error) {
	bindings, release, err := run.env.snapshot()
	if err != nil {
		return false, err
	}
	defer release()
	return i.evaluateConditions(ctx, conditions, bindings, run.trigger)
}
func (i *Interpreter) privateSequence(ctx context.Context, nodes []executableNode, run Run, work *runWork) (nodeFlow, error) {
	child, err := run.env.ForkBranch()
	if err != nil {
		return flowInvalid, err
	}
	defer child.closeBranch()
	run.env = child
	return i.executeNodes(ctx, nodes, run, work)
}

func (i *Interpreter) executeNode(ctx context.Context, node executableNode, run Run, work *runWork) (nodeFlow, error) {
	switch node.kind {
	case ir.ActionVariables:
		for _, variable := range node.variables {
			if err := run.env.Set(variable.Name, variable.Value); err != nil {
				return flowInvalid, err
			}
		}
	case ir.ActionSequence:
		return i.privateSequence(ctx, node.children, run, work)
	case ir.ActionChoose:
		for _, branch := range node.branches {
			selected, err := i.nodeConditions(ctx, branch.conditions, run)
			if err != nil {
				return flowInvalid, err
			}
			if selected {
				return i.privateSequence(ctx, branch.sequence, run, work)
			}
		}
		return i.privateSequence(ctx, node.otherwise, run, work)
	case ir.ActionIf:
		selected, err := i.nodeConditions(ctx, node.conditions, run)
		if err != nil {
			return flowInvalid, err
		}
		if selected {
			return i.privateSequence(ctx, node.children, run, work)
		}
		return i.privateSequence(ctx, node.otherwise, run, work)
	case ir.ActionRepeat:
		child, err := run.env.ForkBranch()
		if err != nil {
			return flowInvalid, err
		}
		defer child.closeBranch()
		run.env = child
		for index := 0; index < node.count; index++ {
			if work.loops.Add(1) > MaxLoopIterations {
				return flowInvalid, executionFailure(ExecutionWorkBudget, node.source)
			}
			value, err := template.Object([]template.Binding{
				{Name: "index", Value: template.Int(int64(index + 1))},
				{Name: "first", Value: template.Bool(index == 0)},
				{Name: "last", Value: template.Bool(index+1 == node.count)},
			})
			if err != nil {
				return flowInvalid, err
			}
			if err := child.Set("repeat", value); err != nil {
				return flowInvalid, err
			}
			iterationRun := run
			if !iterationRun.path.pushIteration(index + 1) {
				return flowInvalid, ErrCapacity
			}
			flow, err := i.executeNodes(ctx, node.children, iterationRun, work)
			if err != nil || flow == flowStop {
				return flow, err
			}
		}
	case ir.ActionParallel:
		// HA gathers all branch completions before propagating the first
		// branch error. Parent cancellation reaches and joins every branch.
		count := len(node.children)
		if run.path.branchDepth >= MaxBranchDepth {
			return flowInvalid, ErrCapacity
		}
		// Reserve task stacks and branch bookkeeping in the same 041 pool
		// before constructing the task/branch arrays or starting a goroutine.
		taskCharge := count * (64 << 10)
		if !i.controller.pool.adjust(0, taskCharge) {
			return flowInvalid, ErrCapacity
		}
		defer i.controller.pool.adjust(taskCharge, 0)
		children := make([]*Environment, 0, count)
		for index := 0; index < count; index++ {
			child, err := run.env.ForkBranch()
			if err != nil {
				for _, admitted := range children {
					admitted.closeBranch()
				}
				return flowInvalid, err
			}
			children = append(children, child)
		}
		type branchOutcome struct {
			flow nodeFlow
			err  error
		}
		outcomes := make([]branchOutcome, count)
		var joined sync.WaitGroup
		joined.Add(count)
		for index, branch := range node.children {
			childRun := run
			childRun.env = children[index]
			childRun.path.pushBranch(index) // depth admitted before any branch starts
			go func(index int, branch executableNode, childRun Run) {
				defer joined.Done()
				defer func() {
					if recover() != nil {
						outcomes[index] = branchOutcome{flowInvalid, ErrPortPanic}
					}
				}()
				outcomes[index].flow, outcomes[index].err = i.executeNodes(ctx, []executableNode{branch}, childRun, work)
			}(index, branch, childRun)
		}
		if run.suspend != nil {
			run.suspend()
		}
		joined.Wait()
		for _, child := range children {
			child.closeBranch()
		}
		for _, outcome := range outcomes {
			if outcome.err != nil {
				return flowInvalid, outcome.err
			}
			if outcome.flow == flowStop {
				return flowStop, nil
			}
			if outcome.flow != flowContinue {
				return flowInvalid, ErrInvalidPlan
			}
		}
	case ir.ActionDelay:
		outcome, err := i.delay(ctx, node.delay, run.suspend)
		if err != nil {
			return flowInvalid, err
		}
		if outcome == delayStopped {
			return flowStop, nil
		}
		if outcome != delayElapsed {
			return flowInvalid, ErrInvalidPlan
		}
	case ir.ActionWait, ir.ActionWaitTemplate:
		outcome, err := i.wait(ctx, node, run)
		i.traceWait(run, node, outcome, err)
		if outcome.exit == WaitCancelled {
			return flowStop, nil
		}
		if err != nil {
			return flowInvalid, err
		}
		if outcome.exit == WaitStoppedByTimeout {
			return flowStop, nil
		}
	case ir.ActionStop:
		return flowStop, nil
	case ir.ActionCall:
		if node.call.Kind() == ir.CallScript {
			if err := i.callScripts(ctx, node, run); err != nil {
				return flowInvalid, err
			}
		} else {
			if err := ctx.Err(); err != nil {
				return flowInvalid, executionCause(ExecutionCommandError, node.source, err)
			}
			execution := CommandExecution{run: run.id, parent: run.parent, generation: run.generation, provenance: run.provenance, visit: run.visit, ordinal: work.ordinal.Add(1), source: node.source, suspend: run.suspend}
			if !i.controller.pool.adjust(0, MaxProgramBytes) {
				return flowInvalid, ErrCapacity
			}
			err := func() error {
				defer i.controller.pool.adjust(MaxProgramBytes, 0)
				return i.input.Commands.ExecuteCall(ctx, node.call, execution)
			}()
			if err != nil {
				return flowInvalid, executionCause(ExecutionCommandError, node.source, err)
			}
		}
	case ir.ActionInvalid:
		return flowInvalid, ErrInvalidPlan
	default:
		return flowInvalid, ErrInvalidPlan
	}
	return flowContinue, nil
}

func (i *Interpreter) admitWaitSources(nodes []executableNode) error {
	for index := range nodes {
		node := &nodes[index]
		if node.kind == ir.ActionWait {
			if i.waitPlan == nil {
				return ErrInvalidPlan
			}
			admitted, err := i.waitPlan.AdmitWait(node.source)
			if err != nil {
				return executionFailure(ExecutionInvalidProgram, node.source)
			}
			node.admittedWait = admitted
		}
		if err := i.admitWaitSources(node.children); err != nil {
			return err
		}
		if err := i.admitWaitSources(node.otherwise); err != nil {
			return err
		}
		for branch := range node.branches {
			if err := i.admitWaitSources(node.branches[branch].sequence); err != nil {
				return err
			}
		}
	}
	return nil
}
