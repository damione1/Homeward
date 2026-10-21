package automationruns

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/automationtriggers"
)

func nha029ImageFixture(t *testing.T) (*Controller, func() (Plan, *Interpreter)) {
	t.Helper()
	id, err := ir.ParseScriptID("script.nha029_image_fence")
	if err != nil {
		t.Fatal(err)
	}
	script := nha042Compile(t, "script.nha029_image_fence", "sequence:\n"+nha042Action("child_enter"), []ir.ScriptID{id})
	conditions := nha042SharedConditions(t)
	newImage := func() (Plan, *Interpreter) {
		t.Helper()
		plan, err := NewPlanWithScripts(nil, []ir.Script{script})
		if err != nil {
			t.Fatal(err)
		}
		interpreter, err := NewInterpreter(InterpreterInput{Plan: plan, Conditions: conditions,
			Commands: &nha029ImageCommands{entered: make(chan nha029ImageCall, 1), release: make(map[RunID]chan struct{})},
			Clock:    automationtriggers.RealClock{}, Zone: time.UTC})
		if err != nil {
			t.Fatal(err)
		}
		return plan, interpreter
	}
	plan, interpreter := newImage()
	controller, err := NewController(plan, nil, interpreter, interpreter)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close(context.Background()) })
	return controller, newImage
}

func TestNHA029BranchEnvironmentRequiresLiveRunArenaMembership(t *testing.T) {
	parentID, _ := ir.ParseScriptID("script.nha029_branch_parent")
	childID, _ := ir.ParseScriptID("script.nha029_branch_child")
	ids := []ir.ScriptID{parentID, childID}
	parent := nha042Compile(t, "script.nha029_branch_parent", "mode: parallel\nmax: 4\nsequence:\n- wait_template: '{{ false }}'\n", ids)
	child := nha042Compile(t, "script.nha029_branch_child", "sequence:\n- stop: done\n", ids)
	rig := nha042FullRig(t, []ir.Script{parent, child}, &nha042Commands{})
	invokeParent := func() *ScriptHandle {
		t.Helper()
		handle, err := rig.controller.InvokeScript(context.Background(), parentID, nil, Run{}, ir.ScriptCallDirect)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		startup, err := handle.Startup(ctx)
		if err != nil || startup.Kind() != StartupSuspended {
			t.Fatalf("real parent did not suspend: %v %v", startup.Kind(), err)
		}
		return handle
	}
	first, second := invokeParent(), invokeParent()
	run := first.entry.run
	fork, err := run.env.ForkBranch()
	if err != nil {
		t.Fatal(err)
	}
	defer fork.closeBranch()
	branch := run
	branch.env = fork
	joinedChild, err := rig.controller.InvokeScript(context.Background(), childID, nil, branch, ir.ScriptCallDirect)
	if err != nil {
		t.Fatalf("registered live branch could not call child: %v", err)
	}
	if err := nha042Joined(t, joinedChild); err != nil {
		t.Fatal(err)
	}
	if joinedChild.entry.image != first.entry.image || joinedChild.entry.run.Generation() != run.Generation() {
		t.Fatal("branch child lost its parent's causal image")
	}
	closedFork, err := run.env.ForkBranch()
	if err != nil {
		t.Fatal(err)
	}
	closedFork.closeBranch()
	if second.entry.arena == first.entry.arena || second.entry.arena.pool != first.entry.arena.pool {
		t.Fatal("foreign-run fixture did not share only the Controller pool")
	}
	refuse := func(t *testing.T, parent Run) {
		t.Helper()
		rig.controller.pool.mu.Lock()
		before := rig.controller.pool.used
		rig.controller.pool.mu.Unlock()
		handle, err := rig.controller.InvokeScript(context.Background(), childID, nil, parent, ir.ScriptCallDirect)
		if handle != nil || err != ErrInvalidPlan {
			t.Fatalf("unowned parent environment admitted: %v %v", handle, err)
		}
		rig.controller.pool.mu.Lock()
		after := rig.controller.pool.used
		rig.controller.pool.mu.Unlock()
		if after != before || len(rig.effects()) != 0 {
			t.Fatal("rejected environment changed retained charge or emitted an effect")
		}
	}
	for _, test := range []struct {
		name string
		env  *Environment
	}{
		{"other_run_same_pool", second.entry.run.env},
		{"unregistered_same_arena", &Environment{arena: first.entry.arena}},
		{"closed_registered_fork", closedFork},
		{"nil_environment", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := run
			invalid.env = test.env
			refuse(t, invalid)
		})
	}
	<-first.Cancel()
	if err := nha042Joined(t, first); err != nil {
		t.Fatal(err)
	}
	t.Run("joined_parent", func(t *testing.T) { refuse(t, branch) })
	<-second.Cancel()
	if err := nha042Joined(t, second); err != nil {
		t.Fatal(err)
	}
}

