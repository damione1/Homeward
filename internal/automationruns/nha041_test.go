package automationruns

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/automationtriggers"
)

type nha041Reader struct{ done chan struct{} }

func (r *nha041Reader) NextResult(ctx context.Context) (automationtriggers.Result, error) {
	<-ctx.Done()
	return automationtriggers.Result{}, ctx.Err()
}
func (r *nha041Reader) Done() <-chan struct{} { return r.done }
func (r *nha041Reader) Error() error          { return nil }

type nha041FailReader struct {
	gate       chan struct{}
	done       chan struct{}
	controller *Controller
}

func (r *nha041FailReader) NextResult(ctx context.Context) (automationtriggers.Result, error) {
	select {
	case <-r.gate:
		return automationtriggers.Result{}, errors.New("synthetic reader failure")
	case <-ctx.Done():
		return automationtriggers.Result{}, ctx.Err()
	}
}
func (r *nha041FailReader) Done() <-chan struct{} { return r.done }
func (r *nha041FailReader) Error() error {
	r.controller.Traces() // The controller mutex must not be held here.
	return errors.New("synthetic reader detail")
}

type nha041ReentrantError struct{ controller *Controller }

func (e nha041ReentrantError) Error() string {
	e.controller.Traces() // The controller mutex must not be held here.
	return "synthetic execution failure"
}

type nha041PrepareFunc func(context.Context, ir.Automation, Trigger) (Preparation, error)

func (f nha041PrepareFunc) Prepare(ctx context.Context, a ir.Automation, t Trigger) (Preparation, error) {
	return f(ctx, a, t)
}

type nha041ExecuteFunc func(context.Context, ir.Automation, Run) error

func (f nha041ExecuteFunc) Execute(ctx context.Context, a ir.Automation, r Run) error {
	return f(ctx, a, r)
}

func nha041Automation(t *testing.T, mode string, max int, maxExceeded ...string) ir.Automation {
	t.Helper()
	entity, err := ir.NewEntity("switch.fixture", ir.ReferencePresent, ir.SupportObserveState, ir.SupportSwitchTurnOn)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue([]ir.Entity{entity}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	modeLine, maxLine := "", ""
	if mode != "" {
		modeLine = "mode: " + mode + "\n"
	}
	if max != 0 {
		maxLine = fmt.Sprintf("max: %d\n", max)
	}
	if len(maxExceeded) != 0 {
		maxLine += "max_exceeded: " + maxExceeded[0] + "\n"
	}
	file, err := parser.NewSourceFile("nha041.yaml", []byte("id: nha041_fixture\n"+modeLine+maxLine+
		"triggers:\n- trigger: time_pattern\n  seconds: '*'\nactions:\n- action: switch.turn_on\n  target: {entity_id: switch.fixture}\n  data: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := ir.CompileSources("automation.nha041_fixture", "nha041.yaml", []parser.SourceFile{file}, catalogue)
	if err != nil || len(compiled.Accepted()) != 1 {
		t.Fatalf("compile mode=%q max=%d: %v rejected=%+v", mode, max, err, compiled.Rejected())
	}
	return compiled.Accepted()[0]
}

func nha041Controller(t *testing.T, mode string, max int, execute nha041ExecuteFunc, maxExceeded ...string) (*Controller, automationSpec) {
	t.Helper()
	automation := nha041Automation(t, mode, max, maxExceeded...)
	plan, err := NewPlan([]ir.Automation{automation})
	if err != nil {
		t.Fatal(err)
	}
	prepare := nha041PrepareFunc(func(context.Context, ir.Automation, Trigger) (Preparation, error) {
		return Preparation{Outcome: PreparationReady}, nil
	})
	c, err := NewController(plan, &nha041Reader{done: make(chan struct{})}, prepare, execute)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := c.Close(ctx); err != nil {
			t.Errorf("controller close: %v", err)
		}
	})
	return c, plan.items[automation.ID()]
}

// Admit through the same controller boundary after reserving the preparation
// charge. The real trigger-reader path is exercised separately by Run tests.
func nha041Admit(t *testing.T, c *Controller, spec automationSpec, label string) {
	t.Helper()
	if !c.pool.adjust(0, preparationCharge) {
		t.Fatal("test preparation reservation failed")
	}
	value, err := template.String(label)
	if err != nil {
		t.Fatal(err)
	}
	arena, env, err := newRunArena(&c.pool, []template.Binding{{Name: "label", Value: value}}, len(label))
	if err != nil {
		c.pool.adjust(preparationCharge, 0)
		t.Fatal(err)
	}
	c.admit(spec, Trigger{automation: spec.value.ID(), triggerKey: label, payload: label}, arena, env)
}

func nha041Receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for run")
		var zero T
		return zero
	}
}

