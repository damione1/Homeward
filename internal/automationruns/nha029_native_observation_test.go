package automationruns

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/damione1/Homeward/compat/ha/ir"
)

func nha029ObservationUsed(c *Controller) int {
	c.pool.mu.Lock()
	defer c.pool.mu.Unlock()
	return c.pool.used
}

func nha029ObservationRuntime(t *testing.T) (*Controller, *Interpreter, ir.ScriptID, ir.Script) {
	t.Helper()
	id, err := ir.ParseScriptID("script.nha029_native_observation")
	if err != nil {
		t.Fatal(err)
	}
	script := nha042Compile(t, "script.nha029_native_observation", "sequence:\n- stop: done\n", []ir.ScriptID{id})
	c, i := nha042Runtime(t, []ir.Script{script}, &nha042Commands{})
	return c, i, id, script
}

func TestNHA029NativeObservationLifetimeAndAliasedRelease(t *testing.T) {
	c, i, id, _ := nha029ObservationRuntime(t)
	before := nha029ObservationUsed(c)
	hold, err := c.RetainNativeObservation(i)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hold.Release() })
	alias := hold
	if alias.state != hold.state || nativeObservationCharge != int(unsafe.Sizeof(nativeObservationState{})) ||
		nha029ObservationUsed(c) != before+nativeObservationCharge {
		t.Fatal("observation did not admit exactly one shared physical header")
	}
	handle, err := c.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDirect)
	if err != nil {
		t.Fatal(err)
	}
	if err := nha042Joined(t, handle); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	image := c.active
	imageCharge := image.charge
	c.mu.Unlock()
	nha188Close(t, c)
	c.mu.Lock()
	last := c.active == nil && c.pending == nil && c.live == 0 && image.refs == 1
	c.mu.Unlock()
	if !last || nha029ObservationUsed(c) != controllerCharge+imageCharge+nativeObservationCharge {
		t.Fatal("Done did not join execution while the real observation image remained charged")
	}
	decisions, decisionLoss := c.Traces()
	nodes, nodeLoss := i.NodeTraces()
	if len(nodes) == 0 || !reflect.DeepEqual(nodes, func() []NodeTrace { v, _ := i.NodeTraces(); return v }()) {
		t.Fatal("postjoin native ring was absent or unstable")
	}
	previous, order := c.traceOrdinal.Load(), c.TraceOrderState()
	results := make(chan error, 2)
	var released sync.WaitGroup
	for _, copied := range []NativeObservation{hold, alias} {
		released.Add(1)
		go func(h NativeObservation) { defer released.Done(); results <- h.Release() }(copied)
	}
	released.Wait()
	close(results)
	successes, refusals := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrInvalidPlan) {
			refusals++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || refusals != 1 || !errors.Is(alias.Release(), ErrInvalidPlan) {
		t.Fatal("copied/concurrent handles released the same ownership more than once")
	}
	hold.state.mu.Lock()
	consumed := hold.state.phase == nativeObservationReleased && hold.state.controller == nil && hold.state.image == nil
	hold.state.mu.Unlock()
	afterNodes, afterNodeLoss := i.NodeTraces()
	afterDecisions, afterDecisionLoss := c.Traces()
	if !consumed || nha029ObservationUsed(c) != controllerCharge || image.refs != 0 ||
		i.image != nil || i.waitPlan != nil || i.charge != 0 || cap(i.traces) != 0 || i.traceNext != 0 ||
		len(afterNodes) != 0 || afterNodeLoss != nodeLoss || afterDecisionLoss != decisionLoss ||
		!reflect.DeepEqual(decisions, afterDecisions) || c.traceOrdinal.Load() != previous || c.TraceOrderState() != order {
		t.Fatal("final observation release retained native storage or changed observations/ordinal")
	}
}

func TestNHA029NativeObservationAdmissionRefusals(t *testing.T) {
	c, _ := nha029ImageFixture(t)
	foreign, _ := nha029ImageFixture(t)
	i := c.active.interpreter
	before, refs := nha029ObservationUsed(c), c.active.refs
	var missing *Controller
	cases := []struct {
		name    string
		acquire func() (NativeObservation, error)
	}{
		{"nil_controller", func() (NativeObservation, error) { return missing.RetainNativeObservation(i) }},
		{"nil_interpreter", func() (NativeObservation, error) { return c.RetainNativeObservation(nil) }},
		{"unbound_interpreter", func() (NativeObservation, error) { return c.RetainNativeObservation(&Interpreter{}) }},
		{"foreign_interpreter", func() (NativeObservation, error) { return c.RetainNativeObservation(foreign.active.interpreter) }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			hold, err := test.acquire()
			if hold.state != nil || !errors.Is(err, ErrInvalidPlan) || nha029ObservationUsed(c) != before || c.active.refs != refs {
				t.Fatalf("invalid observation admitted a ref/header: %v", err)
			}
		})
	}
	if !errors.Is((NativeObservation{}).Release(), ErrInvalidPlan) {
		t.Fatal("zero observation released ownership")
	}
	// A real native image may be held before Controller.Start.
	hold, err := c.RetainNativeObservation(i)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hold.Release() })
	nha188Close(t, c)
	retained := nha029ObservationUsed(c)
	refused, err := c.RetainNativeObservation(i)
	if refused.state != nil || !errors.Is(err, ErrClosed) || nha029ObservationUsed(c) != retained {
		t.Fatalf("terminal controller admitted a new observation: %v", err)
	}
	if err := hold.Release(); err != nil {
		t.Fatal(err)
	}
	if nha029ObservationUsed(c) != controllerCharge {
		t.Fatal("closed observation did not return its exact charge")
	}
}