func TestNHA029ReleasedInterpreterRejectsRebindAndUnreservedEmission(t *testing.T) {
	for _, publishB := range []bool{false, true} {
		name := "close"
		if publishB {
			name = "publish_B"
		}
		t.Run(name, func(t *testing.T) {
			id, _ := ir.ParseScriptID("script.nha029_released_interpreter")
			program := nha042Compile(t, "script.nha029_released_interpreter", "sequence:\n- stop: done\n", []ir.ScriptID{id})
			controller, interpreter := nha042Runtime(t, []ir.Script{program}, &nha042Commands{})
			planA := interpreter.input.Plan
			handle, err := controller.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDirect)
			if err != nil {
				t.Fatal(err)
			}
			if err := nha042Joined(t, handle); err != nil {
				t.Fatal(err)
			}
			// Use the real bounded emitter so release must also handle a wrapped
			// ring and preserve its positive eviction count.
			for count := 0; count <= nodeTraceCapacity; count++ {
				interpreter.appendNodeTrace(NodeTrace{generation: controller.generation, stage: NodeEntered})
			}
			nodes, loss := interpreter.NodeTraces()
			nodeCopy := append([]NodeTrace(nil), nodes...)
			interpreter.mu.Lock()
			wrapped := interpreter.traceNext != 0
			interpreter.mu.Unlock()
			if !wrapped || loss == 0 || len(nodes) != nodeTraceCapacity {
				t.Fatal("release fixture did not wrap its native ring")
			}
			controller.mu.Lock()
			oldImage := controller.active
			oldCharge := oldImage.charge
			controller.mu.Unlock()
			previous := controller.traceOrdinal.Load()
			var beforeRelease, activeCharge int
			if publishB {
				planB, err := NewPlanWithScripts(nil, []ir.Script{program})
				if err != nil {
					t.Fatal(err)
				}
				inputB := interpreter.input
				inputB.Plan = planB
				interpreterB, err := NewInterpreter(inputB)
				if err != nil {
					t.Fatal(err)
				}
				reservation, err := controller.ReserveImage(planB, interpreterB, interpreterB)
				if err != nil {
					t.Fatal(err)
				}
				controller.pool.mu.Lock()
				beforeRelease = controller.pool.used
				controller.pool.mu.Unlock()
				if err := reservation.Publish(); err != nil {
					t.Fatal(err)
				}
				controller.mu.Lock()
				activeCharge = controller.active.charge
				controller.mu.Unlock()
			} else {
				controller.pool.mu.Lock()
				beforeRelease = controller.pool.used
				controller.pool.mu.Unlock()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if err := controller.Close(ctx); err != nil {
					t.Fatal(err)
				}
			}
			controller.mu.Lock()
			noOwners := oldImage.refs == 0
			controller.mu.Unlock()
			interpreter.mu.Lock()
			released := interpreter.controller == controller && interpreter.image == nil && interpreter.waitPlan == nil && interpreter.charge == 0 && cap(interpreter.traces) == 0 && interpreter.traceNext == 0
			interpreter.mu.Unlock()
			afterNodes, afterLoss := interpreter.NodeTraces()
			controller.pool.mu.Lock()
			afterRelease := controller.pool.used
			controller.pool.mu.Unlock()
			if !noOwners || !released || len(afterNodes) != 0 || afterLoss != loss || afterRelease != beforeRelease-oldCharge || afterRelease != controllerCharge+activeCharge || controller.traceOrdinal.Load() != previous || !reflect.DeepEqual(nodes, nodeCopy) {
				t.Fatalf("last A reference did not release its real image: refs=%v released=%v before=%d image=%d after=%d active=%d", noOwners, released, beforeRelease, oldCharge, afterRelease, activeCharge)
			}
			if allocations := testing.AllocsPerRun(100, func() { interpreter.appendNodeTrace(NodeTrace{}) }); allocations != 0 {
				t.Fatalf("released emitter allocated without a reservation: %f", allocations)
			}
			if rebound, err := NewController(planA, nil, interpreter, interpreter); rebound != nil || err != ErrInvalidPlan {
				t.Fatalf("released interpreter rebound to a foreign Controller: %v %v", rebound, err)
			}
			if publishB {
				if reservation, err := controller.ReserveImage(planA, interpreter, interpreter); reservation != nil || err != ErrInvalidPlan {
					t.Fatalf("released interpreter rebound to its original Controller: %v %v", reservation, err)
				}
			}
			afterNodes, afterLoss = interpreter.NodeTraces()
			controller.pool.mu.Lock()
			afterRefusal := controller.pool.used
			controller.pool.mu.Unlock()
			if len(afterNodes) != 0 || afterLoss != loss || cap(interpreter.traces) != 0 || interpreter.controller != controller || controller.traceOrdinal.Load() != previous || afterRefusal != afterRelease {
				t.Fatal("released append/rebind changed observation, ownership, or reserved charge")
			}
		})
	}
}

