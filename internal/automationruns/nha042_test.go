package automationruns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/mqtt"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
	"github.com/damione1/Homeward/internal/reactivetemplate"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	mochi "github.com/mochi-mqtt/server/v2"
	mochipackets "github.com/mochi-mqtt/server/v2/packets"
)

type nha042Authority struct{ actor domain.ActorID }

func (a nha042Authority) Context(ctx context.Context) (context.Context, domain.ActorID, error) {
	return ctx, a.actor, nil
}

type nha042Commands struct {
	mu      sync.Mutex
	aliases []string
	causes  []string
	fail    string
	failure error
}

func (c *nha042Commands) ExecuteCall(_ context.Context, call ir.Call, execution CommandExecution) error {
	cause, err := execution.CausalKey()
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, target := range call.Targets() {
		name, _ := target.Alias().Value()
		c.aliases = append(c.aliases, name)
		c.causes = append(c.causes, cause)
		if name == c.fail {
			return c.failure
		}
	}
	return nil
}
func nha042Compile(t *testing.T, name, body string, ids []ir.ScriptID) ir.Script {
	t.Helper()
	fixture, _ := ir.NewEntity("switch.nha042_fixture", ir.ReferencePresent, ir.SupportSwitchTurnOn)
	wrong, _ := ir.NewEntity("switch.nha042_wrong", ir.ReferencePresent, ir.SupportSwitchTurnOn)
	entities := []ir.Entity{fixture, wrong}
	for _, name := range []string{"switch.fixture", "switch.nha042_child_enter", "switch.nha042_child_exit", "switch.nha042_parent_after_call", "switch.nha042_parent_exit", "switch.nha042_after_wait", "switch.nha042_after_race", "switch.nha042_fail", "switch.nha042_other_branch", "switch.nha042_after_parallel"} {
		entity, err := ir.NewEntity(name, ir.ReferencePresent, ir.SupportObserveState, ir.SupportSwitchTurnOn)
		if err != nil {
			t.Fatal(err)
		}
		entities = append(entities, entity)
	}
	event, err := ir.NewEvent("nha042_release", ir.ReferencePresent)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue(entities, nil, []ir.Event{event})
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err = catalogue.WithScripts(ids)
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.NewSourceFile(name+".yaml", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	result, err := ir.CompileScriptSources(name, name+".yaml", []parser.SourceFile{file}, catalogue)
	if err != nil || len(result.Accepted()) != 1 {
		t.Fatalf("compile %s: %v %+v", name, err, result.Rejected())
	}
	return result.Accepted()[0]
}
func nha042SharedConditions(t *testing.T) *reactivetemplate.Service {
	t.Helper()
	_, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.PublishRegistry(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	plan, err := reactivetemplate.NewPlan(nil, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = plan.WithScriptWaits(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	service, err := reactivetemplate.Start(context.Background(), fixture.Sequencer(), plan, reactivetemplate.RealClock{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return service
}
func nha042Runtime(t *testing.T, scripts []ir.Script, commands *nha042Commands) (*Controller, *Interpreter) {
	t.Helper()
	plan, err := NewPlanWithScripts(nil, scripts)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := domain.ParseActorID("nha042-explicit-test-caller")
	if err != nil {
		t.Fatal(err)
	}
	interpreter, err := NewInterpreter(InterpreterInput{Plan: plan, Conditions: nha042SharedConditions(t),
		Authority: nha042Authority{actor}, Commands: commands, Clock: automationtriggers.RealClock{}, Zone: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewController(plan, nil, interpreter, interpreter)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := controller.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return controller, interpreter
}

func TestNHA042RealIRInterpreterControlsScopesLoopsChooseParallelAndStop(t *testing.T) {
	id, _ := ir.ParseScriptID("script.nha042_language")
	program := nha042Compile(t, "script.nha042_language", `variables: {enabled: true}
sequence:
- sequence:
  - variables: {enabled: false}
  - if:
    - condition: template
      value_template: '{{ enabled }}'
    then:
    - action: switch.turn_on
      target: {entity_id: switch.nha042_wrong}
      data: {}
    else:
    - action: switch.turn_on
      target: {entity_id: switch.nha042_fixture}
      data: {}
- if:
  - condition: template
    value_template: '{{ enabled }}'
  then:
  - repeat:
      count: 3
      sequence:
      - choose:
        - conditions:
          - condition: template
            value_template: '{{ false }}'
          sequence:
          - action: switch.turn_on
            target: {entity_id: switch.nha042_wrong}
            data: {}
        - conditions:
          - condition: template
            value_template: '{{ repeat.first or repeat.last or true }}'
          sequence:
          - action: switch.turn_on
            target: {entity_id: switch.nha042_fixture}
            data: {}
- parallel:
  - sequence:
    - variables: {enabled: false}
    - action: switch.turn_on
      target: {entity_id: switch.nha042_fixture}
      data: {}
  - sequence:
    - if:
      - condition: template
        value_template: '{{ enabled }}'
      then:
      - action: switch.turn_on
        target: {entity_id: switch.nha042_fixture}
        data: {}
      else:
      - action: switch.turn_on
        target: {entity_id: switch.nha042_wrong}
        data: {}
- stop: done
- action: switch.turn_on
  target: {entity_id: switch.nha042_wrong}
  data: {}
`, []ir.ScriptID{id})
	commands := &nha042Commands{}
	controller, interpreter := nha042Runtime(t, []ir.Script{program}, commands)
	controller.pool.mu.Lock()
	baseline := controller.pool.used
	controller.pool.mu.Unlock()
	handle, err := controller.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDirect)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := handle.Result(ctx); err != nil {
		t.Fatal(err)
	}
	commands.mu.Lock()
	aliases, causes := append([]string(nil), commands.aliases...), append([]string(nil), commands.causes...)
	commands.mu.Unlock()
	if len(aliases) != 6 {
		t.Fatalf("branch/loop/stop outcome: %v", aliases)
	}
	for _, name := range aliases {
		if name != "switch.nha042_fixture" {
			t.Fatalf("scope leaked or wrong branch executed: %v", aliases)
		}
	}
	seen := make(map[string]bool)
	for _, cause := range causes {
		if cause == "" || seen[cause] {
			t.Fatalf("repeat reused a physical occurrence: %v", causes)
		}
		seen[cause] = true
	}
	traces, evicted := interpreter.NodeTraces()
	if evicted != 0 || len(traces) < 20 {
		t.Fatalf("node traces missing: %d %d", len(traces), evicted)
	}
	stopped := false
	for _, trace := range traces {
		if trace.Source().Object() != "script.nha042_language" || !strings.Contains(trace.Source().Path(), "sequence") || trace.RunID() != handle.ID() {
			t.Fatalf("trace lost real script/source identity: %+v", trace)
		}
		if trace.Stage() == NodeStopped {
			stopped = true
		}
	}
	if !stopped {
		t.Fatal("stop node was not traced")
	}
	controller.pool.mu.Lock()
	after := controller.pool.used
	controller.pool.mu.Unlock()
	if after != baseline {
		t.Fatalf("completed branches/run leaked pool charge: before %d after %d", baseline, after)
	}
}

func TestNHA042InterpreterCannotBindDifferentPlanOrSecondController(t *testing.T) {
	id, _ := ir.ParseScriptID("script.nha042_identity")
	script := nha042Compile(t, "script.nha042_identity", "sequence:\n- stop: done\n", []ir.ScriptID{id})
	commands := &nha042Commands{}
	first, interpreter := nha042Runtime(t, []ir.Script{script}, commands)
	if _, err := NewController(first.plan, nil, interpreter, interpreter); err == nil {
		t.Fatal("interpreter bound to a second task/pool authority")
	}
	other, err := NewPlanWithScripts(nil, []ir.Script{script})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewController(other, nil, interpreter, interpreter); err == nil {
		t.Fatal("matching names forged an admitted plan identity")
	}
}

type nha042Timer struct {
	channel        chan time.Time
	deadline       time.Duration
	stops          atomic.Int64
	stopped, fired atomic.Bool
}

func (t *nha042Timer) C() <-chan time.Time { return t.channel }
func (t *nha042Timer) Stop() bool          { t.stops.Add(1); return !t.stopped.Swap(true) }

type nha042ManualClock struct {
	mu     sync.Mutex
	wall   time.Time
	mono   time.Duration
	timers []*nha042Timer
}

func (c *nha042ManualClock) Now() time.Time         { c.mu.Lock(); defer c.mu.Unlock(); return c.wall }
func (c *nha042ManualClock) MonoNow() time.Duration { c.mu.Lock(); defer c.mu.Unlock(); return c.mono }
func (c *nha042ManualClock) NewTimer(d time.Duration) automationtriggers.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &nha042Timer{channel: make(chan time.Time, 1), deadline: c.mono + d}
	c.timers = append(c.timers, timer)
	return timer
}
func (c *nha042ManualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall, c.mono = c.wall.Add(d), c.mono+d
	for _, timer := range c.timers {
		if timer.deadline > c.mono || timer.stopped.Load() || timer.fired.Swap(true) {
			continue
		}
		select {
		case timer.channel <- c.wall:
		default:
		}
	}
}

type nha042ReactiveClock struct{ base *nha042ManualClock }

func (c nha042ReactiveClock) Now() time.Time { return c.base.Now() }
func (c nha042ReactiveClock) NewTimer(d time.Duration) reactivetemplate.Timer {
	return c.base.NewTimer(d)
}

type nha042Resolver struct{ entity domain.EntityID }

func (r nha042Resolver) ResolveEntity(alias domain.HAAlias) (domain.EntityID, error) {
	name, err := alias.Value()
	if err != nil || name != "switch.fixture" {
		return domain.EntityID{}, ErrInvalidPlan
	}
	return r.entity, nil
}
func (nha042Resolver) ResolveDevice(ir.HADeviceRef) (domain.DeviceID, error) {
	return domain.DeviceID{}, ErrInvalidPlan
}
func (nha042Resolver) ResolveCalendar(domain.HAAlias) (automationtriggers.CalendarSourceID, error) {
	return automationtriggers.CalendarSourceID{}, ErrInvalidPlan
}

type nha042Rig struct {
	controller  *Controller
	interpreter *Interpreter
	clock       *nha042ManualClock
	commands    *nha042Commands
	session     integrations.SessionHost
	binding     integrations.Binding
}

func nha042FullRig(t *testing.T, programs []ir.Script, commands *nha042Commands) *nha042Rig {
	t.Helper()
	ctx := context.Background()
	clock := &nha042ManualClock{wall: time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)}
	host, fixture, err := fake.NewCommandFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha042-fixture")
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := fake.NewDriver(clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(ctx, session); err != nil {
		t.Fatal(err)
	}
	binding, err := driver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	device, entity, err := binding.IDs()
	if err != nil {
		t.Fatal(err)
	}
	key, _ := integrations.NewSourceKey("fixture-controller", "relay")
	alias, _ := domain.ParseHAAlias("switch.fixture")
	record, err := integrations.NewRegistryRecord(device, entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	triggerPlan, err := automationtriggers.NewPlan(automationtriggers.PlanInput{Scripts: programs, Zone: "UTC", Resolver: nha042Resolver{entity}})
	if err != nil {
		t.Fatal(err)
	}
	triggers, err := automationtriggers.NewService(triggerPlan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := triggers.Start(ctx); err != nil {
		t.Fatal(err)
	}
	reactivePlan, err := reactivetemplate.NewPlan(nil, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	reactivePlan, err = reactivePlan.WithScriptWaits(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	reactive, err := reactivetemplate.Start(ctx, fixture.Sequencer(), reactivePlan, nha042ReactiveClock{clock})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlanWithScripts(nil, programs)
	if err != nil {
		t.Fatal(err)
	}
	actor, _ := domain.ParseActorID("nha042-explicit-test-caller")
	interpreter, err := NewInterpreter(InterpreterInput{Plan: plan, TriggerWaits: triggers, TemplateWaits: reactive,
		Conditions: reactive, Authority: nha042Authority{actor}, Commands: commands, Clock: clock, Zone: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewController(plan, nil, interpreter, interpreter)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := controller.Close(closeCtx); err != nil {
			t.Error(err)
		}
		if err := triggers.Close(); err != nil {
			t.Error(err)
		}
		if err := reactive.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	return &nha042Rig{controller, interpreter, clock, commands, session, binding}
}
func (r *nha042Rig) state(t *testing.T, on bool) {
	t.Helper()
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	at, value := r.clock.Now(), domain.NewSwitchValue(on)
	observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.session.Publish(context.Background(), r.binding, observation); err != nil {
		t.Fatal(err)
	}
	literal, _ := template.Compile("{{ true }}")
	if _, err := r.interpreter.input.Conditions.EvaluateCondition(context.Background(), literal, nil); err != nil {
		t.Fatal(err)
	}
}
func (r *nha042Rig) effects() []string {
	r.commands.mu.Lock()
	defer r.commands.mu.Unlock()
	return append([]string(nil), r.commands.aliases...)
}
func nha042Await(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !predicate() {
		if time.Now().After(deadline) {
			t.Fatal("real phase barrier not reached")
		}
		runtime.Gosched()
	}
}
func nha042Joined(t *testing.T, handle *ScriptHandle) error {
	t.Helper()
	select {
	case <-handle.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("script did not actually join")
	}
	return handle.Result(context.Background())
}
func (r *nha042Rig) running(id ir.ScriptID) bool {
	r.controller.mu.Lock()
	defer r.controller.mu.Unlock()
	name, _ := id.Value()
	return len(r.controller.lanes[laneKey{executionScript, name}].active) != 0
}
func (r *nha042Rig) activeHandle(id ir.ScriptID) *ScriptHandle {
	r.controller.mu.Lock()
	defer r.controller.mu.Unlock()
	name, _ := id.Value()
	for _, entry := range r.controller.lanes[laneKey{executionScript, name}].active {
		return &ScriptHandle{r.controller, entry}
	}
	return nil
}
func (r *nha042Rig) waiting(id RunID) bool {
	traces, _ := r.interpreter.NodeTraces()
	for _, trace := range traces {
		if trace.RunID() == id && trace.Stage() == NodeWaiting {
			return true
		}
	}
	return false
}
func nha042WaitProjection(t *testing.T, value template.Value) map[string]any {
	t.Helper()
	completed, ok := value.Field("completed")
	if !ok {
		t.Fatal("wait.completed absent")
	}
	boolean, ok := completed.Bool()
	if !ok {
		t.Fatal("wait.completed is not boolean")
	}
	remaining, ok := value.Field("remaining")
	if !ok {
		t.Fatal("wait.remaining absent")
	}
	result := map[string]any{"completed": boolean, "remaining": nil}
	if number, present := remaining.Float(); present {
		result["remaining"] = number
	}
	if trigger, present := value.Field("trigger"); present {
		result["trigger"] = nil
		if kind, _ := trigger.Kind(); kind == template.KindObject {
			fields := make(map[string]any)
			for _, name := range []string{"id", "idx", "platform"} {
				field, present := trigger.Field(name)
				text, valid := field.StringValue()
				if !present || !valid {
					t.Fatalf("invalid wait.trigger.%s", name)
				}
				fields[name] = text
			}
			result["trigger"] = fields
		}
	}
	return result
}
func (r *nha042Rig) lastWait(t *testing.T, id RunID) (map[string]any, WaitExit) {
	t.Helper()
	traces, _ := r.interpreter.NodeTraces()
	for index := len(traces) - 1; index >= 0; index-- {
		trace := traces[index]
		if trace.RunID() != id {
			continue
		}
		value, present, err := trace.WaitValue()
		if err != nil {
			t.Fatal(err)
		}
		if present {
			return nha042WaitProjection(t, value), trace.WaitExit()
		}
	}
	t.Fatal("no terminal wait observation")
	return nil, WaitExitInvalid
}

type nha042ObservedCase struct {
	ID           string   `json:"id"`
	Effects      []string `json:"effects"`
	FinalEffects []string `json:"final_effects"`
	Before       struct {
		Effects       []string `json:"effects"`
		ParentRunning bool     `json:"parent_running"`
		ChildRunning  bool     `json:"child_running"`
	} `json:"before"`
	AfterCancel struct {
		ParentRunning bool `json:"parent_running"`
		ChildRunning  bool `json:"child_running"`
	} `json:"after_cancel"`
	ParentResult struct {
		Type      string         `json:"type"`
		Cancelled bool           `json:"task_cancelled"`
		Wait      map[string]any `json:"wait"`
	} `json:"parent_result"`
	Wait map[string]any `json:"wait"`
}

func nha042Observed(t *testing.T, id string) nha042ObservedCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/nha042/p006-observed.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		SHA   string               `json:"raw_observed_sha256"`
		Cases []nha042ObservedCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.SHA != "5451707561a62e1182b4ca6e9450293d2445f7c87102ae8a22e041e32dd6a38d" || len(fixture.Cases) != 13 {
		t.Fatal("P006 provenance/case set changed")
	}
	for _, observation := range fixture.Cases {
		if observation.ID == id {
			return observation
		}
	}
	t.Fatalf("P006 case %q missing", id)
	return nha042ObservedCase{}
}
func nha042Compare(t *testing.T, actual, observed any) {
	t.Helper()
	if !reflect.DeepEqual(actual, observed) {
		t.Fatalf("differential: actual=%#v observed=%#v", actual, observed)
	}
}
func nha042Action(alias string) string {
	return "- action: switch.turn_on\n  target: {entity_id: switch.nha042_" + alias + "}\n  data: {}\n"
}

func nha042CallCase(t *testing.T, detached, cancelParent bool, caseID string) {
	t.Helper()
	parentID, _ := ir.ParseScriptID("script.nha042_parent")
	childID, _ := ir.ParseScriptID("script.nha042_child")
	ids := []ir.ScriptID{parentID, childID}
	child := nha042Compile(t, "script.nha042_child", "mode: parallel\nsequence:\n"+nha042Action("child_enter")+"- wait_template: \"{{ is_state('switch.fixture', 'on') }}\"\n"+nha042Action("child_exit"), ids)
	call := "- action: script.nha042_child\n"
	if detached {
		call = "- action: script.turn_on\n  target: {entity_id: script.nha042_child}\n"
	}
	body := "sequence:\n" + call + nha042Action("parent_after_call")
	if cancelParent {
		body += "- wait_template: '{{ false }}'\n" + nha042Action("parent_exit")
	}
	parent := nha042Compile(t, "script.nha042_parent", body, ids)
	rig := nha042FullRig(t, []ir.Script{parent, child}, &nha042Commands{})
	handle, err := rig.controller.InvokeScript(context.Background(), parentID, nil, Run{}, ir.ScriptCallDirect)
	if err != nil {
		t.Fatal(err)
	}
	nha042Await(t, func() bool {
		traces, _ := rig.interpreter.NodeTraces()
		for _, trace := range traces {
			if trace.Source().Object() == "script.nha042_child" && trace.Stage() == NodeWaiting {
				return true
			}
		}
		return false
	})
	childHandle := rig.activeHandle(childID)
	if childHandle == nil {
		t.Fatal("armed child has no controller-owned handle")
	}
	if detached {
		if cancelParent {
			nha042Await(t, func() bool { return rig.waiting(handle.ID()) })
		} else if err := nha042Joined(t, handle); err != nil {
			t.Fatal(err)
		}
	}
	observed := nha042Observed(t, caseID)
	nha042Compare(t, rig.effects(), observed.Before.Effects)
	nha042Compare(t, rig.running(parentID), observed.Before.ParentRunning)
	nha042Compare(t, rig.running(childID), observed.Before.ChildRunning)
	if cancelParent {
		if err := rig.controller.StopScript(context.Background(), parentID); err != nil {
			t.Fatal(err)
		}
	}
	nha042Compare(t, rig.running(parentID), observed.AfterCancel.ParentRunning)
	nha042Compare(t, rig.running(childID), observed.AfterCancel.ChildRunning)
	rig.state(t, true)
	result := nha042Joined(t, handle)
	nha042Compare(t, errors.Is(result, context.Canceled), observed.ParentResult.Cancelled)
	_ = nha042Joined(t, childHandle)
	nha042Compare(t, rig.effects(), observed.FinalEffects)
	if observed.ParentResult.Wait != nil {
		wait, _ := rig.lastWait(t, handle.ID())
		nha042Compare(t, wait, observed.ParentResult.Wait)
	}
}
func TestNHA042DirectWaitsAndTurnOnReturnsAfterStart(t *testing.T) {
	t.Run("direct_waits", func(t *testing.T) { nha042CallCase(t, false, false, "direct_waits") })
	t.Run("turn_on_returns_after_start", func(t *testing.T) { nha042CallCase(t, true, false, "turn_on_returns_after_start") })
}
func TestNHA042ParentCancellationDirectAndDetached(t *testing.T) {
	t.Run("cancel_direct_parent", func(t *testing.T) { nha042CallCase(t, false, true, "cancel_direct_parent") })
	t.Run("cancel_detached_parent", func(t *testing.T) { nha042CallCase(t, true, true, "cancel_detached_parent") })
}
func TestNHA042ZeroWaitDifferential(t *testing.T) {
	for _, test := range []struct {
		id, kind string
		initial  bool
	}{
		{"zero_trigger_already_matching", "trigger", true}, {"zero_template_already_matching", "template", true}, {"zero_template_false", "template", false},
	} {
		t.Run(test.id, func(t *testing.T) {
			id, _ := ir.ParseScriptID("script.nha042_zero")
			wait := "- wait_template: \"{{ is_state('switch.fixture', 'on') }}\"\n"
			if test.kind == "trigger" {
				wait = "- wait_for_trigger:\n  - trigger: state\n    entity_id: switch.fixture\n    to: 'on'\n"
			}
			program := nha042Compile(t, "script.nha042_zero", "sequence:\n"+wait+"  timeout: {seconds: 0}\n  continue_on_timeout: true\n"+nha042Action("after_wait"), []ir.ScriptID{id})
			rig := nha042FullRig(t, []ir.Script{program}, &nha042Commands{})
			rig.state(t, test.initial)
			handle, err := rig.controller.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDirect)
			if err != nil {
				t.Fatal(err)
			}
			if err := nha042Joined(t, handle); err != nil {
				t.Fatal(err)
			}
			actual, _ := rig.lastWait(t, handle.ID())
			observed := nha042Observed(t, test.id)
			nha042Compare(t, actual, observed.Wait)
			nha042Compare(t, rig.effects(), observed.Effects)
		})
	}
}
func TestNHA042WaitRaceDifferential(t *testing.T) {
	for _, test := range []struct {
		id                          string
		continuation, callbackFirst bool
	}{
		{"trigger_before_timeout_callback", true, false}, {"timeout_callback_before_trigger", true, false},
		{"trigger_before_timeout_abort", false, false}, {"timeout_before_trigger_abort", false, false},
		{"processed_trigger_callback_then_due_timeout", false, true},
	} {
		t.Run(test.id, func(t *testing.T) {
			id, _ := ir.ParseScriptID("script.nha042_race")
			body := fmt.Sprintf("sequence:\n- wait_for_trigger:\n  - trigger: event\n    event_type: nha042_release\n  timeout: {seconds: 60}\n  continue_on_timeout: %t\n", test.continuation) + nha042Action("after_race")
			program := nha042Compile(t, "script.nha042_race", body, []ir.ScriptID{id})
			rig := nha042FullRig(t, []ir.Script{program}, &nha042Commands{})
			if !rig.controller.pool.adjust(0, preparationCharge) {
				t.Fatal("test arena reservation")
			}
			arena, environment, err := newRunArena(&rig.controller.pool, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer arena.close()
			armed, resume := make(chan struct{}), make(chan struct{})
			var release sync.Once
			unblock := func() { release.Do(func() { close(resume) }) }
			defer unblock()
			run := Run{id: 1, generation: rig.controller.generation, trigger: Trigger{script: id}, env: environment,
				suspend: func() { close(armed); <-resume }}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan error, 1)
			joined := make(chan struct{})
			go func() {
				defer close(joined)
				finished <- rig.interpreter.executeProgram(ctx, rig.interpreter.scripts[id], run)
			}()
			defer func() { cancel(); unblock(); <-joined }()
			select {
			case <-armed:
			case <-time.After(3 * time.Second):
				t.Fatal("real wait not armed")
			}
			if test.callbackFirst {
				rig.clock.advance(59 * time.Second)
			} else {
				rig.clock.advance(60 * time.Second)
			}
			event, err := integrations.NewDeviceEvent("nha042_release", rig.clock.Now(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := rig.session.Emit(ctx, rig.binding, event); err != nil {
				t.Fatal(err)
			}
			// The second real observer's registration flushes the preceding
			// event under the existing 040 mutex before it arms. That proves
			// the original callback has completed, without a second reader.
			barrier, err := rig.interpreter.input.TriggerWaits.ObserveWait(ctx, program.Waits()[0].Source())
			if err != nil {
				t.Fatal(err)
			}
			<-barrier.Close()
			if test.callbackFirst {
				rig.clock.advance(time.Second)
			}
			unblock()
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("waiter did not join")
			}
			actual, exit := rig.lastWait(t, run.ID())
			observed := nha042Observed(t, test.id)
			nha042Compare(t, actual, observed.Wait)
			if test.continuation && exit != WaitContinuedAfterTimeout || !test.continuation && exit != WaitStoppedByTimeout {
				t.Fatalf("wrong terminal cause %v", exit)
			}
			effects := rig.effects()
			if len(effects) == 0 {
				effects = []string{}
			}
			nha042Compare(t, effects, observed.Effects)
		})
	}
}
func TestNHA042ParallelErrorWaitsOtherBranch(t *testing.T) {
	id, _ := ir.ParseScriptID("script.nha042_parallel")
	body := "sequence:\n- parallel:\n  - sequence:\n    - action: switch.turn_on\n      target: {entity_id: switch.nha042_fail}\n      data: {}\n  - sequence:\n    - wait_template: \"{{ is_state('switch.fixture', 'on') }}\"\n    - action: switch.turn_on\n      target: {entity_id: switch.nha042_other_branch}\n      data: {}\n" + nha042Action("after_parallel")
	program := nha042Compile(t, "script.nha042_parallel", body, []ir.ScriptID{id})
	failure := errors.New("synthetic branch error")
	rig := nha042FullRig(t, []ir.Script{program}, &nha042Commands{fail: "switch.nha042_fail", failure: failure})
	handle, err := rig.controller.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDirect)
	if err != nil {
		t.Fatal(err)
	}
	nha042Await(t, func() bool { return rig.waiting(handle.ID()) && len(rig.effects()) == 1 })
	select {
	case <-handle.Done():
		t.Fatal("parallel returned before other branch joined")
	default:
	}
	observed := nha042Observed(t, "parallel_error_waits_other_branch")
	nha042Compare(t, rig.effects(), observed.Before.Effects)
	rig.state(t, true)
	if err := nha042Joined(t, handle); !errors.Is(err, failure) {
		t.Fatalf("branch error cause lost: %v", err)
	}
	nha042Compare(t, rig.effects(), observed.Effects)
}

type nha042CountTrigger struct {
	*automationtriggers.WaitHandle
	closes atomic.Int64
}

func (h *nha042CountTrigger) Close() <-chan struct{} { h.closes.Add(1); return h.WaitHandle.Close() }

type nha042CountTemplate struct {
	*reactivetemplate.WaitHandle
	closes atomic.Int64
}

func (h *nha042CountTemplate) Close(ctx context.Context) error {
	h.closes.Add(1)
	return h.WaitHandle.Close(ctx)
}

func TestNHA042WaitCleanupExactlyOnce(t *testing.T) {
	id, _ := ir.ParseScriptID("script.nha042_cleanup")
	script := nha042Compile(t, "script.nha042_cleanup", "sequence:\n- wait_for_trigger:\n  - trigger: event\n    event_type: nha042_release\n- stop: done\n", []ir.ScriptID{id})
	rig := nha042FullRig(t, []ir.Script{script}, &nha042Commands{})
	for _, completed := range []bool{false, true} {
		t.Run(fmt.Sprint(completed), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			trigger, err := rig.interpreter.input.TriggerWaits.ObserveWait(ctx, script.Waits()[0].Source())
			if err != nil {
				t.Fatal(err)
			}
			program, _ := template.Compile("{{ is_state('switch.fixture', 'on') }}")
			observer, err := rig.interpreter.input.TemplateWaits.ObserveTemplateOnClock(ctx, program, nil, rig.clock)
			if err != nil {
				t.Fatal(err)
			}
			countedTrigger := &nha042CountTrigger{WaitHandle: trigger}
			countedTemplate := &nha042CountTemplate{WaitHandle: observer}
			timer := rig.clock.NewTimer(time.Second).(*nha042Timer)
			resources := waitResources{trigger: countedTrigger, template: countedTemplate, timer: timer}
			if completed {
				event, _ := integrations.NewDeviceEvent("nha042_release", rig.clock.Now(), nil)
				if err := rig.session.Emit(ctx, rig.binding, event); err != nil {
					t.Fatal(err)
				}
				rig.state(t, true)
				select {
				case <-trigger.Ready():
				case <-time.After(3 * time.Second):
					t.Fatal("trigger did not complete")
				}
				select {
				case <-observer.Ready():
				case <-time.After(3 * time.Second):
					t.Fatal("template did not complete")
				}
				rig.clock.advance(time.Second)
			} else {
				cancel()
			}
			if err := resources.close(); err != nil {
				t.Fatal(err)
			}
			if err := resources.close(); err != nil {
				t.Fatal(err)
			}
			cancel()
			if countedTrigger.closes.Load() != 1 || countedTemplate.closes.Load() != 1 || timer.stops.Load() != 1 {
				t.Fatalf("actual Close/Stop calls: %d/%d/%d", countedTrigger.closes.Load(), countedTemplate.closes.Load(), timer.stops.Load())
			}
			select {
			case <-trigger.Done():
			default:
				t.Fatal("trigger cleanup returned before join")
			}
			select {
			case <-observer.Done():
			default:
				t.Fatal("template cleanup returned before join")
			}
		})
	}
	// Invalid timer channels are failures, but their actual Stop still runs.
	invalid := &nha042Timer{}
	resources := waitResources{timer: invalid}
	_ = resources.close()
	_ = resources.close()
	if invalid.stops.Load() != 1 {
		t.Fatal("invalid-channel timer was not stopped exactly once")
	}
}
func TestNHA042DelayAndWaitTemplateCancellation(t *testing.T) {
	for _, step := range []string{"- delay: {seconds: 60}\n", "- wait_template: '{{ false }}'\n"} {
		t.Run(step, func(t *testing.T) {
			id, _ := ir.ParseScriptID("script.nha042_cancel")
			program := nha042Compile(t, "script.nha042_cancel", "sequence:\n"+step+nha042Action("parent_exit"), []ir.ScriptID{id})
			rig := nha042FullRig(t, []ir.Script{program}, &nha042Commands{})
			handle, err := rig.controller.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDirect)
			if err != nil {
				t.Fatal(err)
			}
			startup, err := handle.Startup(context.Background())
			if err != nil || startup.Kind() != StartupSuspended {
				t.Fatalf("startup=%v %v", startup.Kind(), err)
			}
			<-handle.Cancel()
			_ = nha042Joined(t, handle)
			rig.clock.advance(2 * time.Minute)
			if len(rig.effects()) != 0 {
				t.Fatal("action ran after cancellation")
			}
		})
	}
}
func TestNHA042ScriptStartupTransitionAndEarlyTerminal(t *testing.T) {
	for _, test := range []struct {
		mode, steps string
		expected    StartupKind
	}{
		{"single", "- stop: done\n", StartupTerminated},
		{"single", "- wait_template: '{{ false }}'\n", StartupSuspended},
	} {
		id, _ := ir.ParseScriptID("script.nha042_startup")
		script := nha042Compile(t, "script.nha042_startup", "mode: "+test.mode+"\nsequence:\n"+test.steps, []ir.ScriptID{id})
		rig := nha042FullRig(t, []ir.Script{script}, &nha042Commands{})
		handle, err := rig.controller.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDetached)
		if err != nil {
			t.Fatal(err)
		}
		startup, err := handle.Startup(context.Background())
		if err != nil || startup.Kind() != test.expected {
			t.Fatalf("startup=%v expected=%v err=%v", startup.Kind(), test.expected, err)
		}
		<-handle.Cancel()
	}
	id, _ := ir.ParseScriptID("script.nha042_queued")
	script := nha042Compile(t, "script.nha042_queued", "mode: queued\nsequence:\n- wait_template: '{{ false }}'\n", []ir.ScriptID{id})
	rig := nha042FullRig(t, []ir.Script{script}, &nha042Commands{})
	first, err := rig.controller.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDetached)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := rig.controller.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDetached)
	if err != nil {
		t.Fatal(err)
	}
	startup, err := second.Startup(context.Background())
	if err != nil || startup.Kind() != StartupQueuedChange {
		t.Fatalf("queued change mistaken for executing action: %v %v", startup.Kind(), err)
	}
	select {
	case <-second.Done():
		t.Fatal("queued run already terminal")
	default:
	}
	<-second.Cancel()
	<-first.Cancel()
}
func TestNHA042ParallelStopAndErrorJoinInSourceOrder(t *testing.T) {
	for _, stopFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(stopFirst), func(t *testing.T) {
			id, _ := ir.ParseScriptID("script.nha042_halt")
			first := "    - action: switch.turn_on\n      target: {entity_id: switch.nha042_fail}\n      data: {}\n"
			second := "    - stop: branch done\n"
			if stopFirst {
				first, second = second, first
			}
			body := "sequence:\n- parallel:\n  - sequence:\n" + first + "  - sequence:\n" + second + nha042Action("after_parallel")
			script := nha042Compile(t, "script.nha042_halt", body, []ir.ScriptID{id})
			failure := errors.New("synthetic branch error")
			rig := nha042FullRig(t, []ir.Script{script}, &nha042Commands{fail: "switch.nha042_fail", failure: failure})
			handle, err := rig.controller.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDirect)
			if err != nil {
				t.Fatal(err)
			}
			err = nha042Joined(t, handle)
			if stopFirst && err != nil || !stopFirst && !errors.Is(err, failure) {
				t.Fatalf("ordered gather result: %v", err)
			}
			nha042Compare(t, rig.effects(), []string{"switch.nha042_fail"})
		})
	}
}
func TestNHA042NodeTracesIncludeBranchAndIteration(t *testing.T) {
	id, _ := ir.ParseScriptID("script.nha042_paths")
	script := nha042Compile(t, "script.nha042_paths", "sequence:\n- repeat:\n    count: 2\n    sequence:\n    - parallel:\n      - sequence:\n        - stop: first\n      - sequence:\n        - stop: second\n", []ir.ScriptID{id})
	rig := nha042FullRig(t, []ir.Script{script}, &nha042Commands{})
	handle, err := rig.controller.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDirect)
	if err != nil {
		t.Fatal(err)
	}
	if err := nha042Joined(t, handle); err != nil {
		t.Fatal(err)
	}
	traces, _ := rig.interpreter.NodeTraces()
	branches := make(map[int]bool)
	for _, trace := range traces {
		if trace.Kind() != ir.ActionStop || trace.Stage() != NodeStopped {
			continue
		}
		if !reflect.DeepEqual(trace.Iterations(), []int{1}) || len(trace.Branches()) != 1 {
			t.Fatalf("dynamic source lost: %+v %+v", trace.Iterations(), trace.Branches())
		}
		branches[trace.Branches()[0]] = true
		copy := trace.Branches()
		copy[0] = 99
		if trace.Branches()[0] == 99 {
			t.Fatal("trace path accessor not copied")
		}
	}
	if !branches[0] || !branches[1] {
		t.Fatal("joined branches not both traced")
	}
}

func TestNHA042RuntimeBindingsPreserveAbsenceZeroFalseAndCopies(t *testing.T) {
	pool := &memoryPool{}
	if !pool.adjust(0, preparationCharge) {
		t.Fatal("reservation refused")
	}
	fields := []template.Binding{{Name: "zero", Value: template.Int(0)}, {Name: "off", Value: template.Bool(false)}, {Name: "absent", Value: template.None()}}
	nested, err := template.Object(fields)
	if err != nil {
		t.Fatal(err)
	}
	arena, root, err := newRunArena(pool, []template.Binding{{Name: "nested", Value: nested}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer arena.close()
	fields[0].Value = template.Int(99)
	branch, err := root.ForkBranch()
	if err != nil {
		t.Fatal(err)
	}
	defer branch.closeBranch()
	changed, _ := template.Object([]template.Binding{{Name: "zero", Value: template.Int(1)}})
	if err := branch.Set("nested", changed); err != nil {
		t.Fatal(err)
	}
	value, present := root.Get("nested")
	zero, hasZero := value.Field("zero")
	off, hasOff := value.Field("off")
	absent, hasAbsent := value.Field("absent")
	number, numberOK := zero.Int()
	boolean, boolOK := off.Bool()
	kind, kindErr := absent.Kind()
	if !present || !hasZero || !hasOff || !hasAbsent || !numberOK || number != 0 || !boolOK || boolean || kindErr != nil || kind != template.KindNone {
		t.Fatal("absence/zero/false or source copy lost")
	}
	noTimeout := waitObservation{kind: ir.ActionWait, exit: WaitCancelled}
	projected, err := noTimeout.value()
	if err != nil {
		t.Fatal(err)
	}
	remaining, _ := projected.Field("remaining")
	trigger, _ := projected.Field("trigger")
	remainingKind, _ := remaining.Kind()
	triggerKind, _ := trigger.Kind()
	if remainingKind != template.KindNone || triggerKind != template.KindNone {
		t.Fatal("absent timeout/trigger became zero")
	}
	immediate := waitObservation{kind: ir.ActionWaitTemplate, completed: true, hasRemaining: true, exit: WaitCompleted}
	projected, err = immediate.value()
	if err != nil {
		t.Fatal(err)
	}
	if _, present := projected.Field("trigger"); present {
		t.Fatal("template fabricated trigger field")
	}
	remaining, _ = projected.Field("remaining")
	if seconds, valid := remaining.Float(); !valid || seconds != 0 {
		t.Fatal("zero timeout lost")
	}
	if _, present := root.Get("ctx"); present {
		t.Fatal("runtime environment fabricated a ctx binding")
	}
	bindings, release, err := root.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctxProgram, err := template.Compile("{{ ctx is defined }}")
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := template.NewInputs(nil, bindings, time.Now(), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := template.EvaluateTracked(context.Background(), ctxProgram, inputs, template.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	result, present := tracked.Result()
	if !present || result.Text() != "False" {
		t.Fatal("absent runtime ctx binding was reported as defined")
	}
}
func TestNHA042ExecutionBudgetsBeforeAllocationAndAction(t *testing.T) {
	id, _ := ir.ParseScriptID("script.nha042_budget")
	script := nha042Compile(t, "script.nha042_budget", "sequence:\n- wait_template: '{{ false }}'\n"+nha042Action("parent_exit"), []ir.ScriptID{id})
	rig := nha042FullRig(t, []ir.Script{script}, &nha042Commands{})
	rig.controller.pool.mu.Lock()
	baseline := rig.controller.pool.used
	rig.controller.pool.mu.Unlock()
	bindings := make([]template.Binding, MaxBindings)
	for index := range bindings {
		bindings[index] = template.Binding{Name: fmt.Sprintf("v%d", index), Value: template.Int(0)}
	}
	if handle, err := rig.controller.InvokeScript(context.Background(), id, bindings, Run{}, ir.ScriptCallDirect); handle != nil || !errors.Is(err, ErrCapacity) {
		t.Fatalf("runtime wait slot exceeded admitted names: %v %v", handle, err)
	}
	rig.controller.pool.mu.Lock()
	after := rig.controller.pool.used
	rig.controller.pool.mu.Unlock()
	if after != baseline || len(rig.effects()) != 0 {
		t.Fatal("refused admission allocated persistent state or action")
	}
	work := &runWork{}
	work.visits.Store(MaxNodeVisits)
	node := rig.interpreter.scripts[id].nodes[1]
	if err := work.visit(node.source); err == nil {
		t.Fatal("node work exhausted without refusal")
	}
	// No command executor receives a synthetic zero CausalKey or Actor.
	execution := CommandExecution{}
	if _, err := execution.CausalKey(); err == nil {
		t.Fatal("zero execution admitted")
	}
	var calls atomic.Int64
	execution.suspend = func() { calls.Add(1) }
	execution.Suspend()
	if calls.Load() != 1 {
		t.Fatal("normal port suspension callback not called")
	}
}
func TestNHA042DetachedSurvivesCallerButJoinsAtRuntimeClose(t *testing.T) {
	childID, _ := ir.ParseScriptID("script.nha042_child")
	parentID, _ := ir.ParseScriptID("script.nha042_parent")
	ids := []ir.ScriptID{childID, parentID}
	child := nha042Compile(t, "script.nha042_child", "sequence:\n- wait_template: '{{ false }}'\n"+nha042Action("child_exit"), ids)
	parent := nha042Compile(t, "script.nha042_parent", "sequence:\n- action: script.turn_on\n  target: {entity_id: script.nha042_child}\n- stop: done\n", ids)
	rig := nha042FullRig(t, []ir.Script{child, parent}, &nha042Commands{})
	caller, cancel := context.WithCancel(context.Background())
	handle, err := rig.controller.InvokeScript(caller, parentID, nil, Run{}, ir.ScriptCallDetached)
	if err != nil {
		t.Fatal(err)
	}
	if err := nha042Joined(t, handle); err != nil {
		t.Fatal(err)
	}
	cancel()
	childHandle := rig.activeHandle(childID)
	if childHandle == nil {
		t.Fatal("detached child disappeared")
	}
	select {
	case <-childHandle.Done():
		t.Fatal("detached child died with caller")
	default:
	}
	closed := rig.controller.BeginClose()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("runtime did not join detached child")
	}
	select {
	case <-childHandle.Done():
	default:
		t.Fatal("controller Done preceded child join")
	}
	if len(rig.effects()) != 0 {
		t.Fatal("action after runtime close")
	}
}
func TestNHA042QueuedRestartRecursionAndStopSelf(t *testing.T) {
	id, _ := ir.ParseScriptID("script.nha042_self")
	for _, mode := range []string{"queued", "restart"} {
		t.Run(mode, func(t *testing.T) {
			script := nha042Compile(t, "script.nha042_self", "mode: "+mode+"\nsequence:\n- action: script.nha042_self\n", []ir.ScriptID{id})
			rig := nha042FullRig(t, []ir.Script{script}, &nha042Commands{})
			handle, err := rig.controller.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDirect)
			if err != nil {
				t.Fatal(err)
			}
			var failure *ExecutionError
			if err := nha042Joined(t, handle); !errors.As(err, &failure) || failure.Kind() != ExecutionRecursion {
				t.Fatalf("recursive %s deadlocked or lost typed refusal: %v", mode, err)
			}
		})
	}
	script := nha042Compile(t, "script.nha042_self", "sequence:\n- action: script.turn_off\n  target: {entity_id: script.nha042_self}\n"+nha042Action("parent_exit"), []ir.ScriptID{id})
	rig := nha042FullRig(t, []ir.Script{script}, &nha042Commands{})
	handle, err := rig.controller.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDirect)
	if err != nil {
		t.Fatal(err)
	}
	_ = nha042Joined(t, handle)
	if len(rig.effects()) != 0 {
		t.Fatal("self Stop continued actions")
	}
}

func TestNHA042RealExecutionExhaustsSharedWorkLoopsAndTraceRing(t *testing.T) {
	for _, kind := range []string{"loop-limit", "node-visit-limit", "parallel-shared-loop-limit"} {
		t.Run(kind, func(t *testing.T) {
			id, err := ir.ParseScriptID("script.nha042_real_budget")
			if err != nil {
				t.Fatal(err)
			}
			call := "- action: switch.turn_on\n  target: {entity_id: switch.nha042_fixture}\n  data: {}\n"
			indent := func(body string, spaces int) string {
				return strings.Repeat(" ", spaces) + strings.ReplaceAll(strings.TrimSuffix(body, "\n"), "\n", "\n"+strings.Repeat(" ", spaces)) + "\n"
			}
			body, expected := "", MaxLoopIterations-1
			switch kind {
			case "loop-limit":
				body = "sequence:\n- repeat:\n    count: 2\n    sequence:\n    - repeat:\n        count: 10000\n        sequence:\n" + indent(call, 8)
			case "node-visit-limit":
				body = "sequence:\n- repeat:\n    count: 10000\n    sequence:\n"
				for index := 0; index < 9; index++ {
					body += fmt.Sprintf("    - variables: {step_%d: 0}\n", index)
				}
				body += indent(call, 4)
			case "parallel-shared-loop-limit":
				body = "sequence:\n- parallel:\n"
				for branch := 0; branch < 2; branch++ {
					body += "  - sequence:\n    - repeat:\n        count: 10000\n        sequence:\n" + indent(call, 8)
				}
				expected = MaxLoopIterations
			default:
				t.Fatal("unhandled test budget")
			}
			body += nha042Action("parent_exit")
			script := nha042Compile(t, "script.nha042_real_budget", body, []ir.ScriptID{id})
			commands := &nha042Commands{}
			controller, interpreter := nha042Runtime(t, []ir.Script{script}, commands)
			controller.pool.mu.Lock()
			baseline := controller.pool.used
			controller.pool.mu.Unlock()
			handle, err := controller.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDirect)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var exhausted *ExecutionError
			if err := handle.Result(ctx); !errors.As(err, &exhausted) || exhausted.Kind() != ExecutionWorkBudget || exhausted.Source().Object() != "script.nha042_real_budget" {
				t.Fatalf("real admitted program did not stop at its shared budget: %v", err)
			}
			commands.mu.Lock()
			effects := append([]string(nil), commands.aliases...)
			commands.mu.Unlock()
			if len(effects) != expected {
				t.Fatalf("effect at or after exhausted budget: got%d want%d", len(effects), expected)
			}
			for _, effect := range effects {
				if effect != "switch.nha042_fixture" {
					t.Fatalf("action after budget refusal: %s", effect)
				}
			}
			traces, evicted := interpreter.NodeTraces()
			if len(traces) != nodeTraceCapacity || evicted == 0 {
				t.Fatalf("real trace production escaped bounded ring: len=%d cap=%d evicted=%d", len(traces), nodeTraceCapacity, evicted)
			}
			last := traces[len(traces)-1]
			if last.Stage() != NodeFailed || last.Failure() != ExecutionWorkBudget {
				t.Fatalf("trace omitted actual terminal budget failure: %+v", last)
			}
			controller.pool.mu.Lock()
			after := controller.pool.used
			controller.pool.mu.Unlock()
			if after != baseline {
				t.Fatalf("budget refusal leaked arena/parallel reservations: before%d after%d", baseline, after)
			}
		})
	}
}

// A normal CommandExecutor suspension boundary with a separately joined fake
// operation. Cancellation reaches it immediately but does not invent its exit.
type nha042GatedCommands struct {
	mu        sync.Mutex
	entered   chan RunID
	cancelled chan RunID
	releases  map[RunID]chan struct{}
}

func nha042CommandGate() *nha042GatedCommands {
	return &nha042GatedCommands{entered: make(chan RunID, MaxLiveRuns+1), cancelled: make(chan RunID, MaxLiveRuns+1), releases: make(map[RunID]chan struct{})}
}
func (g *nha042GatedCommands) ExecuteCall(ctx context.Context, _ ir.Call, execution CommandExecution) error {
	if _, err := execution.CausalKey(); err != nil {
		return err
	}
	execution.Suspend()
	g.mu.Lock()
	release := make(chan struct{})
	g.releases[execution.run] = release
	g.mu.Unlock()
	g.entered <- execution.run
	select {
	case <-release:
		return ctx.Err()
	case <-ctx.Done():
		g.cancelled <- execution.run
		<-release
		return ctx.Err()
	}
}
func (g *nha042GatedCommands) open(id RunID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if release := g.releases[id]; release != nil {
		select {
		case <-release:
		default:
			close(release)
		}
	}
}
func (g *nha042GatedCommands) openAll() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, release := range g.releases {
		select {
		case <-release:
		default:
			close(release)
		}
	}
}
func nha042GatedRuntime(t *testing.T, automations []ir.Automation, scripts []ir.Script, gate *nha042GatedCommands) *Controller {
	t.Helper()
	plan, err := NewPlanWithScripts(automations, scripts)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := domain.ParseActorID("nha042-gated-test-authority")
	if err != nil {
		t.Fatal(err)
	}
	interpreter, err := NewInterpreter(InterpreterInput{Plan: plan, Conditions: nha042SharedConditions(t), Authority: nha042Authority{actor}, Commands: gate, Clock: automationtriggers.RealClock{}, Zone: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	var reader ResultReader
	if len(automations) != 0 {
		reader = &nha041Reader{done: make(chan struct{})}
	}
	controller, err := NewController(plan, reader, interpreter, interpreter)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		gate.openAll()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := controller.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return controller
}

func TestNHA042ScriptFIFOAndRestartJoinLateOperation(t *testing.T) {
	for _, mode := range []string{"queued", "restart"} {
		t.Run(mode, func(t *testing.T) {
			id, err := ir.ParseScriptID("script.nha042_joined_modes")
			if err != nil {
				t.Fatal(err)
			}
			script := nha042Compile(t, "script.nha042_joined_modes", "mode: "+mode+"\nmax: 3\nsequence:\n- action: switch.turn_on\n  target: {entity_id: switch.nha042_fixture}\n  data: {}\n", []ir.ScriptID{id})
			gate := nha042CommandGate()
			controller := nha042GatedRuntime(t, nil, []ir.Script{script}, gate)
			controller.pool.mu.Lock()
			baseline := controller.pool.used
			controller.pool.mu.Unlock()
			first, err := controller.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDetached)
			if err != nil {
				t.Fatal(err)
			}
			if got := nha041Receive(t, gate.entered); got != first.ID() {
				t.Fatalf("first command identity: %d", got)
			}
			second, err := controller.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDetached)
			if err != nil {
				t.Fatal(err)
			}
			var third *ScriptHandle
			if mode == "queued" {
				third, err = controller.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDetached)
				if err != nil {
					t.Fatal(err)
				}
				startup, err := second.Startup(context.Background())
				if err != nil || startup.Kind() != StartupQueuedChange {
					t.Fatalf("queued receipt is not a named change: %+v %v", startup, err)
				}
			} else {
				if got := nha041Receive(t, gate.cancelled); got != first.ID() {
					t.Fatalf("restart cancelled another run: %d", got)
				}
				select {
				case <-first.Done():
					t.Fatal("restart invented old operation exit")
				default:
				}
				select {
				case <-second.entry.ready:
					t.Fatal("replacement reported execution before old join")
				default:
				}
			}
			select {
			case got := <-gate.entered:
				t.Fatalf("next mode run entered before previous operation joined: %d", got)
			default:
			}
			gate.open(first.ID())
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			firstErr := first.Result(ctx)
			if mode == "restart" && !errors.Is(firstErr, context.Canceled) || mode == "queued" && firstErr != nil {
				t.Fatalf("old terminal cause: %v", firstErr)
			}
			if got := nha041Receive(t, gate.entered); got != second.ID() {
				t.Fatalf("mode execution order: got%d second%d", got, second.ID())
			}
			if third != nil {
				select {
				case got := <-gate.entered:
					t.Fatalf("third run overtook second: %d", got)
				default:
				}
			}
			gate.open(second.ID())
			if err := second.Result(ctx); err != nil {
				t.Fatal(err)
			}
			if third != nil {
				if got := nha041Receive(t, gate.entered); got != third.ID() {
					t.Fatalf("FIFO third identity: got%d want%d", got, third.ID())
				}
				gate.open(third.ID())
				if err := third.Result(ctx); err != nil {
					t.Fatal(err)
				}
			}
			controller.pool.mu.Lock()
			after := controller.pool.used
			controller.pool.mu.Unlock()
			if after != baseline {
				t.Fatalf("mode transition leaked arenas: before%d after%d", baseline, after)
			}
		})
	}
}