func nha041Decision(c *Controller, decision Decision) bool {
	traces, _ := c.Traces()
	for _, trace := range traces {
		if trace.Decision == decision {
			return true
		}
	}
	return false
}

func TestNHA041ModeAndLimitAdmission(t *testing.T) {
	for _, test := range []struct {
		mode string
		want ir.AutomationMode
	}{
		{"", ir.AutomationSingle}, {"single", ir.AutomationSingle},
		{"restart", ir.AutomationRestart}, {"queued", ir.AutomationQueued},
		{"parallel", ir.AutomationParallel},
	} {
		automation := nha041Automation(t, test.mode, 0)
		if automation.Mode() != test.want {
			t.Fatalf("%q compiled mode %v", test.mode, automation.Mode())
		}
		plan, err := NewPlan([]ir.Automation{automation})
		if err != nil || plan.items[automation.ID()].max != DefaultMaxRuns {
			t.Fatalf("%q did not retain default max: %v", test.mode, err)
		}
	}
	for _, max := range []int{1, 64, 65, 10000} {
		automation := nha041Automation(t, "parallel", max)
		_, err := NewPlan([]ir.Automation{automation})
		if max <= MaxLiveRuns && err != nil || max > MaxLiveRuns && !errors.Is(err, ErrCapacity) {
			t.Fatalf("IR max=%d runtime decision: %v", max, err)
		}
	}
}

func TestNHA041SingleTraceAndSilent(t *testing.T) {
	started := make(chan Run, 2)
	c, spec := nha041Controller(t, "single", 1, func(ctx context.Context, _ ir.Automation, run Run) error {
		started <- run
		<-ctx.Done()
		return ctx.Err()
	}, "silent")
	if spec.maxExceeded != ir.MaxExceededSilent {
		t.Fatal("silent refusal policy was lost before admission")
	}
	nha041Admit(t, c, spec, "one")
	_ = nha041Receive(t, started)
	nha041Admit(t, c, spec, "two")
	traces, _ := c.Traces()
	if got := traces[len(traces)-1]; got.Decision != DecisionSingleBusy || got.MaxExceeded != ir.MaxExceededSilent {
		t.Fatalf("single refusal lost silent policy: %+v", got)
	}
	select {
	case <-started:
		t.Fatal("single mode started a second run")
	default:
	}
}

