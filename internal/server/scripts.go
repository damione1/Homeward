package server

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/reactivetemplate"
	"github.com/damione1/Homeward/internal/scenes"
	"github.com/damione1/Homeward/internal/scriptcommands"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
)

// DurableCommandsComposition declares the one Run-owned command authority.
// It creates no service, session, ledger or scheduler. Jobs and script runs
// borrow this same declaration; Run constructs its NHA-024 service once.
type DurableCommandsComposition struct {
	generation integrations.GenerationAuthority
	clock      integrations.CommandClock
}

type DurableCommandsInput struct {
	Generation integrations.GenerationAuthority
	Clock      integrations.CommandClock
}

func NewDurableCommandsComposition(input DurableCommandsInput) (*DurableCommandsComposition, error) {
	if nilReactiveInterface(input.Generation) || nilReactiveInterface(input.Clock) || input.Clock.Now().IsZero() {
		return nil, ErrInvalidTriggerComposition
	}
	return &DurableCommandsComposition{generation: input.Generation, clock: input.Clock}, nil
}

// ScriptRunsInput opts the existing run composition into the concrete
// interpreter. It carries sending authority, never ownership of a household
// object. Publication and hot activation of persisted IR belong to NHA-029.
type ScriptRunsInput struct {
	Plan            automationruns.Plan
	Commands        *DurableCommandsComposition
	Authority       automationruns.ExecutionAuthority
	Clock           automationtriggers.Clock
	Zone            *time.Location
	CommandDeadline time.Duration
	// Scenes is an optional admitted catalogue, not an activation instruction.
	// Its runtime borrows this composition's one durable command service.
	Scenes *scenes.Catalogue
}

type scriptCompositionState uint8

const (
	scriptCompositionInvalid scriptCompositionState = iota
	scriptCompositionAdmitted
	scriptCompositionPrepared
	scriptCompositionFailed
)

type scriptRunsComposition struct {
	input       ScriptRunsInput
	state       scriptCompositionState
	interpreter *automationruns.Interpreter
	scenes      *scriptSceneRuntime
}

// NewScriptRunsComposition uses the same AutomationRunsComposition slot as
// NHA-041. Preparation is deferred until Run has the actual shared services;
// no placeholder Preparer or Executor is installed before that boundary.
func NewScriptRunsComposition(input ScriptRunsInput) (*AutomationRunsComposition, error) {
	if !input.Plan.Valid() || input.Commands == nil ||
		nilReactiveInterface(input.Commands.generation) || nilReactiveInterface(input.Commands.clock) ||
		nilReactiveInterface(input.Authority) || nilReactiveInterface(input.Clock) ||
		input.Zone == nil || input.Clock.Now().IsZero() || input.Commands.clock.Now().IsZero() ||
		input.CommandDeadline <= 0 || input.CommandDeadline > 24*time.Hour {
		return nil, ErrInvalidTriggerComposition
	}
	if input.Scenes != nil {
		if len(input.Scenes.IDs()) == 0 {
			return nil, ErrInvalidTriggerComposition
		}
		owned := *input.Scenes
		input.Scenes = &owned
	}
	return &AutomationRunsComposition{
		plan:    input.Plan,
		scripts: &scriptRunsComposition{input: input, state: scriptCompositionAdmitted},
	}, nil
}

// scriptRunServices are borrowed from Run. This function never starts or
// closes them and never constructs a second command, trigger or template
// service. The Run owner drains every service after its controller has joined.
type scriptRunServices struct {
	host       *integrations.Manager
	projection *sequencer.Sequencer
	commands   *integrations.DurableCommandService
	triggers   *automationtriggers.Service
	templates  *reactivetemplate.Service
}