func TestNHA042ScriptsAndAutomationsShareLiveAndMemoryBudgets(t *testing.T) {
	for _, memoryBound := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared64-live-runs", true: "real16MiB-pool"}[memoryBound], func(t *testing.T) {
			id, err := ir.ParseScriptID("script.nha042_shared_budget")
			if err != nil {
				t.Fatal(err)
			}
			sequence := "- action: switch.turn_on\n  target: {entity_id: switch.nha042_fixture}\n  data: {}\n"
			if !memoryBound {
				sequence = "- delay: {minutes: 60}\n"
			}
			script := nha042Compile(t, "script.nha042_shared_budget", "mode: parallel\nmax: 64\nsequence:\n"+sequence, []ir.ScriptID{id})
			automation := nha041Automation(t, "parallel", MaxLiveRuns)
			if !memoryBound {
				catalogue, err := ir.NewCatalogue(nil, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				file, err := parser.NewSourceFile("nha042-live-budget.yaml", []byte("id: nha042_live_budget\nmode: parallel\nmax: 64\ntriggers:\n- trigger: time_pattern\n  seconds: '*'\nactions:\n- delay: {minutes: 60}\n"))
				if err != nil {
					t.Fatal(err)
				}
				compiled, err := ir.CompileSources("automation.nha042_live_budget", "nha042-live-budget.yaml", []parser.SourceFile{file}, catalogue)
				if err != nil || len(compiled.Accepted()) != 1 {
					t.Fatalf("live-budget automation: %v %+v", err, compiled.Rejected())
				}
				automation = compiled.Accepted()[0]
			}
			gate := nha042CommandGate()
			controller := nha042GatedRuntime(t, []ir.Automation{automation}, []ir.Script{script}, gate)
			controller.pool.mu.Lock()
			baseline := controller.pool.used
			controller.pool.mu.Unlock()
			var parameters []template.Binding
			if memoryBound {
				for index := 0; index < MaxBindings; index++ {
					parameters = append(parameters, template.Binding{Name: fmt.Sprintf("owned_%d", index), Value: template.Int(0)})
				}
			}
			live := 0
			var delayed []*runEntry
			if !memoryBound {
				for count := 0; count < MaxLiveRuns/2; count++ {
					nha041Admit(t, controller, controller.plan.items[automation.ID()], "shared-budget")
					controller.mu.Lock()
					entry := controller.lanes[laneKey{executionAutomation, automation.ID()}].active[controller.next]
					controller.mu.Unlock()
					if entry == nil {
						t.Fatal("admitted automation is not live")
					}
					select {
					case <-entry.ready:
						if entry.startup.Kind() != StartupSuspended {
							t.Fatalf("automation did not reach its real delay timer: %v", entry.startup.Kind())
						}
					case <-time.After(3 * time.Second):
						t.Fatal("automation did not suspend on its timer")
					}
					delayed = append(delayed, entry)
					live++
				}
			}
			var refused error
			for live < MaxLiveRuns {
				handle, err := controller.InvokeScript(context.Background(), id, parameters, Run{}, ir.ScriptCallDetached)
				if err != nil {
					if handle != nil {
						t.Fatal("refusal returned a live handle")
					}
					refused = err
					break
				}
				if memoryBound {
					if got := nha041Receive(t, gate.entered); got != handle.ID() {
						t.Fatalf("script port identity changed: %d", got)
					}
				} else {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					startup, err := handle.Startup(ctx)
					cancel()
					if err != nil || startup.Kind() != StartupSuspended {
						t.Fatalf("script did not reach its real delay timer: %v %v", startup.Kind(), err)
					}
					delayed = append(delayed, handle.entry)
				}
				live++
			}
			controller.pool.mu.Lock()
			beforeRefusal := controller.pool.used
			controller.pool.mu.Unlock()
			if !memoryBound {
				handle, err := controller.InvokeScript(context.Background(), id, nil, Run{}, ir.ScriptCallDetached)
				var admission *AdmissionError
				controller.mu.Lock()
				actualLive := controller.live
				controller.mu.Unlock()
				if live != MaxLiveRuns || actualLive != MaxLiveRuns || handle != nil || !errors.As(err, &admission) || admission.Decision() != DecisionMemorySaturated {
					t.Fatalf("per-lane admission escaped shared64 ceiling: live%d handle%v err%v", live, handle, err)
				}
			} else {
				if live == 0 || live >= MaxLiveRuns || !errors.Is(refused, ErrCapacity) {
					t.Fatalf("real run environments did not exhaust common pool: live%d err%v", live, refused)
				}
				if handle, err := controller.InvokeScript(context.Background(), id, parameters, Run{}, ir.ScriptCallDetached); handle != nil || !errors.Is(err, ErrCapacity) {
					t.Fatalf("saturated pool admitted another environment: %v %v", handle, err)
				}
			}
			select {
			case got := <-gate.entered:
				t.Fatalf("refused admission reached command port: %d", got)
			default:
			}
			controller.pool.mu.Lock()
			afterRefusal := controller.pool.used
			controller.pool.mu.Unlock()
			if afterRefusal != beforeRefusal || afterRefusal > MaxMemoryBytes {
				t.Fatalf("refusal changed real retained charge: before%d after%d", beforeRefusal, afterRefusal)
			}
			done := controller.BeginClose()
			if memoryBound {
				for count := 0; count < live; count++ {
					nha041Receive(t, gate.cancelled)
				}
				select {
				case <-done:
					t.Fatal("runtime Done preceded real late operations")
				default:
				}
				gate.openAll()
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("shared controller did not join all actual operations")
			}
			for _, entry := range delayed {
				select {
				case <-entry.done:
				default:
					t.Fatal("controller Done preceded an actual delayed run join")
				}
			}
			controller.pool.mu.Lock()
			after := controller.pool.used
			controller.pool.mu.Unlock()
			if after != baseline {
				t.Fatalf("joined shared runs leaked memory: before%d after%d", baseline, after)
			}
		})
	}
}

