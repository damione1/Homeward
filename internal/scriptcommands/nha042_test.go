package scriptcommands

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	statewire "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/logicselection"
	"github.com/damione1/Homeward/internal/scenes"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
)

func TestNHA042CommandUncertaintyPreservesTypedCause(t *testing.T) {
	refusal := errors.New("synthetic route refusal")
	for _, test := range []struct {
		name     string
		response *actions.CommandResponse
		cause    error
		expected ErrorKind
	}{
		{"durable commit unknown", nil, fmt.Errorf("wrapped: %w", integrations.ErrDurableCommitUnknown), Indeterminate},
		{"transport outcome unknown", nil, fmt.Errorf("wrapped: %w", integrations.ErrCommandOutcomeIndeterminate), Indeterminate},
		{"response overrides generic error", &actions.CommandResponse{Status: actions.ActionStatus_ACTION_STATUS_INDETERMINATE}, refusal, Indeterminate},
		{"refusal", nil, refusal, Refused},
		{"absent response", nil, nil, Indeterminate},
		{"future status", &actions.CommandResponse{Status: actions.ActionStatus(12345)}, nil, Indeterminate},
		{"unspecified status", &actions.CommandResponse{}, nil, Indeterminate},
		{"failed", &actions.CommandResponse{Status: actions.ActionStatus_ACTION_STATUS_FAILED}, nil, Refused},
		{"cancelled", &actions.CommandResponse{Status: actions.ActionStatus_ACTION_STATUS_CANCELLED}, nil, Refused},
		{"accepted", &actions.CommandResponse{Status: actions.ActionStatus_ACTION_STATUS_ACCEPTED}, nil, ErrorInvalid},
		{"dispatched", &actions.CommandResponse{Status: actions.ActionStatus_ACTION_STATUS_DISPATCHED}, nil, ErrorInvalid},
		{"acknowledged", &actions.CommandResponse{Status: actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED}, nil, ErrorInvalid},
		{"confirmed", &actions.CommandResponse{Status: actions.ActionStatus_ACTION_STATUS_CONFIRMED}, nil, ErrorInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := commandResult(test.response, test.cause)
			if test.expected == ErrorInvalid {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var typed *Error
			if !errors.As(err, &typed) || typed.Kind() != test.expected {
				t.Fatalf("classification %v, want %v", err, test.expected)
			}
			if test.cause != nil && !errors.Is(err, test.cause) {
				t.Fatal("primary cause lost")
			}
			if _, present := typed.SceneResult(); present {
				t.Fatal("command fabricated scene result")
			}
		})
	}
	var zero *Error
	if zero.Kind() != ErrorInvalid || zero.Unwrap() != nil {
		t.Fatal("nil error fabricated a domain result")
	}
	if _, present := zero.SceneResult(); present {
		t.Fatal("nil error fabricated scene result")
	}
}

type nha042Generation struct{}

