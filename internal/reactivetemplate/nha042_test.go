package reactivetemplate

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/compat/ha/template"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/helpers"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/scope"
)

func nha042Ready(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("observer did not complete")
	}
}

func TestNHA042WaitOnlyServiceSharesProjectionAndDisarmsOneObserver(t *testing.T) {
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	session, binding, record := nha044Fixture(t, "first", host)
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlan(nil, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = plan.WithScriptWaits(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	clock := &nha044Clock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	service, err := Start(ctx, fixture.Sequencer(), plan, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(ctx)
	program, err := template.Compile("{{ is_state('switch.first', 'on') }}")
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.ObserveTemplate(ctx, program, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.ObserveTemplate(ctx, program, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-second.Ready():
		t.Fatal("initial false completed wait")
	default:
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	nha042Ready(t, first.Done())
	// Closing one observer does not remove another observer's shared reader.
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	at := clock.Now().Add(time.Second)
	on := domain.NewSwitchValue(true)
	observation, err := integrations.NewStateObservation("relay", quality, &on, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Publish(ctx, binding, observation); err != nil {
		t.Fatal(err)
	}
	nha042Ready(t, second.Ready())
	if err := second.Outcome(); err != nil {
		t.Fatal(err)
	}
	tracked, err := service.EvaluateCondition(ctx, program, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, present := tracked.Result()
	if !present || result.Text() != "True" {
		t.Fatalf("condition did not use current shared projection: %q", result.Text())
	}
	select {
	case <-service.Updates():
		t.Fatal("ephemeral wait escaped into permanent output reader")
	default:
	}
	if err := second.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(ctx); err != nil {
		t.Fatal(err)
	}
	nha042Ready(t, second.Done())
	// Mutating transient definitions must never alter the caller-owned plan.
	if len(plan.definitions) != 0 || !plan.waitsEnabled {
		t.Fatal("service mutated original plan")
	}
}

type nha042Timer struct {
	channel chan time.Time
	stops   atomic.Int64
}

func (t *nha042Timer) C() <-chan time.Time { return t.channel }
func (t *nha042Timer) Stop() bool          { return t.stops.Add(1) == 1 }

type nha042Clock struct {
	mu     sync.Mutex
	at     time.Time
	timers []*nha042Timer
}

func (c *nha042Clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *nha042Clock) NewTimer(time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &nha042Timer{channel: make(chan time.Time, 1)}
	c.timers = append(c.timers, timer)
	return timer
}
func TestNHA042RemovingLastMinuteWaitStopsActualTimerOnce(t *testing.T) {
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	_, _, record := nha044Fixture(t, "minute", host)
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlan(nil, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = plan.WithScriptWaits(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	clock := &nha042Clock{at: time.Date(2026, 10, 18, 12, 30, 0, 0, time.UTC)}
	service, err := Start(ctx, fixture.Sequencer(), plan, clock)
	if err != nil {
		t.Fatal(err)
	}
	program, err := template.Compile("{{ now().minute }}")
	if err != nil {
		t.Fatal(err)
	}
	handle, err := service.ObserveTemplate(ctx, program, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Serialize a request after registration so the loop has armed its minute.
	literal, _ := template.Compile("{{ true }}")
	if _, err := service.EvaluateCondition(ctx, literal, nil); err != nil {
		t.Fatal(err)
	}
	clock.mu.Lock()
	count := len(clock.timers)
	var timer *nha042Timer
	if count != 0 {
		timer = clock.timers[0]
	}
	clock.mu.Unlock()
	if count != 1 {
		t.Fatalf("expected one shared minute timer, got %d", count)
	}
	if err := handle.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(ctx); err != nil {
		t.Fatal(err)
	}
	nha042Ready(t, handle.Done())
	if timer.stops.Load() != 1 {
		t.Fatalf("minute Stop calls: %d", timer.stops.Load())
	}
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if timer.stops.Load() != 1 {
		t.Fatalf("shutdown stopped removed minute twice: %d", timer.stops.Load())
	}
}

func TestNHA042ZeroHandlesAndClosedServiceRefuseAdmission(t *testing.T) {
	var handle *WaitHandle
	if handle.Ready() != nil || handle.Done() != nil || handle.Outcome() != ErrInvalidPlan {
		t.Fatal("zero observer admitted")
	}
	if handle.Close(context.Background()) != ErrInvalidPlan {
		t.Fatal("zero observer closed as valid")
	}
	_, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.PublishRegistry(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	plan, _ := NewPlan(nil, nil, time.UTC)
	plan, _ = plan.WithScriptWaits(time.UTC)
	service, err := Start(context.Background(), fixture.Sequencer(), plan, RealClock{})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	program, _ := template.Compile("{{ true }}")
	if handle, err := service.ObserveTemplate(context.Background(), program, nil); handle != nil || !errors.Is(err, ErrClosed) {
		t.Fatalf("closed service admitted: %v %v", handle, err)
	}
	if _, err := service.EvaluateCondition(context.Background(), program, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed evaluation admitted: %v", err)
	}
}

type nha042GateClock struct {
	armed            atomic.Bool
	at               time.Time
	entered, release chan struct{}
}

func (c *nha042GateClock) Now() time.Time {
	if c.armed.Swap(false) {
		close(c.entered)
		<-c.release
	}
	return c.at
}
func (c *nha042GateClock) NewTimer(d time.Duration) Timer {
	return &nha044Timer{channel: make(chan time.Time, 1), due: c.at.Add(d)}
}
func nha042EmptyWaitService(t *testing.T, clock Clock) *Service {
	t.Helper()
	_, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.PublishRegistry(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	plan, _ := NewPlan(nil, nil, time.UTC)
	plan, _ = plan.WithScriptWaits(time.UTC)
	service, err := Start(context.Background(), fixture.Sequencer(), plan, clock)
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
func TestNHA042TemplateControlOwnsBindingsBeforeQueue(t *testing.T) {
	clock := &nha042GateClock{at: time.Now(), entered: make(chan struct{}), release: make(chan struct{})}
	service := nha042EmptyWaitService(t, clock)
	var once sync.Once
	release := func() { once.Do(func() { close(clock.release) }) }
	defer release()
	clock.armed.Store(true)
	literal, _ := template.Compile("{{ true }}")
	blocker := make(chan error, 1)
	go func() { _, err := service.EvaluateCondition(context.Background(), literal, nil); blocker <- err }()
	nha042Ready(t, clock.entered)
	program, _ := template.Compile("{{ enabled }}")
	bindings := []template.Binding{{Name: "enabled", Value: template.Bool(true)}}
	admitted := make(chan *WaitHandle, 1)
	failed := make(chan error, 1)
	go func() {
		handle, err := service.ObserveTemplate(context.Background(), program, bindings)
		if err != nil {
			failed <- err
			return
		}
		admitted <- handle
	}()
	deadline := time.Now().Add(time.Second)
	for len(service.controls) == 0 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	// control sends while holding mu. Acquiring it after the queued send
	// establishes the copy-before-mutation happens-before edge under -race.
	service.mu.Lock()
	queued := len(service.controls)
	if queued != 1 {
		service.mu.Unlock()
		t.Fatal("request was not admitted behind actual engine operation")
	}
	bindings[0].Value = template.Bool(false)
	service.mu.Unlock()
	release()
	if err := <-blocker; err != nil {
		t.Fatal(err)
	}
	var handle *WaitHandle
	select {
	case handle = <-admitted:
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("observer admission did not join")
	}
	nha042Ready(t, handle.Ready())
	if err := handle.Outcome(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	slots := service.controlSlots
	service.mu.Unlock()
	if slots != 0 {
		t.Fatalf("request/observer reservation leaked: %d", slots)
	}
}
func TestNHA042TemplateRegistrationDrainsPendingSuffix(t *testing.T) {
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	session, binding, record := nha044Fixture(t, "suffix", host)
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	plan, _ := NewPlan(nil, nil, time.UTC)
	plan, _ = plan.WithScriptWaits(time.UTC)
	clock := &nha042GateClock{at: time.Now(), entered: make(chan struct{}), release: make(chan struct{})}
	service, err := Start(ctx, fixture.Sequencer(), plan, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(ctx)
	var once sync.Once
	release := func() { once.Do(func() { close(clock.release) }) }
	defer release()
	clock.armed.Store(true)
	program, _ := template.Compile("{{ is_state('switch.suffix', 'on') }}")
	admitted := make(chan *WaitHandle, 1)
	failed := make(chan error, 1)
	go func() {
		handle, err := service.ObserveTemplate(ctx, program, nil)
		if err != nil {
			failed <- err
			return
		}
		admitted <- handle
	}()
	nha042Ready(t, clock.entered)
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	on, at := domain.NewSwitchValue(true), clock.at
	observation, err := integrations.NewStateObservation("relay", quality, &on, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Publish(ctx, binding, observation); err != nil {
		t.Fatal(err)
	}
	release()
	var handle *WaitHandle
	select {
	case handle = <-admitted:
	case err := <-failed:
		t.Fatalf("suffix escaped as caller retry: %v", err)
	case <-time.After(time.Second):
		t.Fatal("registration failed to converge")
	}
	nha042Ready(t, handle.Ready())
	if err := handle.Outcome(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestNHA042TemplateRenderErrorKeepsDependenciesUntilRecovery(t *testing.T) {
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	session, binding, record := nha044Fixture(t, "recover", host)
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	plan, _ := NewPlan(nil, nil, time.UTC)
	plan, _ = plan.WithScriptWaits(time.UTC)
	service, err := Start(ctx, fixture.Sequencer(), plan, RealClock{})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(ctx)
	program, err := template.Compile("{{ is_state('switch.recover', 'on') or unavailable_name }}")
	if err != nil {
		t.Fatal(err)
	}
	handle, err := service.ObserveTemplate(ctx, program, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-handle.Ready():
		t.Fatal("recoverable render error terminated wait")
	default:
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	on, at := domain.NewSwitchValue(true), time.Now()
	observation, _ := integrations.NewStateObservation("relay", quality, &on, &at, at, nil)
	if err := session.Publish(ctx, binding, observation); err != nil {
		t.Fatal(err)
	}
	nha042Ready(t, handle.Ready())
	if err := handle.Outcome(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestNHA042TemplateWaitsShareProgramWorkAndMinuteBudgets(t *testing.T) {
	service := nha042EmptyWaitService(t, RealClock{})
	program, _ := template.Compile("{{ false }}")
	handles := make([]*WaitHandle, 0, MaxPrograms)
	for index := 0; index < MaxPrograms; index++ {
		handle, err := service.ObserveTemplate(context.Background(), program, nil)
		if err != nil {
			t.Fatalf("admission %d: %v", index, err)
		}
		handles = append(handles, handle)
	}
	if handle, err := service.ObserveTemplate(context.Background(), program, nil); handle != nil || !errors.Is(err, ErrWorkBudget) {
		t.Fatalf("64-program bound exceeded: %v %v", handle, err)
	}
	if _, err := service.EvaluateCondition(context.Background(), program, nil); !errors.Is(err, ErrWorkBudget) {
		t.Fatalf("condition escaped same reservation bound: %v", err)
	}
	// Removal must remain admitted even while all program slots are occupied.
	for _, handle := range handles {
		if err := handle.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		nha042Ready(t, handle.Done())
	}
	service.mu.Lock()
	slots := service.controlSlots
	service.mu.Unlock()
	if slots != 0 {
		t.Fatalf("observer capacity leaked: %d", slots)
	}
	if _, err := service.EvaluateCondition(context.Background(), program, nil); err != nil {
		t.Fatal(err)
	}
}

func TestNHA042TemplateWaitLossNeverUsesDisplayValue(t *testing.T) {
	service := nha042EmptyWaitService(t, RealClock{})
	program, _ := template.Compile("{{ false }}")
	handle, err := service.ObserveTemplate(context.Background(), program, nil)
	if err != nil {
		t.Fatal(err)
	}
	literal, _ := template.Compile("{{ true }}")
	inputs, _ := template.NewInputs(nil, nil, time.Now(), time.UTC)
	rendered, err := template.Evaluate(context.Background(), literal, inputs, template.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessStale, domain.ProvenanceEstimated)
	update := Update{id: handle.id, result: rendered, valid: true, quality: quality, diagnostic: DiagnosticHealthy}
	if !service.routeWaitUpdate(update) {
		t.Fatal("observer update escaped to permanent output")
	}
	select {
	case <-handle.Ready():
		t.Fatal("stale display true completed wait")
	default:
	}
	update.diagnostic = DiagnosticLoss
	if !service.routeWaitUpdate(update) {
		t.Fatal("loss did not route to observer")
	}
	nha042Ready(t, handle.Ready())
	if !errors.Is(handle.Outcome(), ErrInvalidProjection) {
		t.Fatalf("loss fabricated success: %v", handle.Outcome())
	}
	if err := handle.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNHA042ConditionsAndWaitsReadNativeHelpers097(t *testing.T) {
	ctx := context.Background()
	file, err := parser.NewSourceFile("helpers.yaml", []byte(`input_boolean:
  nha042_boolean: {initial: false}
input_number:
  nha042_number: {min: -10, max: 10, step: 1, initial: 0}
counter:
  nha042_counter: {minimum: 0, maximum: 10, step: 1, initial: 0}
input_datetime:
  nha042_date: {has_date: true, has_time: false, initial: '2026-10-19'}
  nha042_time: {has_date: false, has_time: true, initial: '00:00:00'}
  nha042_datetime: {has_date: true, has_time: true, initial: '2026-10-19 00:00:00'}
input_select:
  nha042_select: {options: [idle, ready], initial: idle}
input_text:
  nha042_text: {min: 0, max: 20, initial: ''}
`))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := bundle.CompileSourcesV2("helpers.yaml", []parser.SourceFile{file})
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := helpers.Definitions(compiled)
	if err != nil || len(definitions) != 8 {
		t.Fatalf("native helper definitions: %v count%d", err, len(definitions))
	}
	admit := func(value domain.HelperValue, err error) domain.HelperValue {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	date0, err := domain.NewCivilDate(2026, 10, 19)
	if err != nil {
		t.Fatal(err)
	}
	date1, err := domain.NewCivilDate(2026, 10, 20)
	if err != nil {
		t.Fatal(err)
	}
	time0, err := domain.NewCivilTime(0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	time1, err := domain.NewCivilTime(1, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	datetime0, err := domain.NewCivilDateTime(date0, time0)
	if err != nil {
		t.Fatal(err)
	}
	datetime1, err := domain.NewCivilDateTime(date1, time1)
	if err != nil {
		t.Fatal(err)
	}
	type helperCase struct {
		alias, initialText, finalText string
		initial, final                domain.HelperValue
	}
	cases := []helperCase{
		{"input_boolean.nha042_boolean", "off", "on", domain.NewHelperBoolean(false), domain.NewHelperBoolean(true)},
		{"input_number.nha042_number", "0", "1", admit(domain.NewHelperNumber(0)), admit(domain.NewHelperNumber(1))},
		{"counter.nha042_counter", "0", "1", domain.NewHelperCounter(0), domain.NewHelperCounter(1)},
		{"input_datetime.nha042_date", "2026-10-19", "2026-10-20", admit(domain.NewHelperDate(date0)), admit(domain.NewHelperDate(date1))},
		{"input_datetime.nha042_time", "00:00:00", "01:02:03", admit(domain.NewHelperTime(time0)), admit(domain.NewHelperTime(time1))},
		{"input_datetime.nha042_datetime", "2026-10-19 00:00:00", "2026-10-20 01:02:03", admit(domain.NewHelperDateTime(datetime0)), admit(domain.NewHelperDateTime(datetime1))},
		{"input_select.nha042_select", "idle", "ready", admit(domain.NewHelperSelect("idle")), admit(domain.NewHelperSelect("ready"))},
		{"input_text.nha042_text", "", "ready", admit(domain.NewHelperText("")), admit(domain.NewHelperText("ready"))},
	}
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha042-native-helpers")
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := scope.RetainedHelperButtonPolicy()
	if err != nil {
		t.Fatal(err)
	}
	bindings := make(map[string]integrations.Binding, len(definitions))
	records := make([]integrations.RegistryRecord, 0, len(definitions))
	for _, definition := range definitions {
		alias, err := definition.Alias()
		if err != nil {
			t.Fatal(err)
		}
		name, err := alias.Value()
		if err != nil {
			t.Fatal(err)
		}
		key, err := helpers.ConfiguredSourceKey(alias)
		if err != nil {
			t.Fatal(err)
		}
		description, err := helpers.Description(definition, policy)
		if err != nil {
			t.Fatal(err)
		}
		candidate, err := integrations.NewCandidate(key, []*registrywire.CapabilityDescription{description})
		if err != nil {
			t.Fatal(err)
		}
		found, err := session.Discover(ctx, []integrations.Candidate{candidate})
		if err != nil || len(found) != 1 {
			t.Fatalf("native helper discovery: %v count%d", err, len(found))
		}
		bindings[name] = found[0]
		device, entity, err := found[0].IDs()
		if err != nil {
			t.Fatal(err)
		}
		record, err := integrations.NewRegistryRecord(device, entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if err := fixture.PublishRegistry(ctx, records); err != nil {
		t.Fatal(err)
	}
	publish := func(t *testing.T, test helperCase, value domain.HelperValue, revision uint64) {
		t.Helper()
		state, err := domain.NewHelperStateValue(value, revision)
		if err != nil {
			t.Fatal(err)
		}
		quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		if err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC()
		observation, err := integrations.NewStateObservation("value", quality, &state, &at, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Publish(ctx, bindings[test.alias], observation); err != nil {
			t.Fatal(err)
		}
		state = domain.NewSwitchValue(false) // Reuse cannot mutate the admitted native value.
	}
	for _, test := range cases {
		publish(t, test, test.initial, 1)
	}
	plan, err := NewPlan(nil, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = plan.WithScriptWaits(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	service, err := Start(ctx, fixture.Sequencer(), plan, RealClock{})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(ctx)
	for _, test := range cases {
		t.Run(test.alias, func(t *testing.T) {
			initial, err := template.Compile("{{ is_state('" + test.alias + "', '" + test.initialText + "') }}")
			if err != nil {
				t.Fatal(err)
			}
			tracked, err := service.EvaluateCondition(ctx, initial, nil)
			if err != nil {
				t.Fatal(err)
			}
			result, present := tracked.Result()
			if !present || result.Text() != "True" {
				t.Fatalf("native zero/false/empty value disappeared: %q", result.Text())
			}
			program, err := template.Compile("{{ is_state('" + test.alias + "', '" + test.finalText + "') }}")
			if err != nil {
				t.Fatal(err)
			}
			tracked, err = service.EvaluateCondition(ctx, program, nil)
			if err != nil {
				t.Fatal(err)
			}
			result, present = tracked.Result()
			if !present || result.Text() != "False" {
				t.Fatalf("initial native helper condition: %q", result.Text())
			}
			handle, err := service.ObserveTemplate(ctx, program, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := handle.Close(ctx); err != nil {
					t.Error(err)
				}
			})
			select {
			case <-handle.Ready():
				t.Fatal("initial false helper completed wait")
			default:
			}
			publish(t, test, test.final, 2)
			nha042Ready(t, handle.Ready())
			if err := handle.Outcome(); err != nil {
				t.Fatal(err)
			}
			tracked, err = service.EvaluateCondition(ctx, program, nil)
			if err != nil {
				t.Fatal(err)
			}
			result, present = tracked.Result()
			if !present || result.Text() != "True" {
				t.Fatalf("native helper update did not reach shared condition: %q", result.Text())
			}
			if err := handle.Close(ctx); err != nil {
				t.Fatal(err)
			}
			nha042Ready(t, handle.Done())
		})
	}
	service.mu.Lock()
	remaining := len(service.waitHandles) + service.controlSlots
	service.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("native helper waits leaked slots: %d", remaining)
	}
}

func TestNHA042TransientRenderConsumesReal044WorkShare(t *testing.T) {
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	session, binding, record := nha044Fixture(t, "nha042_work", host)
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	publish := func(on bool) {
		t.Helper()
		quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		if err != nil {
			t.Fatal(err)
		}
		value, at := domain.NewSwitchValue(on), time.Now()
		observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Publish(ctx, binding, observation); err != nil {
			t.Fatal(err)
		}
	}
	publish(false)
	plan, err := NewPlan(nil, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = plan.WithScriptWaits(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	service, err := Start(ctx, fixture.Sequencer(), plan, RealClock{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	program, err := template.Compile("{{ '" + strings.Repeat("x", 2048) + "' }}")
	if err != nil {
		t.Fatal(err)
	}
	// Public registration fixes this consumer's original index first. No
	// admitted plan or private engine order is changed by the test.
	handle, err := service.ObserveTemplate(ctx, program, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := handle.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	tracked, err := service.EvaluateCondition(ctx, program, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, present := tracked.Result()
	if !present || result.Text() != strings.Repeat("x", 2048) || result.WorkUnits() < 4096 {
		t.Fatal("individually valid costly template did not render its admitted result")
	}
	select {
	case <-handle.Ready():
		t.Fatal("a healthy non-true render completed the first wait")
	default:
	}
	cheap, err := template.Compile("{{ is_state('switch.nha042_work', 'on') }}")
	if err != nil {
		t.Fatal(err)
	}
	independent := make([]*WaitHandle, 0, MaxPrograms-1)
	t.Cleanup(func() {
		for _, waiter := range independent {
			if err := waiter.Close(ctx); err != nil {
				t.Error(err)
			}
		}
	})
	for index := 0; index < MaxPrograms-1; index++ {
		waiter, err := service.ObserveTemplate(ctx, cheap, nil)
		if err != nil {
			t.Fatal(err)
		}
		independent = append(independent, waiter)
	}
	// At64 actual consumers, the first component's fair share is at most
	// (262144 -64*128)/64 =3968, before graph/snapshot work. Its admitted
	// result alone costs at least4096; no counter or limit is overwritten.
	nha042Ready(t, handle.Ready())
	if err := handle.Outcome(); !errors.Is(err, ErrWorkBudget) {
		t.Fatalf("expensive actual wait did not exhaust its shared044 render allowance: %v", err)
	}
	if _, completed := handle.CompletedAt(); completed {
		t.Fatal("work refusal manufactured a successful completion tick")
	}
	for _, waiter := range independent {
		select {
		case <-waiter.Ready():
			t.Fatal("independent false wait was completed or starved by the costly consumer")
		default:
		}
	}
	if err := handle.Close(ctx); err != nil {
		t.Fatal(err)
	}
	nha042Ready(t, handle.Done())
	literal, err := template.Compile("{{ true }}")
	if err != nil {
		t.Fatal(err)
	}
	tracked, err = service.EvaluateCondition(ctx, literal, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, present = tracked.Result()
	if !present || result.Text() != "True" {
		t.Fatalf("work failure poisoned subsequent shared condition: %q", result.Text())
	}
	// A real Host publication must still wake all63 independent consumers
	// through the one reader after the expensive consumer has been removed.
	publish(true)
	for _, waiter := range independent {
		nha042Ready(t, waiter.Ready())
		if err := waiter.Outcome(); err != nil {
			t.Fatalf("costly transient render starved an independent wait: %v", err)
		}
		if err := waiter.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if err := waiter.Close(ctx); err != nil {
			t.Fatal(err)
		}
		nha042Ready(t, waiter.Done())
	}
	select {
	case update := <-service.Updates():
		t.Fatalf("ephemeral results escaped to the permanent queue: %+v", update)
	default:
	}
	service.mu.Lock()
	slots := service.controlSlots + len(service.waitHandles)
	service.mu.Unlock()
	if slots != 0 {
		t.Fatalf("failed actual render or independent wait leaked transient ownership: %d", slots)
	}
}
