package reactivetemplate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/sequencer"
)

type nha044Clock struct {
	mu     sync.Mutex
	now    time.Time
	count  atomic.Int64
	timers []*nha044Timer
}

func (c *nha044Clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *nha044Clock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count.Add(1)
	timer := &nha044Timer{channel: make(chan time.Time, 1), due: c.now.Add(d)}
	c.timers = append(c.timers, timer)
	return timer
}

func (c *nha044Clock) Advance(to time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = to
	for _, timer := range c.timers {
		if !timer.stopped.Load() && !timer.due.After(to) {
			select {
			case timer.channel <- to:
			default:
			}
			timer.stopped.Store(true)
		}
	}
}

type nha044Timer struct {
	channel chan time.Time
	due     time.Time
	stopped atomic.Bool
}

func (t *nha044Timer) C() <-chan time.Time { return t.channel }
func (t *nha044Timer) Stop() bool          { return !t.stopped.Swap(true) }

func nha044Record(t *testing.T, binding integrations.Binding, instance domain.IntegrationInstanceID, aliasName string) integrations.RegistryRecord {
	t.Helper()
	device, entity, err := binding.IDs()
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("fixture-controller", "relay")
	if err != nil {
		t.Fatal(err)
	}
	alias, err := domain.ParseHAAlias(aliasName)
	if err != nil {
		t.Fatal(err)
	}
	record, err := integrations.NewRegistryRecord(device, entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func nha044Fixture(t *testing.T, name string, host *integrations.Manager) (*integrations.Session, integrations.Binding, integrations.RegistryRecord) {
	t.Helper()
	instance, err := domain.ParseIntegrationInstanceID(name)
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := fake.NewDriver(time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	binding, err := driver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	return session, binding, nha044Record(t, binding, instance, "switch."+name)
}

func nha044Update(t *testing.T, updates <-chan Update) Update {
	t.Helper()
	select {
	case update, ok := <-updates:
		if !ok {
			t.Fatal("updates closed before result")
		}
		return update
	case <-time.After(2 * time.Second):
		t.Fatal("reactive update timeout")
	}
	return Update{}
}

type nha044NilContext struct{ context.Context }
type nha044NilClock struct{}

func (*nha044NilClock) Now() time.Time               { panic("typed nil clock used") }
func (*nha044NilClock) NewTimer(time.Duration) Timer { panic("typed nil clock used") }

func TestNHA044TypedNilContextAndClockAreRejected(t *testing.T) {
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	sequence, err := sequencer.New(nha044SequenceClock{at: at}, 128)
	if err != nil {
		t.Fatal(err)
	}
	if err := sequence.PublishRegistry(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	definition := nha044Definition(t, "nil-boundary", "{{ 'ready' }}", nil)
	plan, err := NewPlan([]Definition{definition}, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	var nilCtx *nha044NilContext
	if _, err := Start(nilCtx, sequence, plan, RealClock{}); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("typed nil Start context: %v", err)
	}
	if _, err := Start(context.Background(), sequence, Plan{}, RealClock{}); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("zero plan: %v", err)
	}
	var absentClock Clock
	if _, err := Start(context.Background(), sequence, plan, absentClock); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("absent clock: %v", err)
	}
	var nilClock *nha044NilClock
	if _, err := Start(context.Background(), sequence, plan, nilClock); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("typed nil Start clock: %v", err)
	}
	service, err := Start(context.Background(), sequence, plan, RealClock{})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())
	if err := service.ReplaceGroups(nilCtx, nil); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("typed nil ReplaceGroups context: %v", err)
	}
	if err := service.Close(nilCtx); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("typed nil Close context: %v", err)
	}
	if _, ok := (Update{}).DisplayResult(); ok {
		t.Fatal("zero update fabricated a display result")
	}
	if _, ok := (Update{}).CurrentResult(); ok {
		t.Fatal("zero update fabricated a current result")
	}
}