func TestNHA029NativeObservationCapacityUsesRealHeaders(t *testing.T) {
	c, _ := nha029ImageFixture(t)
	i := c.active.interpreter
	before := nha029ObservationUsed(c)
	image, refs := c.active, c.active.refs
	limit := (MaxMemoryBytes - before) / nativeObservationCharge
	if limit < 1 {
		t.Fatal("fixture has no legitimate header capacity")
	}
	holds := make([]NativeObservation, 0, limit)
	t.Cleanup(func() {
		for _, hold := range holds {
			_ = hold.Release()
		}
	})
	for count := 0; count < limit; count++ {
		hold, err := c.RetainNativeObservation(i)
		if err != nil {
			t.Fatalf("real header %d/%d: %v", count, limit, err)
		}
		holds = append(holds, hold)
	}
	used := nha029ObservationUsed(c)
	if used != before+len(holds)*nativeObservationCharge || used > MaxMemoryBytes || image.refs != refs+len(holds) {
		t.Fatal("physical headers/image references were not charged exactly")
	}
	previous := c.traceOrdinal.Load()
	refused, err := c.RetainNativeObservation(i)
	if refused.state != nil || !errors.Is(err, ErrCapacity) || nha029ObservationUsed(c) != used || image.refs != refs+len(holds) {
		t.Fatalf("full pool admitted an unreserved header/ref: %v", err)
	}
	if allocations := testing.AllocsPerRun(10, func() {
		rejected, err := c.RetainNativeObservation(i)
		if rejected.state != nil || err != ErrCapacity {
			t.Error("capacity refusal changed")
		}
	}); allocations != 0 {
		t.Fatalf("capacity refusal allocated before admission: %f", allocations)
	}
	nha188Close(t, c)
	for index := len(holds) - 1; index >= 0; index-- {
		if err := holds[index].Release(); err != nil {
			t.Fatal(err)
		}
	}
	holds = nil
	if image.refs != 0 || nha029ObservationUsed(c) != controllerCharge || c.traceOrdinal.Load() != previous {
		t.Fatal("real saturated header owners did not release their final image exactly")
	}
}

func TestNHA029NativeObservationKeepsAWhenBPublishes(t *testing.T) {
	c, i, id, script := nha029ObservationRuntime(t)
	handle, err := c.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDirect)
	if err != nil {
		t.Fatal(err)
	}
	if err := nha042Joined(t, handle); err != nil {
		t.Fatal(err)
	}
	hold, err := c.RetainNativeObservation(i)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hold.Release() })
	a, chargeA := c.active, c.active.charge
	plan, err := NewPlanWithScripts(nil, []ir.Script{script})
	if err != nil {
		t.Fatal(err)
	}
	input := i.input
	input.Plan = plan
	next, err := NewInterpreter(input)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := c.ReserveImage(plan, next, next)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reservation.Release() })
	if err := reservation.Publish(); err != nil {
		t.Fatal(err)
	}
	b := c.active
	if hold.state.image != a || a.refs != 1 || b == a ||
		nha029ObservationUsed(c) != controllerCharge+chargeA+b.charge+nativeObservationCharge {
		t.Fatal("publication retargeted or stopped charging the old observed image")
	}
	nodes, loss := i.NodeTraces()
	if len(nodes) == 0 {
		t.Fatal("observed A ring vanished when B became active")
	}
	if refused, err := c.RetainNativeObservation(i); refused.state != nil || !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("a new observer selected an inactive interpreter A: %v", err)
	}
	previous := c.traceOrdinal.Load()
	if err := hold.Release(); err != nil {
		t.Fatal(err)
	}
	after, afterLoss := i.NodeTraces()
	if len(after) != 0 || afterLoss != loss || a.refs != 0 || c.active != b || next.image != b ||
		nha029ObservationUsed(c) != controllerCharge+b.charge || c.traceOrdinal.Load() != previous {
		t.Fatal("release of observed A damaged B or kept A storage")
	}
	fresh, err := c.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDirect)
	if err != nil {
		t.Fatal(err)
	}
	if err := nha042Joined(t, fresh); err != nil {
		t.Fatal(err)
	}
	nha188Close(t, c)
	if nha029ObservationUsed(c) != controllerCharge {
		t.Fatal("B did not release normally without an observation")
	}
}

func TestNHA029NativeObservationEarlyAndConcurrentCloseRelease(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		name := "early_release"
		if concurrent {
			name = "close_race"
		}
		t.Run(name, func(t *testing.T) {
			c, _ := nha029ImageFixture(t)
			hold, err := c.RetainNativeObservation(c.active.interpreter)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = hold.Release() })
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if concurrent {
				start, results := make(chan struct{}), make(chan error, 2)
				go func() { <-start; results <- c.Close(ctx) }()
				go func() { <-start; results <- hold.Release() }()
				close(start)
				for count := 0; count < 2; count++ {
					if err := <-results; err != nil {
						t.Fatal(err)
					}
				}
			} else {
				if err := hold.Release(); err != nil {
					t.Fatal(err)
				}
				if err := c.Close(ctx); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-c.Done():
			default:
				t.Fatal("observation delayed the real Controller join")
			}
			if nha029ObservationUsed(c) != controllerCharge {
				t.Fatal("close/release left a charged image/header")
			}
		})
	}
}
