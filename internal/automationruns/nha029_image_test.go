package automationruns

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/automationtriggers"
)

type nha029ImageCall struct {
	id    RunID
	alias string
}

type nha029ImageCommands struct {
	mu      sync.Mutex
	entered chan nha029ImageCall
	release map[RunID]chan struct{}
}

func (g *nha029ImageCommands) ExecuteCall(ctx context.Context, call ir.Call, execution CommandExecution) error {
	if _, err := execution.CausalKey(); err != nil {
		return err
	}
	execution.Suspend()
	name, _ := call.Targets()[0].Alias().Value()
	g.mu.Lock()
	done := make(chan struct{})
	g.release[execution.run] = done
	g.mu.Unlock()
	g.entered <- nha029ImageCall{id: execution.run, alias: name}
	select {
	case <-done:
		return ctx.Err()
	case <-ctx.Done():
		<-done
		return ctx.Err()
	}
}
func (g *nha029ImageCommands) open(id RunID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if done := g.release[id]; done != nil {
		select {
		case <-done:
		default:
			close(done)
		}
	}
}
func (g *nha029ImageCommands) openAll() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, done := range g.release {
		select {
		case <-done:
		default:
			close(done)
		}
	}
}

func TestNHA029QueuedAImageAndFreshBImageUseOwnPrograms(t *testing.T) {
	id, _ := ir.ParseScriptID("script.nha029_image")
	ids := []ir.ScriptID{id}
	old := nha042Compile(t, "script.nha029_image", "mode: queued\nmax: 3\nsequence:\n"+nha042Action("child_enter"), ids)
	newScript := nha042Compile(t, "script.nha029_image", "mode: queued\nmax: 3\nsequence:\n"+nha042Action("child_exit"), ids)
	oldPlan, err := NewPlanWithScripts(nil, []ir.Script{old})
	if err != nil {
		t.Fatal(err)
	}
	commands := &nha029ImageCommands{entered: make(chan nha029ImageCall, 3), release: make(map[RunID]chan struct{})}
	conditions := nha042SharedConditions(t)
	oldInterpreter, err := NewInterpreter(InterpreterInput{Plan: oldPlan, Conditions: conditions,
		Commands: commands, Clock: automationtriggers.RealClock{}, Zone: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewController(oldPlan, nil, oldInterpreter, oldInterpreter)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { commands.openAll(); _ = controller.Close(context.Background()) })
	first, err := controller.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDetached)
	if err != nil {
		t.Fatal(err)
	}
	if got := nha041Receive(t, commands.entered); got.id != first.ID() || got.alias != "switch.nha042_child_enter" {
		t.Fatalf("first A call: %+v", got)
	}
	second, err := controller.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDetached)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := second.Startup(context.Background()); err != nil || outcome.Kind() != StartupQueuedChange {
		t.Fatalf("A queue admission: %+v %v", outcome, err)
	}
	newPlan, err := NewPlanWithScripts(nil, []ir.Script{newScript})
	if err != nil {
		t.Fatal(err)
	}
	newInterpreter, err := NewInterpreter(InterpreterInput{Plan: newPlan, Conditions: conditions,
		Commands: commands, Clock: automationtriggers.RealClock{}, Zone: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := controller.ReserveImage(newPlan, newInterpreter, newInterpreter)
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Publish(); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Publish(); err != ErrInvalidPlan {
		t.Fatalf("published image accepted a second publication: %v", err)
	}
	if first.entry.run.Generation() != second.entry.run.Generation() || first.entry.run.Generation() == controller.generation {
		t.Fatal("causal generation changed for queued A run")
	}
	commands.open(first.ID())
	if err := first.Result(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := nha041Receive(t, commands.entered); got.id != second.ID() || got.alias != "switch.nha042_child_enter" {
		t.Fatalf("queued A was rebound to B: %+v", got)
	}
	third, err := controller.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDetached)
	if err != nil {
		t.Fatal(err)
	}
	if third.entry.run.Generation() != controller.generation {
		t.Fatal("new admission did not select B")
	}
	commands.open(second.ID())
	if err := second.Result(context.Background()); err != nil {
		t.Fatal(err)
	}
	nha042Await(t, func() bool {
		oldInterpreter.mu.Lock()
		defer oldInterpreter.mu.Unlock()
		return oldInterpreter.image == nil
	})
	if got := nha041Receive(t, commands.entered); got.id != third.ID() || got.alias != "switch.nha042_child_exit" {
		t.Fatalf("fresh B call: %+v", got)
	}
	commands.open(third.ID())
	if err := third.Result(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNHA029ParentAfterWaitKeepsDirectAndDetachedChildA(t *testing.T) {
	for _, mode := range []struct {
		name, call string
	}{
		{"direct", "- action: script.nha042_child\n"},
		{"detached", "- action: script.turn_on\n  target: {entity_id: script.nha042_child}\n"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			parentID, _ := ir.ParseScriptID("script.nha042_parent")
			childID, _ := ir.ParseScriptID("script.nha042_child")
			ids := []ir.ScriptID{parentID, childID}
			parent := nha042Compile(t, "script.nha042_parent", "sequence:\n- wait_template: \"{{ is_state('switch.fixture', 'on') }}\"\n"+mode.call, ids)
			childA := nha042Compile(t, "script.nha042_child", "sequence:\n"+nha042Action("child_enter"), ids)
			childB := nha042Compile(t, "script.nha042_child", "sequence:\n"+nha042Action("child_exit"), ids)
			commands := &nha042Commands{}
			rig := nha042FullRig(t, []ir.Script{parent, childA}, commands)
			handle, err := rig.controller.InvokeScript(context.Background(), parentID, nil, Run{}, ir.ScriptCallDirect)
			if err != nil {
				t.Fatal(err)
			}
			nha042Await(t, func() bool { return rig.waiting(handle.ID()) })
			select {
			case <-handle.Done():
				t.Fatal("A parent terminated before replacement")
			default:
			}
			_, entity, err := rig.binding.IDs()
			if err != nil {
				t.Fatal(err)
			}
			triggerPlan, err := automationtriggers.NewPlan(automationtriggers.PlanInput{Scripts: []ir.Script{parent, childB}, Zone: "UTC", Resolver: nha042Resolver{entity}})
			if err != nil {
				t.Fatal(err)
			}
			planB, err := NewPlanWithScripts(nil, []ir.Script{parent, childB})
			if err != nil {
				t.Fatal(err)
			}
			interpreterB, err := NewInterpreter(InterpreterInput{Plan: planB, TriggerPlan: triggerPlan,
				TriggerWaits: rig.interpreter.input.TriggerWaits, TemplateWaits: rig.interpreter.input.TemplateWaits,
				Conditions: rig.interpreter.input.Conditions, Commands: commands, Clock: rig.clock, Zone: time.UTC})
			if err != nil {
				t.Fatal(err)
			}
			reserved, err := rig.controller.ReserveImage(planB, interpreterB, interpreterB)
			if err != nil {
				t.Fatal(err)
			}
			if err := reserved.PrepareTriggerPublication(); err != nil {
				t.Fatal(err)
			}
			if err := reserved.PublishTriggers(); err != nil {
				t.Fatal(err)
			}
			if err := reserved.Publish(); err != nil {
				t.Fatal(err)
			}
			if err := reserved.SchedulePublishedTriggers(); err != nil {
				t.Fatal(err)
			}
			rig.state(t, true)
			if err := nha042Joined(t, handle); err != nil {
				t.Fatal(err)
			}
			commands.mu.Lock()
			seen := append([]string(nil), commands.aliases...)
			commands.mu.Unlock()
			if len(seen) != 1 || seen[0] != "switch.nha042_child_enter" {
				t.Fatalf("A parent invoked B child: %v", seen)
			}
			fresh, err := rig.controller.InvokeScript(context.Background(), childID, nil, Run{}, ir.ScriptCallDirect)
			if err != nil {
				t.Fatal(err)
			}
			if err := nha042Joined(t, fresh); err != nil {
				t.Fatal(err)
			}
			commands.mu.Lock()
			seen = append([]string(nil), commands.aliases...)
			commands.mu.Unlock()
			if len(seen) != 2 || seen[1] != "switch.nha042_child_exit" {
				t.Fatalf("fresh child did not use B: %v", seen)
			}
		})
	}
}

func TestNHA029RestartReplacementKeepsItsAImageAfterB(t *testing.T) {
	id, _ := ir.ParseScriptID("script.nha029_restart_image")
	ids := []ir.ScriptID{id}
	a := nha042Compile(t, "script.nha029_restart_image", "mode: restart\nsequence:\n"+nha042Action("child_enter"), ids)
	b := nha042Compile(t, "script.nha029_restart_image", "mode: restart\nsequence:\n"+nha042Action("child_exit"), ids)
	planA, err := NewPlanWithScripts(nil, []ir.Script{a})
	if err != nil {
		t.Fatal(err)
	}
	commands := &nha029ImageCommands{entered: make(chan nha029ImageCall, 3), release: make(map[RunID]chan struct{})}
	conditions := nha042SharedConditions(t)
	interpreterA, err := NewInterpreter(InterpreterInput{Plan: planA, Conditions: conditions,
		Commands: commands, Clock: automationtriggers.RealClock{}, Zone: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewController(planA, nil, interpreterA, interpreterA)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { commands.openAll(); _ = controller.Close(context.Background()) })
	first, err := controller.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDetached)
	if err != nil {
		t.Fatal(err)
	}
	if got := nha041Receive(t, commands.entered); got.id != first.ID() || got.alias != "switch.nha042_child_enter" {
		t.Fatalf("first A call: %+v", got)
	}
	replacement, err := controller.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDetached)
	if err != nil {
		t.Fatal(err)
	}
	planB, err := NewPlanWithScripts(nil, []ir.Script{b})
	if err != nil {
		t.Fatal(err)
	}
	interpreterB, err := NewInterpreter(InterpreterInput{Plan: planB, Conditions: conditions,
		Commands: commands, Clock: automationtriggers.RealClock{}, Zone: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := controller.ReserveImage(planB, interpreterB, interpreterB)
	if err != nil {
		t.Fatal(err)
	}
	if err := reserved.Publish(); err != nil {
		t.Fatal(err)
	}
	commands.open(first.ID())
	// The interpreter keeps the command failure kind and wraps its cancelled cause.
	if err := first.Result(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("restarted A terminal: %v", err)
	}
	if got := nha041Receive(t, commands.entered); got.id != replacement.ID() || got.alias != "switch.nha042_child_enter" {
		t.Fatalf("replacement was rebound to B: %+v", got)
	}
	commands.open(replacement.ID())
	if err := replacement.Result(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNHA029EmptyBRetainsParentAClosureUntilItsChildFinishes(t *testing.T) {
	parentID, _ := ir.ParseScriptID("script.nha042_parent")
	childID, _ := ir.ParseScriptID("script.nha042_child")
	ids := []ir.ScriptID{parentID, childID}
	parent := nha042Compile(t, "script.nha042_parent", "sequence:\n- wait_template: \"{{ is_state('switch.fixture', 'on') }}\"\n- action: script.nha042_child\n", ids)
	child := nha042Compile(t, "script.nha042_child", "sequence:\n"+nha042Action("child_enter"), ids)
	commands := &nha042Commands{}
	rig := nha042FullRig(t, []ir.Script{parent, child}, commands)
	handle, err := rig.controller.InvokeScript(context.Background(), parentID, nil, Run{}, ir.ScriptCallDirect)
	if err != nil {
		t.Fatal(err)
	}
	nha042Await(t, func() bool { return rig.waiting(handle.ID()) })
	emptyTrigger, err := automationtriggers.NewEmptyActivationPlan("UTC")
	if err != nil {
		t.Fatal(err)
	}
	emptyRuns := NewEmptyActivationPlan()
	emptyInterpreter, err := NewInterpreter(InterpreterInput{Plan: emptyRuns, TriggerPlan: emptyTrigger,
		TriggerWaits: rig.interpreter.input.TriggerWaits, TemplateWaits: rig.interpreter.input.TemplateWaits,
		Conditions: rig.interpreter.input.Conditions, Commands: commands, Clock: rig.clock, Zone: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := rig.controller.ReserveImage(emptyRuns, emptyInterpreter, emptyInterpreter)
	if err != nil {
		t.Fatal(err)
	}
	if err := reserved.PrepareTriggerPublication(); err != nil {
		t.Fatal(err)
	}
	if err := reserved.PublishTriggers(); err != nil {
		t.Fatal(err)
	}
	if err := reserved.Publish(); err != nil {
		t.Fatal(err)
	}
	if err := reserved.SchedulePublishedTriggers(); err != nil {
		t.Fatal(err)
	}
	rig.state(t, true)
	if err := nha042Joined(t, handle); err != nil {
		t.Fatal(err)
	}
	commands.mu.Lock()
	seen := append([]string(nil), commands.aliases...)
	commands.mu.Unlock()
	if len(seen) != 1 || seen[0] != "switch.nha042_child_enter" {
		t.Fatalf("A closure lost after empty B: %v", seen)
	}
	if fresh, err := rig.controller.InvokeScript(context.Background(), childID, nil, Run{}, ir.ScriptCallDirect); fresh != nil || err == nil {
		t.Fatalf("empty B admitted removed child: %v", err)
	}
}
