package server

import (
	"context"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/automationruns"
)

// Run supplies the real public admission bounds to its one NHA-024 service.
// The 040/044 handles and Controller remain live while a new action waits at
// the activation gate; no pilot or PostgreSQL intention is fabricated.
func TestNHA029GateRetainsReal040And044WaitsBeforeCommandAdmission(t *testing.T) {
	environment := nha042ServerEnvironmentFor(t)
	parent := "- action: script.turn_on\n  target: {entity_id: script.nha042_child}\n  data: {}\n- wait_template: '{{ false }}'\n"
	child := "- wait_for_trigger:\n  - trigger: event\n    event_type: nha042_release\n" + nha042ServerCommand
	rig := nha042StartServer(t, environment, parent, child, nha042ServerOptions{sharedJobs: true})
	rig.run.waitReady(t)
	nha034WaitSignal(t, rig.pilot.ready, "simulated pilot startup")
	commands, present := rig.jobServices.Commands()
	if !present || commands == nil {
		t.Fatal("Run did not lend its single durable command service")
	}
	rig.pilot.emit(t, "nha042_start")
	nha042ServerWait(t, "real 040 and 044 waits", func() bool {
		return nha042ServerNode(rig.composition, "script.nha042_parent", ir.ActionWaitTemplate, automationruns.NodeWaiting) &&
			nha042ServerNode(rig.composition, "script.nha042_child", ir.ActionWait, automationruns.NodeWaiting)
	})
	pauseCtx, cancelPause := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelPause()
	pause, err := commands.PauseAndDrain(pauseCtx)
	if err != nil {
		t.Fatal(err)
	}
	rig.pilot.emit(t, "nha042_release")
	nha042ServerWait(t, "wait release entered command node", func() bool {
		return nha042ServerNode(rig.composition, "script.nha042_child", ir.ActionCall, automationruns.NodeEntered)
	})
	if !nha042ServerNode(rig.composition, "script.nha042_parent", ir.ActionWaitTemplate, automationruns.NodeWaiting) {
		t.Fatal("activation pause canceled an unaffected 044 wait")
	}
	if effects := rig.driver.snapshot(); len(effects) != 0 {
		t.Fatalf("driver entered before activation resolved: %d", len(effects))
	}
	var intentions int
	queryCtx, stopQuery := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopQuery()
	if err := environment.observer.QueryRow(queryCtx, `SELECT count(*) FROM public.command_intents WHERE origin_kind='internal_automation'`).Scan(&intentions); err != nil || intentions != 0 {
		t.Fatalf("paused waited action claimed SQL ID: count=%d err=%v", intentions, err)
	}
	if err := commands.ResumeOld(pause); err != nil {
		t.Fatal(err)
	}
	nha042ServerWait(t, "same waited action completed after resolution", func() bool {
		return nha042ServerNode(rig.composition, "script.nha042_child", ir.ActionCall, automationruns.NodeCompleted)
	})
	ids := rig.driver.snapshot()
	if len(ids) != 1 {
		t.Fatalf("waited action emitted more than once: %d", len(ids))
	}
	state, _ := nha042ServerIntent(t, environment.observer, ids[0])
	if state != "result" {
		t.Fatalf("waited effect lacked durable result: %s", state)
	}
	rig.run.cancel()
	if err := rig.run.waitDone(t); err != nil {
		t.Fatal(err)
	}
	nha034WaitSignal(t, rig.composition.Done(), "run joined after gate resolution")
	nha034WaitRunStoreClosed(t, rig.store)
}

