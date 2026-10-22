package automationruns

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/template"
)

type nha054ClockSink struct {
	mu        sync.Mutex
	decisions []DecisionObservation
}

func (s *nha054ClockSink) ObserveDecision(value DecisionObservation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decisions = append(s.decisions, value)
}
func (*nha054ClockSink) ObserveNode(NodeObservation)             {}
func (*nha054ClockSink) ObserveScriptLink(ScriptLinkObservation) {}

type nha054ClockConditions struct{ clock *nha042ManualClock }

func (s nha054ClockConditions) EvaluateCondition(ctx context.Context, program template.Program, bindings []template.Binding) (template.TrackedResult, error) {
	input, err := template.NewInputs(nil, bindings, s.clock.Now(), time.UTC)
	if err != nil {
		return template.TrackedResult{}, err
	}
	return template.EvaluateTracked(ctx, program, input, template.DefaultLimits())
}

func TestNHA054ControllerRetirementKeepsInterpreterFixtureClock(t *testing.T) {
	clock := &nha042ManualClock{wall: time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)}
	id, err := ir.ParseScriptID("script.nha054_retire")
	if err != nil {
		t.Fatal(err)
	}
	script := nha042Compile(t, "script.nha054_retire", "sequence:\n- delay: {seconds: 60}\n", []ir.ScriptID{id})
	plan, err := NewPlanWithScripts(nil, []ir.Script{script})
	if err != nil {
		t.Fatal(err)
	}
	interpreter, err := NewInterpreter(InterpreterInput{Plan: plan, Conditions: nha054ClockConditions{clock}, Commands: &nha042Commands{}, Clock: clock, Zone: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewController(plan, nil, interpreter, interpreter)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close(context.Background()) })
	sink := &nha054ClockSink{}
	if err := controller.AttachHistoryObservationSink(sink); err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	handle, err := controller.InvokeScript(ctx, id, nil, Run{}, ir.ScriptCallDirect)
	if err != nil {
		t.Fatal(err)
	}
	if startup, err := handle.Startup(ctx); err != nil || startup.Kind() != StartupSuspended {
		t.Fatal("interpreter did not reach fixture timer", err, startup.Kind())
	}
	select {
	case <-handle.Done():
		t.Fatal("run completed before its unadvanced fixture timer was retired")
	default:
	}
	if clock.MonoNow() != 0 {
		t.Fatal("blocked timer advanced fixture time")
	}
	if err := controller.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-controller.Done():
	default:
		t.Fatal("Close returned before controller joined")
	}
	select {
	case <-handle.Done():
	default:
		t.Fatal("Close returned before suspended script joined")
	}
	// A cancelled delay terminates by flowStop, so the interpreter may return
	// nil. The controller's terminal DecisionCancelled is authoritative.
	if err := handle.Result(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal("unexpected retired run result", err)
	}
	clock.mu.Lock()
	timersStopped := len(clock.timers) == 1 && clock.timers[0].stopped.Load()
	clock.mu.Unlock()
	if !timersStopped {
		t.Fatal("retirement did not stop the fixture timer")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	cancelled := false
	for _, decision := range sink.decisions {
		if !decision.At.Equal(clock.Now()) {
			t.Fatalf("controller observed wall time through retirement: %v want %v", decision.At, clock.Now())
		}
		cancelled = cancelled || decision.Decision == DecisionCancelled
	}
	if !cancelled {
		t.Fatal("terminal cancellation observation missing")
	}
}
