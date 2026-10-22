package automationruns

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/automationtriggers"
)

type nha055DormantCommands struct{ calls atomic.Int32 }

func (p *nha055DormantCommands) ExecuteCall(context.Context, ir.Call, CommandExecution) error {
	p.calls.Add(1)
	return errors.New("NHA055 dormant fixture must never dispatch a command")
}

func nha055DormantScriptPlan(t *testing.T) (Plan, ir.ScriptID) {
	t.Helper()
	catalogue, err := ir.NewCatalogue(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ir.ParseScriptID("script.nha055_next")
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err = catalogue.WithScripts([]ir.ScriptID{id})
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.NewSourceFile("dormant-next.yaml", []byte("sequence:\n - stop: complete\n"))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := ir.CompileScriptSources("script.nha055_next", "dormant-next.yaml", []parser.SourceFile{file}, catalogue)
	if err != nil || len(compiled.Rejected()) != 0 || len(compiled.Accepted()) != 1 {
		t.Fatal("real compiler rejected a source with no provider actions", err)
	}
	plan, err := NewPlanWithScripts(nil, compiled.Accepted())
	if err != nil {
		t.Fatal(err)
	}
	return plan, compiled.Accepted()[0].ID()
}

func TestNHA055DormantControllerUsesRealInterpreterAndKeepsInitialAdmissionStrict(t *testing.T) {
	empty := NewEmptyActivationPlan()
	conditions := nha042SharedConditions(t)
	commands := &nha055DormantCommands{}
	interpreter, err := NewInterpreter(InterpreterInput{Plan: empty, Conditions: conditions, Commands: commands, Clock: automationtriggers.RealClock{}, Zone: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewController(empty, nil, interpreter, interpreter); !errors.Is(err, ErrInvalidPlan) {
		t.Fatal("ordinary constructor silently admitted an empty initial program", err)
	}
	if _, err := NewDormantController(empty, nil, nil); !errors.Is(err, ErrInvalidPlan) {
		t.Fatal("dormant constructor accepted no interpreter", err)
	}
	if _, err := NewDormantController(empty, nil, &Interpreter{}); !errors.Is(err, ErrInvalidPlan) {
		t.Fatal("zero interpreter was treated as executable composition", err)
	}
	if _, err := NewDormantController(Plan{}, nil, interpreter); !errors.Is(err, ErrInvalidPlan) {
		t.Fatal("zero plan was treated as admitted dormancy", err)
	}
	active, _ := nha055DormantScriptPlan(t)
	if _, err := NewDormantController(active, nil, interpreter); !errors.Is(err, ErrInvalidPlan) {
		t.Fatal("dormant constructor accepted active definitions", err)
	}
	controller, err := NewDormantController(empty, nil, interpreter)
	if err != nil {
		t.Fatal("real interpreter did not bind to the explicit dormant controller", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := controller.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if controller.active == nil || controller.active.interpreter != interpreter || interpreter.controller != controller || len(controller.plan.AutomationIDs())+len(controller.plan.ScriptIDs()) != 0 {
		t.Fatal("dormancy used a placeholder or lost its real interpreter")
	}
	if traces, evicted := controller.Traces(); len(traces) != 0 || evicted != 0 || commands.calls.Load() != 0 {
		t.Fatal("dormant startup executed work")
	}
}

func TestNHA055DormantControllerPublishesFutureProgramInTheSameControllerWithoutExecutingIt(t *testing.T) {
	empty := NewEmptyActivationPlan()
	conditions := nha042SharedConditions(t)
	commands := &nha055DormantCommands{}
	interpreter, err := NewInterpreter(InterpreterInput{Plan: empty, Conditions: conditions, Commands: commands, Clock: automationtriggers.RealClock{}, Zone: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewDormantController(empty, nil, interpreter)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := controller.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := controller.generation
	plan, id := nha055DormantScriptPlan(t)
	reserved, err := controller.ReserveProgram(plan, automationtriggers.Plan{})
	if err != nil {
		t.Fatal("existing publication port could not prepare a dormant-to-active image", err)
	}
	defer func() { _ = reserved.Release() }()
	if err := reserved.Publish(); err != nil {
		t.Fatal(err)
	}
	if controller.generation == before || controller.active == nil || controller.active.interpreter == interpreter || controller.active.interpreter.controller != controller {
		t.Fatal("publication replaced the controller or reused an already-bound interpreter")
	}
	if ids := controller.plan.ScriptIDs(); len(ids) != 1 || ids[0] != id || len(controller.plan.AutomationIDs()) != 0 {
		t.Fatal("published program did not become the current image")
	}
	if traces, evicted := controller.Traces(); len(traces) != 0 || evicted != 0 || commands.calls.Load() != 0 {
		t.Fatal("publishing a future image replayed or executed its program")
	}
}