func (*nha042Generation) WithCurrent(ctx context.Context, visit func(uint64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return visit(7)
}

type nha042Clock struct{}

func (*nha042Clock) Now() time.Time         { return time.Now() }
func (*nha042Clock) MonoNow() time.Duration { return 0 }
func (*nha042Clock) NewTimer(d time.Duration) automationtriggers.Timer {
	return automationtriggers.RealClock{}.NewTimer(d)
}
func TestNHA042CommandPortsRejectAbsentTypedNilAndZeroExecution(t *testing.T) {
	var generation *nha042Generation
	var clock *nha042Clock
	if !missing(generation) || !missing(clock) {
		t.Fatal("typed nil admitted")
	}
	if _, err := New(Input{}); err == nil {
		t.Fatal("zero ports admitted")
	}
	var adapter *Adapter
	if err := adapter.ExecuteCall(context.Background(), ir.Call{}, automationruns.CommandExecution{}); err == nil {
		t.Fatal("nil adapter admitted")
	}
	adapter = &Adapter{}
	if err := adapter.ExecuteCall(context.Background(), ir.Call{}, automationruns.CommandExecution{}); err == nil {
		t.Fatal("zero execution fabricated command authority")
	}
}

type nha042Authority struct{ actor domain.ActorID }

func (a nha042Authority) Context(ctx context.Context) (context.Context, domain.ActorID, error) {
	return ctx, a.actor, nil
}

type nha042Conditions struct{}

func (nha042Conditions) EvaluateCondition(context.Context, template.Program, []template.Binding) (template.TrackedResult, error) {
	return template.TrackedResult{}, errors.New("unexpected condition in command-only script")
}

type nha042Driver struct {
	mu               sync.Mutex
	store            *storage.Store
	ids              []domain.CommandID
	release, entered chan struct{}
	once             sync.Once
	cause            error
}

func (d *nha042Driver) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	id, _, _, err := command.Identity()
	if err != nil {
		return err
	}
	// The real 024 ledger must commit dispatching before this fake effect.
	record, present, err := d.store.CommandIntent(ctx, id)
	if err != nil || !present || record.State() != storage.IntentDispatching {
		return fmt.Errorf("dispatch preceded durable fence: %v %v %v", record.State(), present, err)
	}
	d.mu.Lock()
	d.ids = append(d.ids, id)
	d.mu.Unlock()
	d.once.Do(func() { close(d.entered) })
	select {
	case <-d.release:
		return d.cause
	case <-ctx.Done():
		return ctx.Err()
	}
}
func TestNHA042AdapterUsesShared024And046WithoutRetry(t *testing.T) {
	cluster := testpg.Start(t)
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha042-command-fixture")
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	fixtureDriver, err := fake.NewActionDriver(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := fixtureDriver.Start(ctx, session); err != nil {
		t.Fatal(err)
	}
	binding, err := fixtureDriver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	device, entity, err := binding.IDs()
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := domain.ParseHAAlias("switch.nha042_command")
	key, _ := integrations.NewSourceKey("fixture-controller", "relay")
	registry, err := integrations.NewRegistryRecord(device, entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{registry}); err != nil {
		t.Fatal(err)
	}
	driver := &nha042Driver{store: store, release: make(chan struct{}), entered: make(chan struct{})}
	var unblock sync.Once
	release := func() { unblock.Do(func() { close(driver.release) }) }
	// Registered before the controller cleanup, so failed tests release the
	// driver before joining the controller/024; no actor or store is orphaned.
	defer release()
	if err := host.AttachCommandDriver(session, driver); err != nil {
		t.Fatal(err)
	}
	generation := &nha042Generation{}
	commands, err := integrations.NewDurableCommandService(ctx, host, generation, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := commands.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	adapter, err := New(Input{Commands: commands, Host: host, Projection: fixture.Sequencer(), Generation: generation, Clock: &nha042Clock{}, CommandDeadline: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	entityIR, _ := ir.NewEntity("switch.nha042_command", ir.ReferencePresent, ir.SupportSwitchTurnOn)
	catalogue, _ := ir.NewCatalogue([]ir.Entity{entityIR}, nil, nil)
	id, _ := ir.ParseScriptID("script.nha042_commands")
	catalogue, _ = catalogue.WithScripts([]ir.ScriptID{id})
	source, _ := parser.NewSourceFile("commands.yaml", []byte("sequence:\n- repeat:\n    count: 2\n    sequence:\n    - action: switch.turn_on\n      target: {entity_id: switch.nha042_command}\n      data: {}\n"))
	compiled, err := ir.CompileScriptSources("script.nha042_commands", "commands.yaml", []parser.SourceFile{source}, catalogue)
	if err != nil || len(compiled.Accepted()) != 1 {
		t.Fatalf("script: %v %+v", err, compiled.Rejected())
	}
	plan, err := automationruns.NewPlanWithScripts(nil, compiled.Accepted())
	if err != nil {
		t.Fatal(err)
	}
	actor, _ := domain.ParseActorID("nha042-explicit-caller")
	interpreter, err := automationruns.NewInterpreter(automationruns.InterpreterInput{Plan: plan, Conditions: nha042Conditions{}, Authority: nha042Authority{actor}, Commands: adapter, Clock: automationtriggers.RealClock{}, Zone: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := automationruns.NewController(plan, nil, interpreter, interpreter)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := controller.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	handle, err := controller.InvokeScript(ctx, id, nil, automationruns.Run{}, ir.ScriptCallDetached)
	if err != nil {
		t.Fatal(err)
	}
	startupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	startup, err := handle.Startup(startupCtx)
	if err != nil || startup.Kind() != automationruns.StartupSuspended {
		t.Fatalf("real adapter did not declare asynchronous boundary: %v %v", startup.Kind(), err)
	}
	select {
	case <-driver.entered:
	case <-startupCtx.Done():
		t.Fatal("driver not reached")
	}
	select {
	case <-handle.Done():
		t.Fatal("command completed before fake driver release")
	default:
	}
	release()
	if err := handle.Result(startupCtx); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	ids := append([]domain.CommandID(nil), driver.ids...)
	driver.mu.Unlock()
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("occurrence identity or retry changed: %+v", ids)
	}
	for _, id := range ids {
		record, present, err := store.CommandIntent(ctx, id)
		if err != nil || !present || record.State() != storage.IntentResult {
			t.Fatalf("terminal ledger result lost: %v %v %v", record.State(), present, err)
		}
	}
	selection, err := logicselection.LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	sceneCatalogue, err := scenes.CompileSelected(selection, scope.LogicMVPSourcesV1())
	if err != nil {
		t.Fatal(err)
	}
	sceneService, err := scenes.NewService(sceneCatalogue, fixture.Sequencer(), generation, commands, &nha042Clock{})
	if err != nil {
		t.Fatal(err)
	}
	defer sceneService.Close()
	explicit, err := scenes.NewExplicitSwitch(alias, true)
	if err != nil {
		t.Fatal(err)
	}
	sceneID, _ := domain.ParseHAAlias("scene.nha042_partial")
	missingAlias, _ := domain.ParseHAAlias("switch.nha042_missing")
	ref, omitted, err := sceneService.CreateTemporary(ctx, sceneID, []scenes.ExplicitMember{explicit}, []domain.HAAlias{missingAlias}, time.Hour, nil)
	if err != nil || len(omitted) != 1 {
		t.Fatalf("046 partial scene: %v %+v", err, omitted)
	}
	invocation, _ := domain.ParseCommandID("nha042-partial-scene-occurrence")
	slot, err := scenes.NewPreparationSlot(sceneService, ref, actor, invocation, nil, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := slot.PrepareOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := slot.PrepareOnce(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := sceneService.ApplyPrepared(ctx, prepared)
	if err != nil || result.Aggregate() != scenes.AggregatePartial {
		t.Fatalf("real 046 result: %v %v", result.Aggregate(), err)
	}
	var partial *Error
	if err := sceneResult(result); !errors.As(err, &partial) || partial.Kind() != Partial {
		t.Fatalf("046 partial flattened: %v", err)
	}
	retained, present := partial.SceneResult()
	if !present || retained.Aggregate() != scenes.AggregatePartial || len(retained.Members()) != 2 {
		t.Fatal("046 member diagnostics lost")
	}
	copied := retained.Members()
	copied[0] = scenes.MemberResult{}
	if retained.Members()[0].Outcome() == scenes.OutcomeInvalid {
		t.Fatal("scene result exposed mutable members")
	}
	if _, err := sceneService.ApplyPrepared(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	sceneSends := len(driver.ids)
	driver.mu.Unlock()
	if sceneSends != 3 {
		t.Fatalf("046 repeated prepared result retried effect: %d", sceneSends)
	}
	driver.cause = errors.New("synthetic transport outcome unknown")
	uncertain, err := controller.InvokeScript(ctx, id, nil, automationruns.Run{}, ir.ScriptCallDirect)
	if err != nil {
		t.Fatal(err)
	}
	err = uncertain.Result(startupCtx)
	var executionError *automationruns.ExecutionError
	var commandError *Error
	if !errors.As(err, &executionError) || executionError.Kind() != automationruns.ExecutionCommandError ||
		!errors.As(err, &commandError) || commandError.Kind() != Indeterminate || !errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) {
		t.Fatalf("024 uncertainty lost through interpreter/adapter: %v", err)
	}
	driver.mu.Lock()
	attempted := append([]domain.CommandID(nil), driver.ids...)
	driver.mu.Unlock()
	if len(attempted) != 4 {
		t.Fatalf("uncertain occurrence was retried or loop continued: %d", len(attempted))
	}
	record, present, err := store.CommandIntent(ctx, attempted[3])
	if err != nil || !present || record.State() != storage.IntentIndeterminate {
		t.Fatalf("uncertain ledger result lost: %v %v %v", record.State(), present, err)
	}
}

type nha042MatrixGeneration struct {
	mu        sync.Mutex
	calls     int
	change    func() error
	changeErr error
	changed   bool
	revision  uint64
}

func (g *nha042MatrixGeneration) WithCurrent(ctx context.Context, visit func(uint64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	// The first visit captures the actual route/state. The second visit is
	// the existing024 final route fence, before a Driver can be admitted.
	if g.calls == 2 && g.change != nil {
		g.changed = true
		g.changeErr = g.change()
		if g.changeErr != nil {
			return g.changeErr
		}
	}
	return visit(g.revision)
}

type nha042MatrixDriver struct {
	mu    sync.Mutex
	store *storage.Store
	calls []integrations.AdmittedCommand
}

func (d *nha042MatrixDriver) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	id, _, _, err := command.Identity()
	if err != nil {
		return err
	}
	record, present, err := d.store.CommandIntent(ctx, id)
	if err != nil || !present || record.State() != storage.IntentDispatching {
		return fmt.Errorf("synthetic effect preceded024 dispatching commit: %v %v %v", record.State(), present, err)
	}
	d.mu.Lock()
	d.calls = append(d.calls, command)
	d.mu.Unlock()
	return nil
}
func (d *nha042MatrixDriver) snapshot() []integrations.AdmittedCommand {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]integrations.AdmittedCommand(nil), d.calls...)
}

type nha042MatrixBinding struct {
	binding   integrations.Binding
	candidate integrations.Candidate
	entity    domain.EntityID
}
type nha042MatrixRig struct {
	host       *integrations.Manager
	fixture    *fake.Fixture
	session    *integrations.Session
	generation *nha042MatrixGeneration
	driver     *nha042MatrixDriver
	adapter    *Adapter
	bindings   map[string]nha042MatrixBinding
	encoding   integrations.LightEncoding
}

func nha042MatrixSetup(t *testing.T, completeScene bool) *nha042MatrixRig {
	t.Helper()
	ctx := context.Background()
	cluster := testpg.Start(t)
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha042-command-matrix")
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	encoding, err := integrations.NewMQTTLightEncoding(254, sha256.Sum256([]byte("nha042-synthetic-scale254")), false)
	if err != nil {
		t.Fatal(err)
	}
	rig := &nha042MatrixRig{host: host, fixture: fixture, session: session, generation: &nha042MatrixGeneration{revision: 7}, driver: &nha042MatrixDriver{store: store}, bindings: make(map[string]nha042MatrixBinding), encoding: encoding}
	aliases := []string{"light.nha042_relative", "climate.nha042_thermostat", "light.kitchen_lights", "light.kitchen_main_lights"}
	if completeScene {
		aliases = append(aliases, "light.kitchen_sink_light")
	}
	var records []integrations.RegistryRecord
	for index, text := range aliases {
		key, err := integrations.NewSourceKey(fmt.Sprintf("nha042-matrix-%d", index), "state")
		if err != nil {
			t.Fatal(err)
		}
		identities, err := fixture.Resolve(ctx, instance, []integrations.SourceKey{key})
		if err != nil || len(identities) != 1 {
			t.Fatalf("identity: %v %d", err, len(identities))
		}
		alias, err := domain.ParseHAAlias(text)
		if err != nil {
			t.Fatal(err)
		}
		record, err := integrations.NewRegistryRecord(identities[0].Device, identities[0].Entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
		var description *registrywire.CapabilityDescription
		if text == "climate.nha042_thermostat" {
			step := 0.5
			description = &registrywire.CapabilityDescription{SchemaId: contracts.ClimateSchema, Major: 1, Endpoint: "state", DisplayName: "Synthetic script thermostat", Constraints: &registrywire.CapabilityDescription_Climate{Climate: &registrywire.ClimateConstraints{SetpointCelsius: &registrywire.NumberBounds{Minimum: 5, Maximum: 30, Step: &step}, ObservedTemperatureCelsius: &registrywire.ClimateTemperatureObservation{Bounds: &registrywire.NumberBounds{Minimum: -20, Maximum: 50}}, ObservedSetpointCelsius: &registrywire.ClimateTemperatureObservation{Bounds: &registrywire.NumberBounds{Minimum: 5, Maximum: 30, Step: &step}}, ObservedModeOptions: []registrywire.ClimateMode{registrywire.ClimateMode_CLIMATE_MODE_HEAT}, ActivityOptions: []registrywire.ClimateActivity{registrywire.ClimateActivity_CLIMATE_ACTIVITY_IDLE}}}}
		} else {
			description = &registrywire.CapabilityDescription{SchemaId: contracts.LightSchema, Major: 1, Endpoint: "state", DisplayName: "Synthetic script light", Constraints: &registrywire.CapabilityDescription_Light{Light: &registrywire.LightConstraints{SetOn: true, BrightnessPercent: &registrywire.NumberBounds{Minimum: 0, Maximum: 100}}}}
		}
		candidate, err := integrations.NewCandidate(key, []*registrywire.CapabilityDescription{description})
		if err != nil {
			t.Fatal(err)
		}
		if text != "climate.nha042_thermostat" {
			candidate, err = candidate.WithLightEncoding(encoding)
			if err != nil {
				t.Fatal(err)
			}
		}
		bindings, err := session.Discover(ctx, []integrations.Candidate{candidate})
		if err != nil || len(bindings) != 1 {
			t.Fatalf("capability: %v %d", err, len(bindings))
		}
		rig.bindings[text] = nha042MatrixBinding{bindings[0], candidate, identities[0].Entity}
	}
	if err := fixture.PublishRegistry(ctx, records); err != nil {
		t.Fatal(err)
	}
	if err := host.AttachCommandDriver(session, rig.driver); err != nil {
		t.Fatal(err)
	}
	for text := range rig.bindings {
		var value domain.StateValue
		if text == "climate.nha042_thermostat" {
			current, setpoint := 0.0, 21.0
			mode, activity := registrywire.ClimateMode_CLIMATE_MODE_HEAT, registrywire.ClimateActivity_CLIMATE_ACTIVITY_IDLE
			climate, err := contracts.AdmitWireClimateValue(&statewire.ClimateValue{CurrentTemperatureCelsius: &current, Setpoint: &statewire.ClimateValue_SetpointCelsius{SetpointCelsius: setpoint}, Mode: &mode, Activity: &activity})
			if err != nil {
				t.Fatal(err)
			}
			value, err = domain.NewClimateStateValue(climate)
			if err != nil {
				t.Fatal(err)
			}
		} else {
			level := 35.0
			light, err := domain.NewLightValue(true, &level, nil)
			if err != nil {
				t.Fatal(err)
			}
			value, err = domain.NewLightStateValue(light)
			if err != nil {
				t.Fatal(err)
			}
		}
		quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		if err != nil {
			t.Fatal(err)
		}
		if err := rig.publish(text, quality, &value); err != nil {
			t.Fatal(err)
		}
	}
	commands, err := integrations.NewDurableCommandService(ctx, host, rig.generation, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		if err := commands.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	selection, err := logicselection.LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := scenes.CompileSelected(selection, scope.LogicMVPSourcesV1())
	if err != nil {
		t.Fatal(err)
	}
	sceneService, err := scenes.NewService(catalogue, fixture.Sequencer(), rig.generation, commands, &nha042Clock{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sceneService.Close)
	rig.adapter, err = New(Input{Commands: commands, Scenes: sceneService, Host: host, Projection: fixture.Sequencer(), Generation: rig.generation, Clock: &nha042Clock{}, CommandDeadline: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return rig
}
func (r *nha042MatrixRig) publish(alias string, quality domain.Quality, value *domain.StateValue) error {
	at := time.Now()
	report, err := integrations.NewStateObservation("state", quality, value, &at, at, nil)
	if err != nil {
		return err
	}
	return r.session.Publish(context.Background(), r.bindings[alias].binding, report)
}
func (r *nha042MatrixRig) run(t *testing.T, body string) error {
	t.Helper()
	light, err := ir.NewEntity("light.nha042_relative", ir.ReferencePresent, ir.SupportLightTurnOn, ir.SupportLightBrightnessRelative)
	if err != nil {
		t.Fatal(err)
	}
	climate, err := ir.NewEntity("climate.nha042_thermostat", ir.ReferencePresent, ir.SupportClimateSetpoint)
	if err != nil {
		t.Fatal(err)
	}
	scene, err := ir.NewEntity("scene.kitchen_cosy_mode", ir.ReferencePresent, ir.SupportSceneTurnOn)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue([]ir.Entity{light, climate, scene}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ir.ParseScriptID("script.nha042_matrix")
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err = catalogue.WithScripts([]ir.ScriptID{id})
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.NewSourceFile("matrix.yaml", []byte("sequence:\n"+body))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := ir.CompileScriptSources("script.nha042_matrix", "matrix.yaml", []parser.SourceFile{file}, catalogue)
	if err != nil || len(compiled.Accepted()) != 1 {
		t.Fatalf("script matrix: %v %+v", err, compiled.Rejected())
	}
	plan, err := automationruns.NewPlanWithScripts(nil, compiled.Accepted())
	if err != nil {
		t.Fatal(err)
	}
	actor, err := domain.ParseActorID("nha042-explicit-matrix-caller")
	if err != nil {
		t.Fatal(err)
	}
	interpreter, err := automationruns.NewInterpreter(automationruns.InterpreterInput{Plan: plan, Conditions: nha042Conditions{}, Authority: nha042Authority{actor}, Commands: r.adapter, Clock: automationtriggers.RealClock{}, Zone: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := automationruns.NewController(plan, nil, interpreter, interpreter)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		if err := controller.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	handle, err := controller.InvokeScript(context.Background(), id, nil, automationruns.Run{}, ir.ScriptCallDirect)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	return handle.Result(ctx)
}

func TestNHA042ExecuteCallRelativeLightClimateAndDeclarativeScene(t *testing.T) {
	t.Run("relative-two-occurrences", func(t *testing.T) {
		rig := nha042MatrixSetup(t, true)
		if err := rig.run(t, "- repeat:\n    count: 2\n    sequence:\n    - action: light.turn_on\n      target: {entity_id: light.nha042_relative}\n      data: {brightness_step_pct: 15}\n"); err != nil {
			t.Fatal(err)
		}
		calls := rig.driver.snapshot()
		if len(calls) != 2 {
			t.Fatalf("relative occurrence sends: %d", len(calls))
		}
		var previous domain.CommandID
		for _, call := range calls {
			id, _, _, err := call.Identity()
			if err != nil || id == previous {
				t.Fatalf("relative occurrence identity reused: %v %v", id, err)
			}
			previous = id
			request, err := call.Request()
			if err != nil {
				t.Fatal(err)
			}
			light := request.GetArguments().GetSetLight()
			if light == nil || light.On == nil || !*light.On || light.BrightnessPercent == nil || math.Abs(*light.BrightnessPercent-float64(128)*100/255) > 1e-9 || request.ExpectedRevision == nil || *request.ExpectedRevision != 1 {
				t.Fatalf("relative preparation changed source revision/rounding: %v", request)
			}
			q, present, err := call.MQTTLightEmission(rig.encoding)
			if err != nil || !present || q != 127 {
				t.Fatalf("relative intention lost its one encoded target: %d %v %v", q, present, err)
			}
		}
	})
	t.Run("climate-one-coherent-setpoint", func(t *testing.T) {
		rig := nha042MatrixSetup(t, true)
		if err := rig.run(t, "- action: climate.set_temperature\n  target: {entity_id: climate.nha042_thermostat}\n  data: {temperature: 22.5}\n"); err != nil {
			t.Fatal(err)
		}
		calls := rig.driver.snapshot()
		if len(calls) != 1 {
			t.Fatalf("climate sends: %d", len(calls))
		}
		request, err := calls[0].Request()
		if err != nil {
			t.Fatal(err)
		}
		climate := request.GetArguments().GetSetClimate()
		if request.CapabilitySchemaId != contracts.ClimateSchema || request.ExpectedRevision == nil || *request.ExpectedRevision != 1 || climate == nil || climate.SetpointCelsius == nil || *climate.SetpointCelsius != 22.5 || climate.Mode != nil {
			t.Fatalf("climate absence/setpoint/revision lost: %v", request)
		}
	})
	for _, complete := range []bool{true, false} {
		t.Run(fmt.Sprintf("declarative-scene-complete=%t", complete), func(t *testing.T) {
			rig := nha042MatrixSetup(t, complete)
			err := rig.run(t, "- action: scene.turn_on\n  target: {entity_id: scene.kitchen_cosy_mode}\n  data: {}\n")
			want := 3
			if complete {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				want = 2
				var commandError *Error
				if !errors.As(err, &commandError) || commandError.Kind() != Partial {
					t.Fatalf("declarative scene partial result flattened through ExecuteCall: %v", err)
				}
				result, present := commandError.SceneResult()
				if !present || result.Aggregate() != scenes.AggregatePartial || len(result.Members()) != 3 {
					t.Fatal("declarative member diagnostics lost")
				}
				refused := 0
				for _, member := range result.Members() {
					if member.Outcome() == scenes.OutcomeRefused {
						refused++
					}
				}
				if refused != 1 {
					t.Fatalf("missing fake route diagnostics: %d", refused)
				}
			}
			if calls := rig.driver.snapshot(); len(calls) != want {
				t.Fatalf("declarative scene sends: %d want%d", len(calls), want)
			}
		})
	}
}

func TestNHA042ExecuteCallRevalidatesRevokedFencesBeforeDriver(t *testing.T) {
	for _, fence := range []string{"configuration", "source-session", "source-binding", "light-profile", "observed-revision", "climate-revision"} {
		t.Run(fence, func(t *testing.T) {
			rig := nha042MatrixSetup(t, true)
			rig.generation.change = func() error {
				switch fence {
				case "configuration":
					rig.generation.revision = 8
					return nil
				case "source-session":
					instance, _, err := rig.bindings["light.nha042_relative"].binding.Source()
					if err != nil {
						return err
					}
					_, err = rig.host.OpenSession(instance)
					return err
				case "source-binding":
					return rig.session.Withdraw(context.Background(), rig.bindings["light.nha042_relative"].binding)
				case "light-profile":
					changed, err := integrations.NewMQTTLightEncoding(255, sha256.Sum256([]byte("nha042-revoked-scale255")), false)
					if err != nil {
						return err
					}
					candidate, err := rig.bindings["light.nha042_relative"].candidate.WithLightEncoding(changed)
					if err != nil {
						return err
					}
					_, err = rig.session.Discover(context.Background(), []integrations.Candidate{candidate})
					return err
				case "observed-revision", "climate-revision":
					alias := "light.nha042_relative"
					if fence == "climate-revision" {
						alias = "climate.nha042_thermostat"
					}
					observed, present, err := rig.fixture.Sequencer().State(rig.bindings[alias].entity)
					if err != nil || !present {
						return fmt.Errorf("required current source absent: %v %v", present, err)
					}
					value, present := observed.CurrentValue()
					if !present {
						return errors.New("required current value absent")
					}
					quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
					if err != nil {
						return err
					}
					return rig.publish(alias, quality, &value)
				default:
					return errors.New("unhandled closed fence case")
				}
			}
			body := "- action: light.turn_on\n  target: {entity_id: light.nha042_relative}\n  data: {brightness_step_pct: 15}\n"
			if fence == "climate-revision" {
				body = "- action: climate.set_temperature\n  target: {entity_id: climate.nha042_thermostat}\n  data: {temperature: 22.5}\n"
			}
			err := rig.run(t, body)
			var commandError *Error
			if !errors.As(err, &commandError) || commandError.Kind() != Refused {
				t.Fatalf("revoked fence lost typed refusal: %v", err)
			}
			rig.generation.mu.Lock()
			changed, changeErr := rig.generation.changed, rig.generation.changeErr
			rig.generation.mu.Unlock()
			if !changed || changeErr != nil {
				t.Fatalf("test did not mutate the actual captured fence: %v %v", changed, changeErr)
			}
			if calls := rig.driver.snapshot(); len(calls) != 0 {
				t.Fatalf("revoked fence emitted %d physical fake commands", len(calls))
			}
		})
	}
}