// A detached child must acknowledge its first suspension before waiting at
// the activation gate. Otherwise script.turn_on holds its parent before the
// parent's real 044 wait even though neither run needs a new physical effect.
func TestNHA029GateSuspendsDetachedChildBeforeFirstCommandAdmission(t *testing.T) {
	environment := nha042ServerEnvironmentFor(t)
	parent := "- action: script.turn_on\n  target: {entity_id: script.nha042_child}\n  data: {}\n- wait_template: '{{ false }}'\n"
	rig := nha042StartServer(t, environment, parent, nha042ServerCommand, nha042ServerOptions{sharedJobs: true})
	rig.run.waitReady(t)
	nha034WaitSignal(t, rig.pilot.ready, "simulated pilot startup")
	commands, present := rig.jobServices.Commands()
	if !present || commands == nil {
		t.Fatal("Run did not lend its single durable command service")
	}
	pauseCtx, cancelPause := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelPause()
	pause, err := commands.PauseAndDrain(pauseCtx)
	if err != nil {
		t.Fatal(err)
	}
	resolved := false
	t.Cleanup(func() {
		if !resolved {
			if err := commands.ResumeOld(pause); err != nil {
				t.Errorf("unresolved activation pause during cleanup: %v", err)
			}
		}
	})
	rig.pilot.emit(t, "nha042_start")
	nha042ServerWait(t, "detached child startup and parent's real 044 wait", func() bool {
		return nha042ServerNode(rig.composition, "script.nha042_child", ir.ActionCall, automationruns.NodeEntered) &&
			nha042ServerNode(rig.composition, "script.nha042_parent", ir.ActionWaitTemplate, automationruns.NodeWaiting)
	})
	var parentRun, childRun automationruns.RunID
	traces, _ := rig.composition.NodeTraces()
	for _, trace := range traces {
		if trace.Source().Object() == "script.nha042_parent" && trace.Kind() == ir.ActionWaitTemplate && trace.Stage() == automationruns.NodeWaiting {
			parentRun = trace.RunID()
		}
		if trace.Source().Object() == "script.nha042_child" && trace.Kind() == ir.ActionCall && trace.Stage() == automationruns.NodeEntered {
			childRun = trace.RunID()
		}
	}
	if parentRun == 0 || childRun == 0 || parentRun == childRun {
		t.Fatalf("detached parent/child lacked distinct live runs: parent=%d child=%d", parentRun, childRun)
	}
	if effects := rig.driver.snapshot(); len(effects) != 0 {
		t.Fatalf("driver entered before activation resolved: %d", len(effects))
	}
	var intentions int
	queryCtx, stopQuery := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopQuery()
	if err := environment.observer.QueryRow(queryCtx, `SELECT count(*) FROM public.command_intents WHERE origin_kind='internal_automation'`).Scan(&intentions); err != nil || intentions != 0 {
		t.Fatalf("blocked child claimed SQL intention: count=%d err=%v", intentions, err)
	}
	if err := commands.ResumeOld(pause); err != nil {
		t.Fatal(err)
	}
	resolved = true
	nha042ServerWait(t, "same detached child completes its first command", func() bool {
		traces, _ := rig.composition.NodeTraces()
		for _, trace := range traces {
			if trace.Source().Object() == "script.nha042_child" && trace.RunID() == childRun &&
				trace.Kind() == ir.ActionCall && trace.Stage() == automationruns.NodeCompleted {
				return true
			}
		}
		return false
	})
	traces, _ = rig.composition.NodeTraces()
	parentWaitStage := automationruns.NodeStageInvalid
	for _, trace := range traces {
		if trace.Source().Object() == "script.nha042_parent" && trace.RunID() == parentRun &&
			trace.Kind() == ir.ActionWaitTemplate {
			parentWaitStage = trace.Stage()
		}
	}
	if parentWaitStage != automationruns.NodeWaiting {
		t.Fatalf("activation resolution replaced or canceled the parent's 044 wait: latest stage=%d", parentWaitStage)
	}
	ids := rig.driver.snapshot()
	if len(ids) != 1 {
		t.Fatalf("detached child's first command emitted %d effects", len(ids))
	}
	state, _ := nha042ServerIntent(t, environment.observer, ids[0])
	if state != "result" {
		t.Fatalf("detached effect lacked durable result: %s", state)
	}
	rig.run.cancel()
	if err := rig.run.waitDone(t); err != nil {
		t.Fatal(err)
	}
	nha034WaitSignal(t, rig.composition.Done(), "parent and child joined after activation resolution")
	nha034WaitRunStoreClosed(t, rig.store)
}