func TestNHA041RefusalPolicyIsEffectiveAndTyped(t *testing.T) {
	for _, test := range []struct {
		name, mode, policy string
		decision           Decision
		want               ir.MaxExceededMode
	}{
		{"single_default", "single", "", DecisionSingleBusy, ir.MaxExceededWarning},
		{"single_error", "single", "error", DecisionSingleBusy, ir.MaxExceededError},
		{"parallel_silent", "parallel", "silent", DecisionMaxExceeded, ir.MaxExceededSilent},
		{"parallel_warning", "parallel", "warning", DecisionMaxExceeded, ir.MaxExceededWarning},
		{"parallel_error", "parallel", "error", DecisionMaxExceeded, ir.MaxExceededError},
	} {
		t.Run(test.name, func(t *testing.T) {
			started := make(chan Run, 2)
			var policy []string
			if test.policy != "" {
				policy = []string{test.policy}
			}
			c, spec := nha041Controller(t, test.mode, 1, func(ctx context.Context, _ ir.Automation, run Run) error {
				started <- run
				<-ctx.Done()
				return ctx.Err()
			}, policy...)
			nha041Admit(t, c, spec, "one")
			_ = nha041Receive(t, started)
			nha041Admit(t, c, spec, "two")
			traces, _ := c.Traces()
			got := traces[len(traces)-1]
			if got.Decision != test.decision || got.MaxExceeded != test.want {
				t.Fatalf("refusal policy: got %+v, want decision %v policy %v", got, test.decision, test.want)
			}
			select {
			case <-started:
				t.Fatal("refused trigger started another run")
			default:
			}
		})
	}
}