func TestNHA044RealSequencerProjectionAndUnrelatedState(t *testing.T) {
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	first, firstBinding, firstRecord := nha044Fixture(t, "first", host)
	second, secondBinding, secondRecord := nha044Fixture(t, "second", host)
	if err := fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{firstRecord, secondRecord}); err != nil {
		t.Fatal(err)
	}
	program, err := template.Compile("{{ states('switch.first') }}")
	if err != nil {
		t.Fatal(err)
	}
	id, err := ParseProgramID("current-relay")
	if err != nil {
		t.Fatal(err)
	}
	definition, err := NewDefinition(id, program, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlan([]Definition{definition}, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	clock := &nha044Clock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, err := Start(ctx, fixture.Sequencer(), plan, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())
	initial := nha044Update(t, service.Updates())
	result, ok := initial.DisplayResult()
	if !ok || result.Text() != "off" || initial.Diagnostic() != DiagnosticHealthy {
		t.Fatalf("initial output: %q %v %v", result.Text(), ok, initial.Diagnostic())
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	at := clock.Now().Add(time.Second)
	on := domain.NewSwitchValue(true)
	unrelated, err := integrations.NewStateObservation("relay", quality, &on, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Publish(context.Background(), secondBinding, unrelated); err != nil {
		t.Fatal(err)
	}
	select {
	case update := <-service.Updates():
		t.Fatalf("unrelated state rendered: %v", update.Diagnostic())
	case <-time.After(40 * time.Millisecond):
	}
	if err := first.Publish(context.Background(), firstBinding, unrelated); err != nil {
		t.Fatal(err)
	}
	changed := nha044Update(t, service.Updates())
	result, ok = changed.DisplayResult()
	if !ok || result.Text() != "on" {
		t.Fatalf("related state missed: %q %v", result.Text(), ok)
	}
	if err := fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{secondRecord}); err != nil {
		t.Fatal(err)
	}
	removed := nha044Update(t, service.Updates())
	result, ok = removed.DisplayResult()
	if !ok || result.Text() != "unknown" {
		t.Fatalf("removed alias retained: %q %v", result.Text(), ok)
	}
	if clock.count.Load() != 0 {
		t.Fatalf("timer armed without executed now(): %d", clock.count.Load())
	}
}

func TestNHA044ClimateAttributeZeroAndUnavailableDisplayIsolation(t *testing.T) {
	device, _ := domain.ParseDeviceID("climate-fixture-device")
	entity, _ := domain.ParseEntityID("climate-fixture-entity")
	instance, _ := domain.ParseIntegrationInstanceID("climate-fixture")
	key, _ := integrations.NewSourceKey("fixture", "climate")
	alias, _ := domain.ParseHAAlias("climate.fixture")
	record, err := integrations.NewRegistryRecord(device, entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	zero, err := domain.NewCelsiusTemperature(0)
	if err != nil {
		t.Fatal(err)
	}
	fanOff := false
	climate, err := domain.NewClimateValue(domain.ClimateValueInput{CurrentTemperature: &zero, FanActive: &fanOff})
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.NewClimateStateValue(climate)
	if err != nil {
		t.Fatal(err)
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	stamp, _ := domain.NewSourceStamp("synthetic-session", nil)
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	report, err := domain.NewStateReport(quality, &value, &at, at, stamp, 1)
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := domain.ApplyStateReport(entity, nil, report)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := sequencer.Snapshot{RegistryReady: true, Registry: map[domain.EntityID]integrations.RegistryRecord{entity: record}, States: map[domain.EntityID]domain.ObservedState{entity: state}}
	view, err := ReadViewFromSnapshot(snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	in, err := template.NewInputsFromReadView(view, nil, at, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	program, err := template.Compile("{{ state_attr('climate.fixture','current_temperature') }}")
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := template.EvaluateTracked(context.Background(), program, in, template.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	result, ok := tracked.Result()
	if !ok || result.Text() != "0.0" || len(tracked.Reads()) != 1 || !tracked.Reads()[0].Present() {
		t.Fatalf("zero Celsius was lost: %q %+v", result.Text(), tracked.Reads())
	}
	if native, ok := result.Native().Float(); !ok || native != 0 {
		t.Fatalf("zero native: %v %v", native, ok)
	}
	unavailable, _ := domain.NewQuality(domain.AvailabilityUnavailable, domain.FreshnessStale, domain.ProvenanceObserved)
	later := at.Add(time.Second)
	report, err = domain.NewStateReport(unavailable, nil, nil, later, stamp, 2)
	if err != nil {
		t.Fatal(err)
	}
	state, _, err = domain.ApplyStateReport(entity, &state, report)
	if err != nil {
		t.Fatal(err)
	}
	if _, display := state.DisplayValue(); !display {
		t.Fatal("fixture lacks old display value")
	}
	snapshot.States[entity] = state
	view, err = ReadViewFromSnapshot(snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	in, err = template.NewInputsFromReadView(view, nil, later, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	program, err = template.Compile("{{ states('climate.fixture') }}")
	if err != nil {
		t.Fatal(err)
	}
	tracked, err = template.EvaluateTracked(context.Background(), program, in, template.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	result, ok = tracked.Result()
	if !ok || result.Text() != "unavailable" || tracked.Reads()[0].Present() {
		t.Fatalf("display value leaked as current: %q %+v", result.Text(), tracked.Reads())
	}
}

type nha044SequenceClock struct{ at time.Time }

func (c nha044SequenceClock) Now() time.Time         { return c.at }
func (c nha044SequenceClock) MonoNow() time.Duration { return 0 }

func nha044WaitForTimer(t *testing.T, clock *nha044Clock, count int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for clock.count.Load() < count {
		if time.Now().After(deadline) {
			t.Fatalf("timer %d was never armed", count)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestNHA044MinuteUTCAndDSTNoReplay(t *testing.T) {
	start := time.Date(2026, 11, 1, 5, 59, 30, 0, time.UTC)
	sequence, err := sequencer.New(nha044SequenceClock{at: start}, 128)
	if err != nil {
		t.Fatal(err)
	}
	if err := sequence.PublishRegistry(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	zone, err := time.LoadLocation("America/Toronto")
	if err != nil {
		t.Fatal(err)
	}
	program, err := template.Compile("{{ now() }}")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := ParseProgramID("clock")
	definition, err := NewDefinition(id, program, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlan([]Definition{definition}, nil, zone)
	if err != nil {
		t.Fatal(err)
	}
	clock := &nha044Clock{now: start}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, err := Start(ctx, sequence, plan, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())
	first := nha044Update(t, service.Updates())
	firstResult, _ := first.DisplayResult()
	if firstResult.Text() != "2026-11-01 01:59:30-04:00" {
		t.Fatalf("pre-DST output %q", firstResult.Text())
	}
	nha044WaitForTimer(t, clock, 1)
	clock.Advance(time.Date(2026, 11, 1, 6, 0, 5, 0, time.UTC))
	second := nha044Update(t, service.Updates())
	secondResult, _ := second.DisplayResult()
	if secondResult.Text() != "2026-11-01 01:00:05-05:00" {
		t.Fatalf("DST fallback output %q", secondResult.Text())
	}
	nha044WaitForTimer(t, clock, 2)
	clock.Advance(time.Date(2026, 11, 1, 5, 59, 0, 0, time.UTC))
	select {
	case update := <-service.Updates():
		t.Fatalf("backward clock replayed minute: %v", update.Diagnostic())
	case <-time.After(20 * time.Millisecond):
	}
	clock.Advance(time.Date(2026, 11, 1, 6, 5, 5, 0, time.UTC))
	late := nha044Update(t, service.Updates())
	lateResult, _ := late.DisplayResult()
	if lateResult.Text() != "2026-11-01 01:05:05-05:00" {
		t.Fatalf("late wake output %q", lateResult.Text())
	}
	select {
	case update := <-service.Updates():
		t.Fatalf("late wake replayed skipped minutes: %v", update.Diagnostic())
	case <-time.After(20 * time.Millisecond):
	}
}

func TestNHA044ProjectionLossDegradesBeforeRecapture(t *testing.T) {
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	sequence, err := sequencer.New(nha044SequenceClock{at: at}, 128)
	if err != nil {
		t.Fatal(err)
	}
	if err := sequence.PublishRegistry(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	program, err := template.Compile("{{ 'stable' }}")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := ParseProgramID("constant")
	definition, err := NewDefinition(id, program, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlan([]Definition{definition}, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	clock := &nha044Clock{now: at}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, err := Start(ctx, sequence, plan, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())
	initial := nha044Update(t, service.Updates())
	if initial.Diagnostic() != DiagnosticHealthy {
		t.Fatal("initial result degraded")
	}
	sequence.InvalidateRegistry()
	lost := nha044Update(t, service.Updates())
	if lost.Diagnostic() != DiagnosticLoss {
		t.Fatalf("loss not visible before recapture: %v", lost.Diagnostic())
	}
	_, freshness, _, err := lost.Quality().Components()
	if err != nil || freshness != domain.FreshnessStale {
		t.Fatalf("loss retained fresh quality: %v %v", freshness, err)
	}
	nha044WaitForTimer(t, clock, 1)
	if err := sequence.PublishRegistry(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	clock.Advance(at.Add(time.Second))
	recovered := nha044Update(t, service.Updates())
	if recovered.Diagnostic() != DiagnosticHealthy {
		t.Fatalf("recapture did not recover: %v", recovered.Diagnostic())
	}
}

func nha044CalculatedSnapshot(t *testing.T, names []string) (sequencer.Snapshot, map[string]domain.EntityID) {
	t.Helper()
	registry := make(map[domain.EntityID]integrations.RegistryRecord, len(names))
	ids := make(map[string]domain.EntityID, len(names))
	instance, _ := domain.ParseIntegrationInstanceID("reactive-synthetic")
	for _, name := range names {
		device, _ := domain.ParseDeviceID("device-" + name)
		entity, _ := domain.ParseEntityID("entity-" + name)
		key, _ := integrations.NewSourceKey("fixture", name)
		alias, err := domain.ParseHAAlias("sensor." + name)
		if err != nil {
			t.Fatal(err)
		}
		record, err := integrations.NewRegistryRecord(device, entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
		if err != nil {
			t.Fatal(err)
		}
		registry[entity] = record
		ids[name] = entity
	}
	return sequencer.Snapshot{RegistryReady: true, Registry: registry, States: make(map[domain.EntityID]domain.ObservedState)}, ids
}

func nha044Definition(t *testing.T, idName, source string, target *domain.EntityID) Definition {
	t.Helper()
	program, err := template.Compile(source)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ParseProgramID(idName)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := NewDefinition(id, program, nil, target)
	if err != nil {
		t.Fatal(err)
	}
	return definition
}

func nha044Engine(t *testing.T, snapshot sequencer.Snapshot, definitions []Definition) *engine {
	t.Helper()
	plan, err := NewPlan(definitions, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	return &engine{
		service:  &Service{plan: plan, clock: &nha044Clock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}, ctx: context.Background()},
		snapshot: snapshot, outputs: make(map[ProgramID]Update), readers: make(map[ProgramID][]template.Dependency),
	}
}

type nha044AdvancingClock struct {
	base  time.Time
	calls atomic.Int32
}

func (c *nha044AdvancingClock) Now() time.Time {
	return c.base.Add(time.Duration(c.calls.Add(1)-1) * time.Second)
}
func (c *nha044AdvancingClock) NewTimer(d time.Duration) Timer {
	return &nha044Timer{channel: make(chan time.Time, 1), due: c.base.Add(d)}
}

func TestNHA044CyclicPointFixUsesOneClockInstant(t *testing.T) {
	snapshot, ids := nha044CalculatedSnapshot(t, []string{"a", "b"})
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	value := domain.NewSwitchValue(false)
	stamp, _ := domain.NewSourceStamp("synthetic-cycle", nil)
	for _, name := range []string{"a", "b"} {
		report, err := domain.NewStateReport(quality, &value, &at, at, stamp, 1)
		if err != nil {
			t.Fatal(err)
		}
		observed, _, err := domain.ApplyStateReport(ids[name], nil, report)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.States[ids[name]] = observed
	}
	definitions := []Definition{
		nha044Definition(t, "a", "{{ is_state('sensor.b', 'never') or now() }}", ptrNHA044Entity(ids["a"])),
		nha044Definition(t, "b", "{{ is_state('sensor.a', 'never') or now() }}", ptrNHA044Entity(ids["b"])),
	}
	graph, err := buildDependencyGraph(definitions, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.order) != 1 || !graph.order[0].cyclic || len(graph.order[0].members) != 2 {
		t.Fatalf("fixture did not form one SCC: %+v", graph.order)
	}
	e := nha044Engine(t, snapshot, definitions)
	clock := &nha044AdvancingClock{base: at}
	e.service.clock = clock
	if _, err := e.recompute(); err != nil {
		t.Fatal(err)
	}
	if calls := clock.calls.Load(); calls != 1 {
		t.Fatalf("one fixed point sampled clock %d times", calls)
	}
	for _, name := range []string{"a", "b"} {
		id, _ := ParseProgramID(name)
		update := e.outputs[id]
		result, ok := update.CurrentResult()
		if !ok || update.Diagnostic() != DiagnosticHealthy || result.Text() != "2026-10-18 12:00:00+00:00" {
			t.Fatalf("%s failed stable same-instant SCC: %q %v %v", name, result.Text(), ok, update.Diagnostic())
		}
	}
}

func TestNHA044GraphDepth32AndIndependent(t *testing.T) {
	names := make([]string, 34)
	for i := 0; i < 33; i++ {
		names[i] = fmt.Sprintf("chain_%02d", i)
	}
	names[33] = "independent"
	snapshot, ids := nha044CalculatedSnapshot(t, names)
	definitions := make([]Definition, 0, len(names))
	for i := 0; i < 33; i++ {
		source := "{{ 'seed' }}"
		if i > 0 {
			source = fmt.Sprintf("{{ states('sensor.chain_%02d') }}", i-1)
		}
		definitions = append(definitions, nha044Definition(t, names[i], source, ptrNHA044Entity(ids[names[i]])))
	}
	definitions = append(definitions, nha044Definition(t, "independent", "{{ 'okay' }}", ptrNHA044Entity(ids["independent"])))
	e := nha044Engine(t, snapshot, definitions)
	graph, err := buildDependencyGraph(definitions, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	var deepest int
	for _, component := range graph.order {
		if component.members[0] == 32 {
			deepest = component.depth
		}
	}
	if deepest != 33 {
		t.Fatalf("chain depth: %d", deepest)
	}
	if _, err := e.recompute(); err != nil {
		t.Fatal(err)
	}
	chainID, _ := ParseProgramID("chain_32")
	independentID, _ := ParseProgramID("independent")
	if got := e.outputs[chainID].Diagnostic(); got != DiagnosticWorkBudget {
		t.Fatalf("depth 33 diagnostic: %v", got)
	}
	if got := e.outputs[independentID].Diagnostic(); got != DiagnosticHealthy {
		t.Fatalf("independent diagnostic: %v", got)
	}
}

func ptrNHA044Entity(value domain.EntityID) *domain.EntityID { return &value }

func TestNHA044OscillationAndIndependentWorkBudget(t *testing.T) {
	snapshot, ids := nha044CalculatedSnapshot(t, []string{"a", "b"})
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	value := domain.NewSwitchValue(false)
	stamp, _ := domain.NewSourceStamp("synthetic-cycle", nil)
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{"a", "b"} {
		report, err := domain.NewStateReport(quality, &value, &at, at, stamp, 1)
		if err != nil {
			t.Fatal(err)
		}
		observed, _, err := domain.ApplyStateReport(ids[name], nil, report)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.States[ids[name]] = observed
	}
	definitions := []Definition{
		nha044Definition(t, "a", "{{ is_state('sensor.b', 'True') }}", ptrNHA044Entity(ids["a"])),
		nha044Definition(t, "b", "{{ is_state('sensor.a', 'False') }}", ptrNHA044Entity(ids["b"])),
		nha044Definition(t, "independent", "{{ 'okay' }}", nil),
	}
	e := nha044Engine(t, snapshot, definitions)
	if _, err := e.recompute(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		id, _ := ParseProgramID(name)
		if got := e.outputs[id].Diagnostic(); got != DiagnosticOscillation {
			t.Fatalf("%s: %v", name, got)
		}
		if _, valid := e.outputs[id].DisplayResult(); valid {
			t.Fatalf("%s fabricated a stable result", name)
		}
	}
	id, _ := ParseProgramID("independent")
	if got := e.outputs[id].Diagnostic(); got != DiagnosticHealthy {
		t.Fatalf("independent: %v", got)
	}

	// 64 independent programs force a small fair share. The long native
	// string exceeds its own share but must not starve the final program.
	definitions = []Definition{nha044Definition(t, "expensive", "{{ '"+strings.Repeat("x", 2048)+"' }}", nil)}
	for i := 1; i < MaxPrograms; i++ {
		definitions = append(definitions, nha044Definition(t, fmt.Sprintf("simple_%02d", i), "{{ 'okay' }}", nil))
	}
	e = nha044Engine(t, sequencer.Snapshot{RegistryReady: true, Registry: map[domain.EntityID]integrations.RegistryRecord{}, States: map[domain.EntityID]domain.ObservedState{}}, definitions)
	if _, err := e.recompute(); err != nil {
		t.Fatal(err)
	}
	costlyID, _ := ParseProgramID("expensive")
	finalID, _ := ParseProgramID("simple_63")
	if got := e.outputs[costlyID].Diagnostic(); got != DiagnosticWorkBudget {
		t.Fatalf("expensive: %v", got)
	}
	if got := e.outputs[finalID].Diagnostic(); got != DiagnosticHealthy {
		t.Fatalf("independent after expensive: %v", got)
	}
}

func TestNHA044ExecutedReadsReplaceStaticCycle(t *testing.T) {
	snapshot, ids := nha044CalculatedSnapshot(t, []string{"self"})
	definition := nha044Definition(t, "self", "{{ 'on' or states('sensor.self') }}", ptrNHA044Entity(ids["self"]))
	e := nha044Engine(t, snapshot, []Definition{definition})
	if _, err := e.recompute(); err != nil {
		t.Fatal(err)
	}
	id, _ := ParseProgramID("self")
	if got := e.outputs[id].Diagnostic(); got != DiagnosticHealthy {
		t.Fatalf("short circuit retained false cycle: %v", got)
	}
	if len(e.readers[id]) != 0 {
		t.Fatalf("skipped read remained active: %+v", e.readers[id])
	}
}

func nha044Groups(t *testing.T, member string) []bundle.Group {
	t.Helper()
	dir := t.TempDir()
	text := "input_boolean:\n  first:\n    initial: false\n  second:\n    initial: false\ngroup:\n  pick:\n    entities:\n      - input_boolean." + member + "\n"
	if err := os.WriteFile(filepath.Join(dir, "configuration.yaml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := bundle.Compile(dir, "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return candidate.Groups()
}

func TestNHA044GroupGenerationReplacesMembershipWithoutInventedState(t *testing.T) {
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	sequence, err := sequencer.New(nha044SequenceClock{at: at}, 128)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("group-synthetic")
	records := make([]integrations.RegistryRecord, 0, 2)
	for _, name := range []string{"first", "second"} {
		device, _ := domain.ParseDeviceID("device-" + name)
		entity, _ := domain.ParseEntityID("entity-" + name)
		key, _ := integrations.NewSourceKey("helper", name)
		alias, _ := domain.ParseHAAlias("input_boolean." + name)
		record, err := integrations.NewRegistryRecord(device, entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if err := sequence.PublishRegistry(context.Background(), records); err != nil {
		t.Fatal(err)
	}
	program, err := template.Compile("{{ expand('group.pick') | map(attribute='entity_id') | list }}")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := ParseProgramID("group-members")
	definition, err := NewDefinition(id, program, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlan([]Definition{definition}, nha044Groups(t, "first"), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, err := Start(ctx, sequence, plan, &nha044Clock{now: at})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())
	first := nha044Update(t, service.Updates())
	firstResult, ok := first.DisplayResult()
	if !ok || !strings.Contains(firstResult.Text(), "input_boolean.first") || strings.Contains(firstResult.Text(), "input_boolean.second") {
		t.Fatalf("initial group projection: %q", firstResult.Text())
	}
	if service.GroupGeneration() != 1 {
		t.Fatalf("initial group generation %d", service.GroupGeneration())
	}
	if err := service.ReplaceGroups(context.Background(), nha044Groups(t, "second")); err != nil {
		t.Fatal(err)
	}
	second := nha044Update(t, service.Updates())
	secondResult, ok := second.DisplayResult()
	if !ok || !strings.Contains(secondResult.Text(), "input_boolean.second") || strings.Contains(secondResult.Text(), "input_boolean.first") {
		t.Fatalf("replacement group projection: %q", secondResult.Text())
	}
	if service.GroupGeneration() != 2 {
		t.Fatalf("replacement group generation %d", service.GroupGeneration())
	}
}

func TestNHA044DisplayResultIsNotCurrentAfterLossOrOscillation(t *testing.T) {
	definition := nha044Definition(t, "display", "{{ 7 }}", nil)
	e := nha044Engine(t, sequencer.Snapshot{RegistryReady: true, Registry: map[domain.EntityID]integrations.RegistryRecord{}, States: map[domain.EntityID]domain.ObservedState{}}, []Definition{definition})
	if _, err := e.recompute(); err != nil {
		t.Fatal(err)
	}
	healthy := e.outputs[definition.id]
	if result, ok := healthy.CurrentResult(); !ok || result.Text() != "7" {
		t.Fatalf("healthy calculated value absent: %q %v", result.Text(), ok)
	}
	stale := healthy
	stale.quality = staleQuality(healthy.quality)
	stale.diagnostic = DiagnosticLoss
	if result, ok := stale.DisplayResult(); !ok || result.Text() != "7" {
		t.Fatalf("display value lost: %q %v", result.Text(), ok)
	}
	if _, ok := stale.CurrentResult(); ok {
		t.Fatal("stale display result admitted as current")
	}
	oscillating := stale
	oscillating.diagnostic = DiagnosticOscillation
	if _, ok := oscillating.CurrentResult(); ok {
		t.Fatal("oscillating display result admitted as current")
	}
	unknown, _ := domain.NewQuality(domain.AvailabilityUnknown, domain.FreshnessFresh, domain.ProvenanceEstimated)
	unknownValue := healthy
	unknownValue.quality = unknown
	if _, ok := unknownValue.CurrentResult(); ok {
		t.Fatal("unknown result admitted as current")
	}
	noValue := healthy
	noValue.valid = false
	if _, ok := noValue.DisplayResult(); ok {
		t.Fatal("absent result admitted as display")
	}
	if _, ok := noValue.CurrentResult(); ok {
		t.Fatal("absent result admitted as current")
	}
}

func TestNHA044MinuteUTCSpringDSTSkipsLocalHour(t *testing.T) {
	start := time.Date(2026, 3, 8, 6, 59, 30, 0, time.UTC)
	sequence, err := sequencer.New(nha044SequenceClock{at: start}, 128)
	if err != nil {
		t.Fatal(err)
	}
	if err := sequence.PublishRegistry(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	zone, err := time.LoadLocation("America/Toronto")
	if err != nil {
		t.Fatal(err)
	}
	definition := nha044Definition(t, "spring", "{{ now() }}", nil)
	plan, err := NewPlan([]Definition{definition}, nil, zone)
	if err != nil {
		t.Fatal(err)
	}
	clock := &nha044Clock{now: start}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, err := Start(ctx, sequence, plan, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())
	first, ok := nha044Update(t, service.Updates()).CurrentResult()
	if !ok || first.Text() != "2026-03-08 01:59:30-05:00" {
		t.Fatalf("spring before: %q %v", first.Text(), ok)
	}
	nha044WaitForTimer(t, clock, 1)
	clock.Advance(time.Date(2026, 3, 8, 7, 0, 5, 0, time.UTC))
	second, ok := nha044Update(t, service.Updates()).CurrentResult()
	if !ok || second.Text() != "2026-03-08 03:00:05-04:00" {
		t.Fatalf("spring after: %q %v", second.Text(), ok)
	}
}

type nha044BarrierClock struct {
	at      time.Time
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (c *nha044BarrierClock) Now() time.Time {
	c.once.Do(func() { close(c.entered); <-c.release })
	return c.at
}
func (c *nha044BarrierClock) NewTimer(d time.Duration) Timer {
	return &nha044Timer{channel: make(chan time.Time, 1), due: c.at.Add(d)}
}

func TestNHA044CaptureRenderSubscribeBarrierKeepsSourceSuffix(t *testing.T) {
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	session, binding, record := nha044Fixture(t, "barrier", host)
	if err := fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	definition := nha044Definition(t, "barrier", "{{ states('switch.barrier') }}", nil)
	plan, err := NewPlan([]Definition{definition}, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	clock := &nha044BarrierClock{at: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC), entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan *Service, 1)
	failure := make(chan error, 1)
	go func() {
		service, err := Start(ctx, fixture.Sequencer(), plan, clock)
		if err != nil {
			failure <- err
			return
		}
		result <- service
	}()
	select {
	case <-clock.entered:
	case err := <-failure:
		t.Fatalf("start failed before captured render: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("captured render did not reach barrier")
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	at := clock.at.Add(time.Second)
	on := domain.NewSwitchValue(true)
	observation, err := integrations.NewStateObservation("relay", quality, &on, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Publish(context.Background(), binding, observation); err != nil {
		t.Fatal(err)
	}
	close(clock.release)
	var service *Service
	select {
	case service = <-result:
	case err := <-failure:
		t.Fatalf("barrier rebase failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("barrier rebase stalled")
	}
	defer service.Close(context.Background())
	update := nha044Update(t, service.Updates())
	current, ok := update.CurrentResult()
	if !ok || current.Text() != "on" {
		t.Fatalf("suffix was lost after captured off: %q %v", current.Text(), ok)
	}
	select {
	case stale := <-service.Updates():
		t.Fatalf("captured off escaped after suffix: %v", stale.Diagnostic())
	case <-time.After(25 * time.Millisecond):
	}
}

type nha044SecondCallClock struct {
	at      time.Time
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (c *nha044SecondCallClock) Now() time.Time {
	if c.calls.Add(1) == 2 {
		close(c.entered)
		<-c.release
	}
	return c.at
}
func (c *nha044SecondCallClock) NewTimer(d time.Duration) Timer {
	return &nha044Timer{channel: make(chan time.Time, 1), due: c.at.Add(d)}
}

func TestNHA044PendingRenderDrainsPastUnrelatedEntry(t *testing.T) {
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	first, firstBinding, firstRecord := nha044Fixture(t, "first", host)
	second, secondBinding, secondRecord := nha044Fixture(t, "second", host)
	if err := fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{firstRecord, secondRecord}); err != nil {
		t.Fatal(err)
	}
	definition := nha044Definition(t, "drain", "{{ states('switch.first') }}", nil)
	plan, err := NewPlan([]Definition{definition}, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	clock := &nha044SecondCallClock{at: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC), entered: make(chan struct{}), release: make(chan struct{})}
	released := false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, err := Start(ctx, fixture.Sequencer(), plan, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())
	defer func() {
		if !released {
			close(clock.release)
		}
	}()
	initial := nha044Update(t, service.Updates())
	if result, ok := initial.CurrentResult(); !ok || result.Text() != "off" {
		t.Fatalf("initial state: %q %v", result.Text(), ok)
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	at := clock.at.Add(time.Second)
	on := domain.NewSwitchValue(true)
	observation, err := integrations.NewStateObservation("relay", quality, &on, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Publish(context.Background(), firstBinding, observation); err != nil {
		t.Fatal(err)
	}
	select {
	case <-clock.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("pertinent render did not enter barrier")
	}
	// Publish is synchronous: B is in the view queue before A may finish.
	// B has no active reader and must not erase A's deferred recompute intent.
	if err := second.Publish(context.Background(), secondBinding, observation); err != nil {
		t.Fatal(err)
	}
	close(clock.release)
	released = true
	updated := nha044Update(t, service.Updates())
	if result, ok := updated.CurrentResult(); !ok || result.Text() != "on" {
		t.Fatalf("A was lost after unrelated B drained: %q %v diagnostic=%v", result.Text(), ok, updated.Diagnostic())
	}
}

func TestNHA044ViewOverflowCutsContinuityBeforeRecapture(t *testing.T) {
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	session, binding, record := nha044Fixture(t, "overflow", host)
	if err := fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	definition := nha044Definition(t, "overflow", "{{ states('switch.overflow') }}", nil)
	plan, err := NewPlan([]Definition{definition}, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	clock := &nha044SecondCallClock{at: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC), entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, err := Start(ctx, fixture.Sequencer(), plan, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())
	initial := nha044Update(t, service.Updates())
	if result, ok := initial.CurrentResult(); !ok || result.Text() != "off" {
		t.Fatalf("initial source: %q %v", result.Text(), ok)
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	publish := func(i int) {
		at := clock.at.Add(time.Duration(i+1) * time.Second)
		value := domain.NewSwitchValue(i%2 == 0)
		observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Publish(context.Background(), binding, observation); err != nil {
			t.Fatal(err)
		}
	}
	publish(0)
	select {
	case <-clock.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("live render did not pause")
	}
	for i := 1; i <= 257; i++ {
		publish(i)
	}
	close(clock.release)
	lost := nha044Update(t, service.Updates())
	if lost.Diagnostic() != DiagnosticLoss {
		t.Fatalf("overflow admitted stale render: %v", lost.Diagnostic())
	}
	if _, ok := lost.CurrentResult(); ok {
		t.Fatal("overflow result stayed current")
	}
	recovered := nha044Update(t, service.Updates())
	if recovered.Diagnostic() != DiagnosticHealthy {
		t.Fatalf("overflow did not recapture: %v", recovered.Diagnostic())
	}
	current, ok := recovered.CurrentResult()
	if !ok || current.Text() != "off" {
		t.Fatalf("recovered value not latest: %q %v", current.Text(), ok)
	}
}

func TestNHA044ReplayOver256RefusesCapturedStaleResult(t *testing.T) {
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	session, binding, record := nha044Fixture(t, "replay_limit", host)
	if err := fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	definition := nha044Definition(t, "replay-limit", "{{ states('switch.replay_limit') }}", nil)
	plan, err := NewPlan([]Definition{definition}, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	clock := &nha044BarrierClock{at: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC), entered: make(chan struct{}), release: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		service, err := Start(context.Background(), fixture.Sequencer(), plan, clock)
		if service != nil {
			_ = service.Close(context.Background())
		}
		result <- err
	}()
	select {
	case <-clock.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("captured render did not pause")
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	for i := 0; i <= 256; i++ {
		at := clock.at.Add(time.Duration(i+1) * time.Second)
		value := domain.NewSwitchValue(i%2 == 0)
		observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Publish(context.Background(), binding, observation); err != nil {
			t.Fatal(err)
		}
	}
	close(clock.release)
	select {
	case err := <-result:
		if !errors.Is(err, sequencer.ErrCursorExpired) {
			t.Fatalf("oversized replay was silently admitted: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("oversized replay did not stop")
	}
}
