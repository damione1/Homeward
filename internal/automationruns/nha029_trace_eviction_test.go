package automationruns

import (
	"context"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/observability"
	"github.com/damione1/Homeward/internal/storage"
)

func TestNHA029ObservationTraceKeepsCausalNodesAfterControllerEviction(t *testing.T) {
	for _, terminalParent := range []bool{false, true} {
		name := "live_wait_A"
		if terminalParent {
			name = "terminal_parent_with_live_child_A"
		}
		t.Run(name, func(t *testing.T) {
			parentID, _ := ir.ParseScriptID("script.nha042_parent")
			childID, _ := ir.ParseScriptID("script.nha042_child")
			noiseID, _ := ir.ParseScriptID("script.nha029_trace_noise")
			ids := []ir.ScriptID{parentID, childID, noiseID}
			parentBody := "sequence:\n- wait_template: '{{ false }}'\n"
			if terminalParent {
				parentBody = "sequence:\n- action: script.turn_on\n  target: {entity_id: script.nha042_child}\n- stop: parent_done\n"
			}
			parent := nha042Compile(t, "script.nha042_parent", parentBody, ids)
			child := nha042Compile(t, "script.nha042_child", "sequence:\n- wait_template: '{{ false }}'\n", ids)
			noise := nha042Compile(t, "script.nha029_trace_noise", "sequence:\n- stop: noise_done\n", ids)
			revisionA, _ := storage.NewConfigGenerationRevision(11)
			revisionB, _ := storage.NewConfigGenerationRevision(12)
			stampA, err := NewBoundProgram(revisionA, [32]byte{1})
			if err != nil {
				t.Fatal(err)
			}
			stampB, err := NewBoundProgram(revisionB, [32]byte{2})
			if err != nil {
				t.Fatal(err)
			}
			planA, err := NewPlanWithScripts(nil, []ir.Script{parent, child, noise})
			if err != nil {
				t.Fatal(err)
			}
			planA, err = planA.WithProvenance(stampA)
			if err != nil {
				t.Fatal(err)
			}
			conditions := nha042SharedConditions(t)
			interpreterA, err := NewInterpreter(InterpreterInput{Plan: planA, Conditions: conditions,
				TemplateWaits: conditions, Commands: &nha042Commands{}, Clock: automationtriggers.RealClock{}, Zone: time.UTC})
			if err != nil {
				t.Fatal(err)
			}
			controller, err := NewController(planA, nil, interpreterA, interpreterA)
			if err != nil {
				t.Fatal(err)
			}
			epoch, err := observability.NewProcessObservationEpoch()
			if err != nil {
				t.Fatal(err)
			}
			cells, err := observability.NewCells(epoch)
			if err != nil {
				t.Fatal(err)
			}
			if err := controller.AttachObservation(cells); err != nil {
				t.Fatal(err)
			}
			if err := controller.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { nha188Close(t, controller) })
			target, err := controller.InvokeScript(context.Background(), parentID, nil, Run{}, ir.ScriptCallDirect)
			if err != nil {
				t.Fatal(err)
			}
			waiting := target
			if terminalParent {
				if err := nha042Joined(t, target); err != nil {
					t.Fatal(err)
				}
				controller.mu.Lock()
				waiting = nil
				for _, lane := range controller.lanes {
					for _, entry := range lane.active {
						if entry.run.parent == target.ID() {
							waiting = &ScriptHandle{controller, entry}
						}
					}
				}
				controller.mu.Unlock()
				if waiting == nil || waiting.entry.run.trigger.script != childID {
					t.Fatal("terminal A parent has no real live causal child")
				}
			}
			nha042Await(t, func() bool {
				nodes, _ := interpreterA.NodeTraces()
				for _, trace := range nodes {
					if trace.RunID() == waiting.ID() && trace.Stage() == NodeWaiting {
						return true
					}
				}
				return false
			})
			imageA := target.entry.image
			if waiting.entry.image != imageA || target.entry.run.Provenance() != stampA || waiting.entry.run.Provenance() != stampA {
				t.Fatal("wait/descendant did not retain the actual A image and provenance")
			}
			planB, err := NewPlanWithScripts(nil, []ir.Script{noise})
			if err != nil {
				t.Fatal(err)
			}
			planB, err = planB.WithProvenance(stampB)
			if err != nil {
				t.Fatal(err)
			}
			inputB := interpreterA.input
			inputB.Plan = planB
			interpreterB, err := NewInterpreter(inputB)
			if err != nil {
				t.Fatal(err)
			}
			reservation, err := controller.ReserveImage(planB, interpreterB, interpreterB)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reservation.Release() })
			if err := reservation.Publish(); err != nil {
				t.Fatal(err)
			}
			controller.mu.Lock()
			imageB := controller.active
			publishedB := imageB != imageA && imageB.interpreter == interpreterB && imageB.generation != imageA.generation
			controller.mu.Unlock()
			if !publishedB {
				t.Fatal("B did not become the distinct active image")
			}
			// Every pressure row comes from an admitted and joined distinct B
			// run. B also evicts native nodes, so its loss must not leak into A's cut.
			previousID := waiting.ID()
			pressureRuns := MaxTraces + nodeTraceCapacity + 1
			for range pressureRuns {
				other, err := controller.InvokeScript(context.Background(), noiseID, nil, Run{}, ir.ScriptCallDirect)
				if err != nil {
					t.Fatal(err)
				}
				if other.ID() <= previousID || other.entry.run.Generation() != imageB.generation || other.entry.run.Provenance() != stampB {
					t.Fatal("pressure did not use distinct real B runs and provenance")
				}
				previousID = other.ID()
				if err := nha042Joined(t, other); err != nil {
					t.Fatal(err)
				}
			}
			// Script Done closes before its image reference is returned. Observe
			// that real return before checking temporary reader ownership below.
			nha042Await(t, func() bool {
				controller.mu.Lock()
				defer controller.mu.Unlock()
				return controller.live == 1 && imageA.refs == 1 && imageB.refs == 1
			})
			decisions, decisionLoss := controller.Traces()
			if decisionLoss == 0 || len(decisions) != MaxTraces {
				t.Fatal("real controller pressure did not evict the bounded ring")
			}
			for _, trace := range decisions {
				if trace.RunID == target.ID() || trace.Generation != imageB.generation || trace.Provenance != stampB {
					t.Fatal("A controller rows survived, or pressure forged a causal identity")
				}
			}
			nodesA, lossA := interpreterA.NodeTraces()
			_, lossB := interpreterB.NodeTraces()
			var targetNodes []NodeTrace
			for _, trace := range nodesA {
				if trace.RunID() == target.ID() {
					if trace.Generation() != imageA.generation || trace.Provenance() != stampA || trace.ParentID() != 0 {
						t.Fatal("retained target nodes lost their real A provenance")
					}
					targetNodes = append(targetNodes, trace)
				}
			}
			if len(targetNodes) == 0 || lossA != 0 || lossB == 0 {
				t.Fatal("fixture did not isolate retained A nodes from an evicted B ring")
			}
			t.Logf("distinct real B runs=%d; controller evicted=%d; retained A nodes=%d/loss=%d; unrelated B node loss=%d", pressureRuns, decisionLoss, len(targetNodes), lossA, lossB)
			select {
			case <-waiting.Done():
				t.Fatal("the only live owner of A terminated before observation")
			default:
			}
			used := nha029ObservationUsed(controller)
			controller.mu.Lock()
			refsA, refsB := imageA.refs, imageB.refs
			controller.mu.Unlock()
			rows, cuts, truncated, err := controller.ObservationTrace(target.ID())
			if err != nil || truncated || len(rows) != len(targetNodes) {
				t.Fatalf("A nodes disappeared with controller rows: rows=%d want=%d truncated=%v err=%v", len(rows), len(targetNodes), truncated, err)
			}
			for index, row := range rows {
				wire, err := row.Wire()
				ordinal, valid := targetNodes[index].Ordinal().Value()
				if err != nil || !valid || wire.RunId != uint64(target.ID()) || wire.GetNode() == nil || wire.Ordinal.GetNative() != ordinal {
					t.Fatal("projection substituted B's ring or mixed a different run", err)
				}
			}
			codec, err := observability.NewCursorCodec(epoch)
			if err != nil {
				t.Fatal(err)
			}
			captures, err := observability.NewTraceCaptures(epoch, codec)
			if err != nil {
				t.Fatal(err)
			}
			binding, err := observability.NewReadBinding(1, [32]byte{1})
			if err != nil {
				t.Fatal(err)
			}
			handle, err := controller.ObservationHandle()
			if err != nil {
				t.Fatal(err)
			}
			capture, err := captures.Reserve(binding, handle, uint64(target.ID()))
			if err != nil {
				t.Fatal(err)
			}
			defer capture.Release()
			captured, err := capture.Commit(rows, cuts)
			if err != nil {
				t.Fatal(err)
			}
			size, _ := observability.NewPageSize(100)
			request, err := observability.NewTracePageRequest(size, captured)
			if err != nil {
				t.Fatal(err)
			}
			page, err := captures.Read(request, binding)
			if err != nil {
				t.Fatal(err)
			}
			wire, err := page.Wire("")
			if err != nil || wire.GetPage() == nil || wire.GetPage().Cuts.NodeEvicted != lossA || wire.GetPage().Cuts.ControllerEvicted != decisionLoss || wire.GetPage().Cuts.Coherence != obs.TraceCutCoherence_TRACE_CUT_COHERENCE_SEPARATE_OWNER_CUTS {
				t.Fatal("node cut included losses from the unrelated B image", err)
			}
			controller.mu.Lock()
			unchangedRefs := imageA.refs == refsA && imageB.refs == refsB && controller.active == imageB
			controller.mu.Unlock()
			if !unchangedRefs || nha029ObservationUsed(controller) != used {
				t.Fatal("reading causal rings retained an extra image or pool charge")
			}
			nha188Close(t, controller)
			remaining, _ := interpreterA.NodeTraces()
			if len(remaining) != 0 || imageA.refs != 0 || nha029ObservationUsed(controller) != controllerCharge {
				t.Fatal("finished read extended terminal A retention")
			}
		})
	}
}