// Wait-only ingress uses the existing mapper, supervisor and one TCP session.
// This repository admits no discovery document or physical command source.
type nha042WaitOnlyRepository struct{}

func (nha042WaitOnlyRepository) MatchMQTTSource(context.Context, domain.IntegrationInstanceID, []string, string) (integrations.SourceKey, error) {
	return integrations.SourceKey{}, integrations.ErrInvalidInput
}
func (nha042WaitOnlyRepository) MQTTTopicSources(context.Context, domain.IntegrationInstanceID, string) ([]integrations.SourceKey, error) {
	return nil, nil
}
func (nha042WaitOnlyRepository) RememberMQTTTopics(context.Context, domain.IntegrationInstanceID, string, []integrations.SourceKey) error {
	return nil
}
func (nha042WaitOnlyRepository) Tombstone(context.Context, domain.EntityID) error { return nil }

type nha042RetryTimer struct{ requested chan chan time.Time }

func (timer nha042RetryTimer) After(time.Duration) <-chan time.Time {
	next := make(chan time.Time, 1)
	timer.requested <- next
	return next
}

type nha042NoJitter struct{}

func (nha042NoJitter) Offset(time.Duration) time.Duration { return 0 }

type nha042RetainedWitness struct {
	mochi.HookBase
	topic         string
	retained      chan *mochi.Client
	subscriptions atomic.Int64
	overflow      atomic.Bool
}