func TestNHA029BuildingCandidateSerializesConcurrentAdmission(t *testing.T) {
	controller, newImage := nha029ImageFixture(t)
	firstPlan, firstInterpreter := newImage()
	firstInterpreter.mu.Lock() // bindImage cannot finish while admission is observed.
	locked := true
	defer func() {
		if locked {
			firstInterpreter.mu.Unlock()
		}
	}()
	type result struct {
		reservation *ImageReservation
		err         error
	}
	finished := make(chan result, 1)
	go func() {
		reservation, err := controller.ReserveImage(firstPlan, firstInterpreter, firstInterpreter)
		finished <- result{reservation, err}
	}()
	nha042Await(t, func() bool {
		controller.mu.Lock()
		defer controller.mu.Unlock()
		return controller.pending != nil && controller.pending.state == imageReservationBuilding
	})
	secondPlan, secondInterpreter := newImage()
	second, err := controller.ReserveImage(secondPlan, secondInterpreter, secondInterpreter)
	if second != nil || err != ErrInvalidPlan {
		t.Fatalf("concurrent candidate crossed the building gate: reservation=%v err=%v", second, err)
	}
	firstInterpreter.mu.Unlock()
	locked = false
	select {
	case outcome := <-finished:
		if outcome.err != nil || outcome.reservation == nil {
			t.Fatalf("first candidate was lost: %v", outcome.err)
		}
		if err := outcome.reservation.Release(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first candidate did not finish after bind gate opened")
	}
}

func TestNHA029CloseJoinsBuildingCandidateBeforeDone(t *testing.T) {
	controller, newImage := nha029ImageFixture(t)
	plan, interpreter := newImage()
	interpreter.mu.Lock()
	locked := true
	defer func() {
		if locked {
			interpreter.mu.Unlock()
		}
	}()
	finished := make(chan error, 1)
	go func() {
		_, err := controller.ReserveImage(plan, interpreter, interpreter)
		finished <- err
	}()
	nha042Await(t, func() bool {
		controller.mu.Lock()
		defer controller.mu.Unlock()
		return controller.pending != nil && controller.pending.state == imageReservationBuilding
	})
	done := controller.BeginClose()
	select {
	case <-done:
		t.Fatal("controller acknowledged Done while candidate still compiles")
	default:
	}
	interpreter.mu.Unlock()
	locked = false
	select {
	case err := <-finished:
		if err != ErrClosed {
			t.Fatalf("closed controller admitted building candidate: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("building candidate did not finish after close")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("controller Done did not join candidate cleanup")
	}
	controller.pool.mu.Lock()
	used := controller.pool.used
	controller.pool.mu.Unlock()
	if used != controllerCharge {
		t.Fatalf("closed candidate retained charged memory: %d", used)
	}
}

func TestNHA029ImageReservationRejectsZeroTypedNilAndReuse(t *testing.T) {
	controller, newImage := nha029ImageFixture(t)
	plan, interpreter := newImage()
	var nilInterpreter *Interpreter
	if reservation, err := controller.ReserveImage(plan, nilInterpreter, nilInterpreter); reservation != nil || err != ErrInvalidPlan {
		t.Fatalf("typed-nil interpreter admitted: reservation=%v err=%v", reservation, err)
	}
	if reservation, err := controller.ReserveImage(Plan{}, interpreter, interpreter); reservation != nil || err != ErrInvalidPlan {
		t.Fatalf("zero plan admitted: reservation=%v err=%v", reservation, err)
	}
	reservation, err := controller.ReserveImage(plan, interpreter, interpreter)
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Release(); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Release(); err != ErrInvalidPlan {
		t.Fatalf("released candidate reused: %v", err)
	}
	if err := reservation.Publish(); err != ErrInvalidPlan {
		t.Fatalf("released candidate published: %v", err)
	}
}

func TestNHA029IdleChildLaneEndsAtLastOldImageReference(t *testing.T) {
	controller, newImage := nha029ImageFixture(t)
	controller.mu.Lock()
	old := controller.active
	if !controller.retainImageLocked(old) {
		controller.mu.Unlock()
		t.Fatal("old image could not represent a retained parent")
	}
	controller.mu.Unlock()
	var child laneKey
	for key := range old.ownKeys {
		child = key
	}
	empty := NewEmptyActivationPlan()
	_, freshInterpreter := newImage()
	// A truly empty B must use its own admitted interpreter, not A's token.
	emptyInterpreter, err := NewInterpreter(InterpreterInput{Plan: empty, Conditions: freshInterpreter.input.Conditions,
		Commands: freshInterpreter.input.Commands, Clock: automationtriggers.RealClock{}, Zone: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := controller.ReserveImage(empty, emptyInterpreter, emptyInterpreter)
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Publish(); err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	_, retained := controller.lanes[child]
	controller.mu.Unlock()
	if !retained {
		t.Fatal("idle child lane vanished while an A parent still owns its image")
	}
	controller.releaseImage(old)
	controller.mu.Lock()
	_, retained = controller.lanes[child]
	controller.mu.Unlock()
	if retained {
		t.Fatal("idle A child lane survived its final image reference")
	}
}

func TestNHA029UnionBudgetReopensAfterLastOldImageReference(t *testing.T) {
	controller, newImage := nha029ImageFixture(t)
	controller.mu.Lock()
	old := controller.active
	if !controller.retainImageLocked(old) {
		controller.mu.Unlock()
		t.Fatal("old run image could not be retained")
	}
	controller.mu.Unlock()
	planB, interpreterB := newImage()
	second, err := controller.ReserveImage(planB, interpreterB, interpreterB)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Publish(); err != nil {
		t.Fatal(err)
	}
	controller.pool.mu.Lock()
	free := MaxMemoryBytes - controller.pool.used
	controller.pool.mu.Unlock()
	remaining := old.charge / 2
	if remaining <= 0 || free <= remaining {
		t.Fatalf("fixture cannot pressure retained image union: free=%d old=%d", free, old.charge)
	}
	padding := free - remaining
	if !controller.pool.adjust(0, padding) {
		t.Fatal("fixture could not reserve other admitted memory")
	}
	planC, interpreterC := newImage()
	third, err := controller.ReserveImage(planC, interpreterC, interpreterC)
	if third != nil || err != ErrCapacity {
		t.Fatalf("candidate ignored retained A charge: reservation=%v err=%v", third, err)
	}
	controller.releaseImage(old) // the final A run/child releases its image
	third, err = controller.ReserveImage(planC, interpreterC, interpreterC)
	if err != nil {
		t.Fatalf("capacity did not return after last A reference: %v", err)
	}
	if err := third.Release(); err != nil {
		t.Fatal(err)
	}
	if !controller.pool.adjust(padding, 0) {
		t.Fatal("fixture could not release competing memory")
	}
}