func TestNHA041QueuedFIFOAndCapturedInputs(t *testing.T) {
	started := make(chan string, 4)
	release := make(chan struct{}, 3)
	c, spec := nha041Controller(t, "queued", 3, func(ctx context.Context, _ ir.Automation, run Run) error {
		value, ok := run.Environment().Get("label")
		if !ok {
			return ErrInvalidPlan
		}
		label, valid := value.StringValue()
		if !valid {
			return ErrInvalidPlan
		}
		started <- run.Trigger().Payload() + ":" + label
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	for _, label := range []string{"one", "two", "three", "four"} {
		nha041Admit(t, c, spec, label)
	}
	for _, want := range []string{"one", "two", "three"} {
		if got := nha041Receive(t, started); got != want+":"+want {
			t.Fatalf("FIFO/captured input: got %q want %q", got, want)
		}
		release <- struct{}{}
	}
	if !nha041Decision(c, DecisionMaxExceeded) {
		t.Fatal("queued max did not count active plus waiters")
	}
}

func TestNHA041RestartJoinsBeforeNext(t *testing.T) {
	started := make(chan string, 3)
	oldMayFinish := make(chan struct{})
	c, spec := nha041Controller(t, "restart", 0, func(ctx context.Context, _ ir.Automation, run Run) error {
		started <- run.Trigger().Payload()
		if run.Trigger().Payload() == "old" {
			<-ctx.Done()
			<-oldMayFinish
			return ctx.Err()
		}
		<-ctx.Done()
		return ctx.Err()
	})
	nha041Admit(t, c, spec, "old")
	if got := nha041Receive(t, started); got != "old" {
		t.Fatal(got)
	}
	nha041Admit(t, c, spec, "stale")
	nha041Admit(t, c, spec, "latest")
	if !nha041Decision(c, DecisionSuperseded) {
		t.Fatal("restart did not fence intermediate candidate")
	}
	select {
	case got := <-started:
		t.Fatalf("restart overlapped old run: %q", got)
	default:
	}
	close(oldMayFinish)
	if got := nha041Receive(t, started); got != "latest" {
		t.Fatalf("restart started %q, wanted latest", got)
	}
}

func TestNHA041ParallelRunAndBranchEnvironmentIsolation(t *testing.T) {
	started := make(chan Run, 2)
	c, spec := nha041Controller(t, "parallel", 2, func(ctx context.Context, _ ir.Automation, run Run) error {
		started <- run
		<-ctx.Done()
		return ctx.Err()
	})
	nha041Admit(t, c, spec, "first")
	nha041Admit(t, c, spec, "second")
	first, second := nha041Receive(t, started), nha041Receive(t, started)
	if first.ID() == second.ID() || first.Environment() == second.Environment() {
		t.Fatal("parallel runs shared identity or environment")
	}
	nested := func(number int64) template.Value {
		t.Helper()
		item, err := template.Object([]template.Binding{{Name: "state", Value: template.Int(number)}})
		if err != nil {
			t.Fatal(err)
		}
		items, err := template.List([]template.Value{item})
		if err != nil {
			t.Fatal(err)
		}
		object, err := template.Object([]template.Binding{{Name: "items", Value: items}})
		if err != nil {
			t.Fatal(err)
		}
		return object
	}
	if err := first.Environment().Set("nested", nested(1)); err != nil {
		t.Fatal(err)
	}
	left, err := first.Environment().ForkBranch()
	if err != nil {
		t.Fatal(err)
	}
	right, err := first.Environment().ForkBranch()
	if err != nil {
		t.Fatal(err)
	}
	changed, _ := template.String("private")
	if err := left.Set("label", changed); err != nil {
		t.Fatal(err)
	}
	if err := left.Set("nested", nested(2)); err != nil {
		t.Fatal(err)
	}
	for _, env := range []*Environment{first.Environment(), right, second.Environment()} {
		value, ok := env.Get("label")
		label, valid := value.StringValue()
		if !ok || !valid || label == "private" {
			t.Fatal("parallel run or sibling branch observed private mutation")
		}
	}
	for _, env := range []*Environment{first.Environment(), right} {
		object, ok := env.Get("nested")
		if !ok {
			t.Fatal("sibling lost nested value")
		}
		list, ok := object.Field("items")
		if !ok {
			t.Fatal("sibling lost nested list")
		}
		items, ok := list.Items()
		if !ok || len(items) != 1 {
			t.Fatal("sibling nested list changed")
		}
		state, ok := items[0].Field("state")
		if number, valid := state.Int(); !ok || !valid || number != 1 {
			t.Fatal("nested branch mutation contaminated parent or sibling")
		}
		items[0] = template.Int(99) // Public accessors return private copies.
		again, _ := env.Get("nested")
		againList, _ := again.Field("items")
		againItems, _ := againList.Items()
		againState, _ := againItems[0].Field("state")
		if number, valid := againState.Int(); !valid || number != 1 {
			t.Fatal("nested accessor exposed environment-owned storage")
		}
	}
	if _, ok := second.Environment().Get("nested"); ok {
		t.Fatal("nested variable leaked into another parallel run")
	}
	nha041Admit(t, c, spec, "third")
	if !nha041Decision(c, DecisionMaxExceeded) {
		t.Fatal("parallel run cap was not observable")
	}
}

func TestNHA041ForkLimitsAndSetAdmission(t *testing.T) {
	pool := &memoryPool{}
	if !pool.adjust(0, preparationCharge) {
		t.Fatal("preparation reservation failed")
	}
	arena, root, err := newRunArena(pool, []template.Binding{{Name: "safe", Value: template.Int(1)}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer arena.close()
	if err := root.Set("states", template.Int(2)); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("reserved name passed Set: %v", err)
	}
	branch := root
	for i := 0; i < MaxBranchDepth; i++ {
		branch, err = branch.ForkBranch()
		if err != nil {
			t.Fatalf("fork depth %d: %v", i, err)
		}
	}
	if _, err := branch.ForkBranch(); !errors.Is(err, ErrCapacity) {
		t.Fatalf("unbounded branch depth: %v", err)
	}
	for i := MaxBranchDepth; i < MaxBranchesPerRun; i++ {
		if _, err := root.ForkBranch(); err != nil {
			t.Fatalf("fork number %d: %v", i, err)
		}
	}
	if _, err := root.ForkBranch(); !errors.Is(err, ErrCapacity) {
		t.Fatalf("unbounded branch count: %v", err)
	}
	if pool.used > MaxMemoryBytes {
		t.Fatal("forks bypassed global memory ledger")
	}
}

func TestNHA041CloseJoinsAllAndFencesAdmissions(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	c, spec := nha041Controller(t, "single", 0, func(ctx context.Context, _ ir.Automation, _ Run) error {
		started <- struct{}{}
		<-ctx.Done()
		<-release
		return ctx.Err()
	})
	nha041Admit(t, c, spec, "one")
	_ = nha041Receive(t, started)
	done := c.BeginClose()
	select {
	case <-done:
		t.Fatal("Done closed before executor joined")
	default:
	}
	nha041Admit(t, c, spec, "late")
	if !nha041Decision(c, DecisionClosed) {
		t.Fatal("closed controller admitted a new run")
	}
	close(release)
	_ = nha041Receive(t, done)
}

func TestNHA041ExecutionErrorReleasesSlot(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	done := make(chan struct{}, 2)
	c, spec := nha041Controller(t, "single", 0, func(context.Context, ir.Automation, Run) error {
		mu.Lock()
		calls++
		mu.Unlock()
		done <- struct{}{}
		return errors.New("synthetic failure")
	})
	nha041Admit(t, c, spec, "first")
	_ = nha041Receive(t, done)
	deadline := time.Now().Add(time.Second)
	for !nha041Decision(c, DecisionExecutionError) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	nha041Admit(t, c, spec, "second")
	_ = nha041Receive(t, done)
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("execution failure retained slot: %d calls", calls)
	}
}

func TestNHA041PortErrorCallbacksAreOutsideMutex(t *testing.T) {
	var executionController *Controller
	finished := make(chan struct{}, 1)
	executionController, spec := nha041Controller(t, "single", 0, func(context.Context, ir.Automation, Run) error {
		finished <- struct{}{}
		return nha041ReentrantError{controller: executionController}
	})
	nha041Admit(t, executionController, spec, "reentrant")
	_ = nha041Receive(t, finished)
	deadline := time.Now().Add(2 * time.Second)
	for !nha041Decision(executionController, DecisionExecutionError) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !nha041Decision(executionController, DecisionExecutionError) {
		t.Fatal("execution error callback deadlocked controller trace lock")
	}

	plan, err := NewPlan([]ir.Automation{nha041Automation(t, "single", 0)})
	if err != nil {
		t.Fatal(err)
	}
	reader := &nha041FailReader{gate: make(chan struct{}), done: make(chan struct{})}
	controller, err := NewController(plan, reader,
		nha041PrepareFunc(func(context.Context, ir.Automation, Trigger) (Preparation, error) {
			return Preparation{Outcome: PreparationReady}, nil
		}),
		nha041ExecuteFunc(func(context.Context, ir.Automation, Run) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	reader.controller = controller
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(reader.gate)
	_ = nha041Receive(t, controller.Done())
	if controller.Error() == nil {
		t.Fatal("reader error was not retained after reentrant callback")
	}
}

func TestNHA041TraceOwnsBoundedErrorBacking(t *testing.T) {
	large := strings.Repeat("x", 1<<20)
	controller := &Controller{traces: make([]Trace, 0, MaxTraces)}
	controller.recordLocked(Trace{Decision: DecisionPreparationError, Error: large[:512]})
	retained := controller.traces[0].Error
	if len(retained) != 256 || unsafe.StringData(retained) == unsafe.StringData(large) {
		t.Fatal("trace retained an unbounded error backing allocation")
	}
}

func TestNHA041CloseBeforeStartAndStartAfterClose(t *testing.T) {
	plan, err := NewPlan([]ir.Automation{nha041Automation(t, "single", 0)})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	controller, err := NewController(plan, &nha041Reader{done: make(chan struct{})},
		nha041PrepareFunc(func(context.Context, ir.Automation, Trigger) (Preparation, error) {
			called = true
			return Preparation{Outcome: PreparationReady}, nil
		}),
		nha041ExecuteFunc(func(context.Context, ir.Automation, Run) error {
			called = true
			return nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := controller.Close(ctx)
		cancel()
		if err != nil {
			t.Fatalf("pre-start close %d: %v", i, err)
		}
	}
	select {
	case <-controller.Done():
	default:
		t.Fatal("pre-start Done was left open")
	}
	if err := controller.Start(context.Background()); !errors.Is(err, ErrClosed) || called {
		t.Fatalf("closed controller restarted or invoked a port: err=%v called=%v", err, called)
	}
}