func prepareScriptRuns(ctx context.Context, composition *AutomationRunsComposition, services scriptRunServices) (*automationruns.Controller, error) {
	if nilReactiveInterface(ctx) || composition == nil {
		return nil, ErrInvalidTriggerComposition
	}
	composition.mu.Lock()
	defer composition.mu.Unlock()
	concrete := composition.scripts
	if concrete == nil || concrete.state != scriptCompositionAdmitted || composition.runtime != nil {
		return nil, ErrInvalidTriggerComposition
	}
	// Failed preparation is terminal for this declaration. Retrying requires
	// a fresh composition, as does starting a new server process.
	concrete.state = scriptCompositionFailed
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if services.host == nil || services.projection == nil || services.commands == nil || services.templates == nil ||
		!services.templates.SupportsScriptWaits(concrete.input.Zone.String()) {
		return nil, ErrInvalidTriggerComposition
	}
	var sceneService *scenes.Service
	if concrete.input.Scenes != nil {
		service, err := scenes.NewService(*concrete.input.Scenes, services.projection, concrete.input.Commands.generation, services.commands, concrete.input.Clock)
		if err != nil {
			return nil, errors.Join(ErrInvalidTriggerComposition, err)
		}
		concrete.scenes = &scriptSceneRuntime{service: service, done: make(chan struct{})}
		sceneService = service
	}
	adapter, err := scriptcommands.New(scriptcommands.Input{
		Commands:        services.commands,
		Scenes:          sceneService,
		Host:            services.host,
		Projection:      services.projection,
		Generation:      concrete.input.Commands.generation,
		Clock:           concrete.input.Clock,
		CommandDeadline: concrete.input.CommandDeadline,
	})
	if err != nil {
		return nil, errors.Join(ErrInvalidTriggerComposition, err)
	}
	interpreter, err := automationruns.NewInterpreter(automationruns.InterpreterInput{
		Plan:          concrete.input.Plan,
		TriggerWaits:  services.triggers,
		TemplateWaits: services.templates,
		Conditions:    services.templates,
		Authority:     concrete.input.Authority,
		Commands:      adapter,
		Clock:         concrete.input.Clock,
		Zone:          concrete.input.Zone,
	})
	if err != nil {
		return nil, errors.Join(ErrInvalidTriggerComposition, err)
	}
	var reader automationruns.ResultReader
	if services.triggers != nil {
		reader = triggerResultReader{services.triggers}
	}
	controller, err := automationruns.NewController(composition.plan, reader, interpreter, interpreter)
	if err != nil {
		return nil, errors.Join(ErrInvalidTriggerComposition, err)
	}
	composition.runtime = controller
	concrete.interpreter, concrete.state = interpreter, scriptCompositionPrepared
	return controller, nil
}

// NodeTraces observes the same interpreter that executes this composition.
// It confers no invocation, mutable service or alternate trigger-reader port.
func (c *AutomationRunsComposition) NodeTraces() ([]automationruns.NodeTrace, uint64) {
	if c == nil {
		return nil, 0
	}
	c.mu.Lock()
	var interpreter *automationruns.Interpreter
	if c.scripts != nil {
		interpreter = c.scripts.interpreter
	}
	c.mu.Unlock()
	return interpreter.NodeTraces()
}

// resolveDurableCommands admits identity once, before acquiring Host's runtime.
// A job and script composition must borrow the exact same declaration. There
// is no comparison of interface values, functions, clocks or generations.
func resolveDurableCommands(plan RuntimePlan) (*DurableCommandsComposition, error) {
	commands := plan.DurableCommands
	if plan.DurableJobs != nil && plan.DurableJobs.commands != nil {
		if commands == nil {
			commands = plan.DurableJobs.commands
		} else if commands != plan.DurableJobs.commands {
			return nil, ErrInvalidTriggerComposition
		}
	}
	if plan.AutomationRuns != nil && plan.AutomationRuns.scripts != nil {
		if plan.DurableCommands == nil || commands != plan.AutomationRuns.scripts.input.Commands {
			return nil, ErrInvalidTriggerComposition
		}
	}
	if commands != nil && (nilReactiveInterface(commands.generation) || nilReactiveInterface(commands.clock) || commands.clock.Now().IsZero()) {
		return nil, ErrInvalidTriggerComposition
	}
	return commands, nil
}

func prepareDurableCommands(ctx context.Context, host *integrations.Manager, store *storage.Store, composition *DurableCommandsComposition) (*integrations.DurableCommandService, error) {
	if composition == nil {
		return nil, nil
	}
	return integrations.NewDurableCommandServiceWithClock(ctx, host, composition.generation, store, composition.clock)
}

// Scene.Close waits for real applications. Keep its wait outside composition
// and server locks, and retain its Done alongside the controller/Host owners.
type scriptSceneRuntime struct {
	service *scenes.Service
	once    sync.Once
	done    chan struct{}
}

func (r *scriptSceneRuntime) beginClose() <-chan struct{} {
	if r == nil {
		return nil
	}
	r.once.Do(func() { go func() { r.service.Close(); close(r.done) }() })
	return r.done
}

func scriptScenesRuntime(composition *AutomationRunsComposition) *scriptSceneRuntime {
	if composition == nil {
		return nil
	}
	composition.mu.Lock()
	defer composition.mu.Unlock()
	if composition.scripts == nil {
		return nil
	}
	return composition.scripts.scenes
}

func beginCloseScriptScenes(composition *AutomationRunsComposition) <-chan struct{} {
	return scriptScenesRuntime(composition).beginClose()
}

func drainScriptScenes(ctx context.Context, composition *AutomationRunsComposition) error {
	done := beginCloseScriptScenes(composition)
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