func (*nha042RetainedWitness) ID() string { return "nha042-retained-wire-witness" }
func (*nha042RetainedWitness) Provides(event byte) bool {
	return event == mochi.OnPacketSent || event == mochi.OnPacketRead
}
func (h *nha042RetainedWitness) OnPacketSent(client *mochi.Client, packet mochipackets.Packet, _ []byte) {
	if string(client.Properties.Username) == testbroker.PilotUser && packet.FixedHeader.Type == mochipackets.Publish && packet.FixedHeader.Retain && packet.TopicName == h.topic {
		select {
		case h.retained <- client:
		default:
			h.overflow.Store(true)
		}
	}
}
func (h *nha042RetainedWitness) OnPacketRead(client *mochi.Client, packet mochipackets.Packet) (mochipackets.Packet, error) {
	if string(client.Properties.Username) == testbroker.PilotUser && packet.FixedHeader.Type == mochipackets.Subscribe {
		h.subscriptions.Add(1)
	}
	return packet, nil
}

func TestNHA042NestedMQTTWaitsShareIngressAcrossCancellationAndReconnect(t *testing.T) {
	ctx := context.Background()
	broker := testbroker.Start(t)
	const topic = "zigbee2mqtt/nha042/shared-wait"
	// Only this disposable broker receives retained data. Both restorations
	// must be consumed before the nonmatching live barrier below completes.
	broker.Publish(t, topic, []byte("release"), true)
	witness := &nha042RetainedWitness{topic: topic, retained: make(chan *mochi.Client, 2)}
	if err := broker.Server.AddHook(witness, nil); err != nil {
		t.Fatal(err)
	}
	settings, err := mqtt.ParseSettings(mqtt.Input{Profile: "5", Broker: "mqtt://" + broker.Address, Instance: "nha042-wait-ingress", Mode: "passive", UsernameSecret: "mqtt-username", PasswordSecret: "mqtt-password", Subscriptions: []string{"homeassistant/#", topic}})
	if err != nil {
		t.Fatal(err)
	}
	instance, err := settings.Instance()
	if err != nil {
		t.Fatal(err)
	}
	filter, err := mqtt.ParseTopicFilter(topic)
	if err != nil {
		t.Fatal(err)
	}
	ingress, err := mqtt.NewTriggerEdgePilot(settings, nha042WaitOnlyRepository{}, []mqtt.TopicFilter{filter})
	if err != nil {
		t.Fatal(err)
	}
	_, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.PublishRegistry(ctx, nil); err != nil {
		t.Fatal(err)
	}
	catalogue, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(fixture, fixture.Sequencer(), catalogue)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.EnableNativeMQTTEdges(instance); err != nil {
		t.Fatal(err)
	}
	cluster := testpg.Start(t)
	pg, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(ctx, pg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	secrets, err := store.BootstrapSecrets(ctx, filepath.Join(t.TempDir(), "app-data"))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"mqtt-username": testbroker.PilotUser, "mqtt-password": testbroker.PilotPassword} {
		secretName, err := storage.ParseSecretName(name)
		if err != nil {
			t.Fatal(err)
		}
		handle, err := storage.NewSecretHandle(instance, secretName)
		if err != nil {
			t.Fatal(err)
		}
		secret, err := storage.NewSecretValue([]byte(value))
		if err != nil {
			t.Fatal(err)
		}
		if err := secrets.RotateCredential(ctx, handle, secret); err != nil {
			t.Fatal(err)
		}
	}
	if err := host.AttachSecrets(secrets); err != nil {
		t.Fatal(err)
	}
	childID, _ := ir.ParseScriptID("script.nha042_mqtt_child")
	parentID, _ := ir.ParseScriptID("script.nha042_mqtt_parent")
	parallelID, _ := ir.ParseScriptID("script.nha042_mqtt_parallel")
	ids := []ir.ScriptID{childID, parentID, parallelID}
	child := nha042Compile(t, "script.nha042_mqtt_child", "mode: parallel\nmax: 4\nsequence:\n- wait_for_trigger:\n  - trigger: mqtt\n    topic: "+topic+"\n    payload: release\n    id: shared\n"+nha042Action("child_exit"), ids)
	parent := nha042Compile(t, "script.nha042_mqtt_parent", "mode: parallel\nmax: 4\nsequence:\n- action: script.nha042_mqtt_child\n"+nha042Action("parent_exit"), ids)
	parallel := nha042Compile(t, "script.nha042_mqtt_parallel", "sequence:\n- parallel:\n  - sequence:\n    - action: script.nha042_mqtt_child\n  - sequence:\n    - action: script.nha042_mqtt_child\n"+nha042Action("parent_exit"), ids)
	programs := []ir.Script{child, parent, parallel}
	triggerPlan, err := automationtriggers.NewPlan(automationtriggers.PlanInput{Scripts: programs, Zone: "UTC", Resolver: nha042Resolver{}, MQTTSettings: settings, MQTTIngress: ingress})
	if err != nil {
		t.Fatal(err)
	}
	triggers, err := automationtriggers.NewService(triggerPlan, fixture.Sequencer(), automationtriggers.RealClock{})
	if err != nil {
		t.Fatal(err)
	}
	if err := triggers.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := triggers.Close(); err != nil {
			t.Error(err)
		}
	})
	reactivePlan, err := reactivetemplate.NewPlan(nil, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	reactivePlan, err = reactivePlan.WithScriptWaits(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	reactive, err := reactivetemplate.Start(ctx, fixture.Sequencer(), reactivePlan, reactivetemplate.RealClock{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reactive.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	plan, err := NewPlanWithScripts(nil, programs)
	if err != nil {
		t.Fatal(err)
	}
	actor, _ := domain.ParseActorID("nha042-explicit-mqtt-caller")
	commands := &nha042Commands{}
	interpreter, err := NewInterpreter(InterpreterInput{Plan: plan, TriggerWaits: triggers, TemplateWaits: reactive, Conditions: reactive, Authority: nha042Authority{actor}, Commands: commands, Clock: automationtriggers.RealClock{}, Zone: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewController(plan, nil, interpreter, interpreter)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		if err := controller.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	spec, err := ingress.PilotSpec()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := integrations.NewRetryPolicy(1, time.Millisecond, time.Second, 4*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	retry := nha042RetryTimer{requested: make(chan chan time.Time, 1)}
	supervisor, err := integrations.NewSupervisor(host, []integrations.PilotSpec{spec}, policy, retry, nha042NoJitter{})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := supervisor.Stop(closeCtx); err != nil {
			t.Error(err)
		}
	})
	running := func(attempt int) {
		t.Helper()
		nha042Await(t, func() bool {
			status, ok := supervisor.Status(instance)
			return ok && status.Phase == integrations.PilotRunning && status.Attempts == attempt
		})
	}
	running(1) // successful real SUBACK, not merely a declared factory/filter
	retainedConnection := func() *mochi.Client {
		t.Helper()
		select {
		case client := <-witness.retained:
			return client
		case <-time.After(3 * time.Second):
			t.Fatal("retained restoration was not actually sent on the pilot TCP connection")
			return nil
		}
	}
	firstConnection := retainedConnection()
	readEdges := func() []struct {
		session  string
		payload  string
		sequence uint64
	} {
		t.Helper()
		start, err := fixture.Sequencer().Start()
		if err != nil {
			t.Fatal(err)
		}
		entries, _, err := fixture.Sequencer().ReadAfter(start, 256)
		if err != nil {
			t.Fatal(err)
		}
		var edges []struct {
			session  string
			payload  string
			sequence uint64
		}
		for _, entry := range entries {
			if edge, present := entry.NativeMQTTEdge(); present {
				name, payload, sequence, err := edge.Components()
				if err != nil || name != topic {
					t.Fatalf("unexpected native edge: %q %v", name, err)
				}
				actualInstance, source, _, hasOrigin := entry.Origin()
				if !hasOrigin || actualInstance != instance {
					t.Fatal("native edge lost session origin")
				}
				sourceSession, sourceSequence, err := source.Components()
				if err != nil || sourceSequence == nil || *sourceSequence != sequence {
					t.Fatalf("native edge lost typed source sequence: %v %v", sourceSequence, err)
				}
				edges = append(edges, struct {
					session  string
					payload  string
					sequence uint64
				}{sourceSession, string(payload), sequence})
			}
		}
		return edges
	}
	childWaiting := func(count int) {
		t.Helper()
		nha042Await(t, func() bool {
			traces, _ := interpreter.NodeTraces()
			latest := make(map[RunID]NodeStage)
			for _, trace := range traces {
				if trace.Source().Object() == "script.nha042_mqtt_child" {
					latest[trace.RunID()] = trace.Stage()
				}
			}
			waiting := 0
			for _, stage := range latest {
				if stage == NodeWaiting {
					waiting++
				}
			}
			return waiting == count
		})
	}
	first, err := controller.InvokeScript(ctx, parentID, nil, Run{}, ir.ScriptCallDirect)
	if err != nil {
		t.Fatal(err)
	}
	second, err := controller.InvokeScript(ctx, parentID, nil, Run{}, ir.ScriptCallDirect)
	if err != nil {
		t.Fatal(err)
	}
	childWaiting(2)
	barrier := func(expected int) {
		t.Helper()
		// Publish joins TCP PUBACK plus this connection's broker fanout (182).
		// The native edge witnesses actual pilot/Host consumption afterwards.
		broker.Publish(t, topic, []byte("barrier"), false)
		nha042Await(t, func() bool { return len(readEdges()) == expected })
		probe, err := triggers.ObserveWait(ctx, child.Waits()[0].Source())
		if err != nil {
			t.Fatal(err)
		}
		// ObserveWait flushes the existing reader through the current journal
		// head, so retained processing cannot be hidden by a scheduling delay.
		<-probe.Close()
		commands.mu.Lock()
		count := len(commands.aliases)
		commands.mu.Unlock()
		if count != 0 {
			t.Fatalf("restored or nonmatching packet triggered %d effects", count)
		}
		for _, handle := range []*ScriptHandle{first, second} {
			select {
			case <-handle.Done():
				t.Fatal("nonmatching barrier completed a nested wait")
			default:
			}
		}
	}
	barrier(1)
	first.Cancel()
	if err := nha042Joined(t, first); !errors.Is(err, context.Canceled) {
		t.Fatalf("first parent cancellation lost: %v", err)
	}
	childWaiting(1)
	clients := broker.Server.Clients.GetAll()
	if len(clients) == 0 {
		t.Fatal("real pilot connection absent")
	}
	var pilotClientID string
	for _, client := range clients {
		if string(client.Properties.Username) == testbroker.PilotUser && !client.Closed() {
			if pilotClientID != "" {
				t.Fatal("more than one pilot TCP connection")
			}
			pilotClientID = client.ID
			client.Stop(errors.New("synthetic NHA042 TCP loss"))
		}
	}
	if pilotClientID == "" {
		t.Fatal("pilot TCP connection not identified")
	}
	var release chan time.Time
	select {
	case release = <-retry.requested:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not join retired pilot before retry")
	}
	broker.WaitSessionRemoved(t, pilotClientID)
	select {
	case <-second.Done():
		t.Fatal("one cancelled consumer or session retirement completed the other wait")
	default:
	}
	release <- time.Now()
	running(2)
	if successor := retainedConnection(); successor == firstConnection || successor.ID != firstConnection.ID {
		t.Fatal("retry did not replace the actual TCP connection under the same configured client identity")
	}
	// First was cancelled: this barrier checks the remaining waiter only.
	broker.Publish(t, topic, []byte("barrier"), false)
	nha042Await(t, func() bool { return len(readEdges()) == 2 })
	probe, err := triggers.ObserveWait(ctx, child.Waits()[0].Source())
	if err != nil {
		t.Fatal(err)
	}
	<-probe.Close()
	select {
	case <-second.Done():
		t.Fatal("reconnect replay fired the remaining wait")
	default:
	}
	commands.mu.Lock()
	beforeLive := len(commands.aliases)
	commands.mu.Unlock()
	if beforeLive != 0 {
		t.Fatal("reconnect manufactured a command")
	}
	broker.Publish(t, topic, []byte("release"), false)
	if err := nha042Joined(t, second); err != nil {
		t.Fatal(err)
	}
	third, err := controller.InvokeScript(ctx, parallelID, nil, Run{}, ir.ScriptCallDirect)
	if err != nil {
		t.Fatal(err)
	}
	childWaiting(2)
	broker.Publish(t, topic, []byte("release"), false)
	if err := nha042Joined(t, third); err != nil {
		t.Fatal(err)
	}
	edges := readEdges()
	if len(edges) != 4 || edges[0].payload != "barrier" || edges[1].payload != "barrier" || edges[2].payload != "release" || edges[3].payload != "release" {
		t.Fatalf("retained/duplicate edge count changed: %+v", edges)
	}
	if edges[0].session == edges[1].session || edges[1].session != edges[2].session || edges[2].session != edges[3].session || edges[0].sequence != 1 || edges[1].sequence != 1 || edges[2].sequence != 2 || edges[3].sequence != 3 {
		t.Fatalf("new source session or same-payload live identity lost: %+v", edges)
	}
	commands.mu.Lock()
	effects := append([]string(nil), commands.aliases...)
	commands.mu.Unlock()
	children, parents := 0, 0
	for _, effect := range effects {
		switch effect {
		case "switch.nha042_child_exit":
			children++
		case "switch.nha042_parent_exit":
			parents++
		default:
			t.Fatalf("unexpected fake effect %q", effect)
		}
	}
	if children != 3 || parents != 2 {
		t.Fatalf("shared/nested wait results: children%d parents%d effects%v", children, parents, effects)
	}
	if broker.PublishCount(pilotClientID) != 0 {
		t.Fatal("passive pilot emitted an application PUBLISH")
	}
	if subscriptions := witness.subscriptions.Load(); subscriptions != 2 {
		t.Fatalf("transient consumer registration/removal changed static broker subscriptions: %d want2 connection SUBSCRIBEs", subscriptions)
	}
	if witness.overflow.Load() {
		t.Fatal("retained connection witness overflowed its two-attempt bound")
	}
}
