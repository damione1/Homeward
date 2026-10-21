package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/jobs"
	"github.com/damione1/Homeward/internal/logicselection"
	"github.com/damione1/Homeward/internal/mqtt"
	"github.com/damione1/Homeward/internal/reactivetemplate"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/scenes"
	"github.com/damione1/Homeward/internal/solar"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// These tests intentionally exercise Run's concrete composition. The common
// Run wiring and the solar package arrive after NHA-105; this static tranche
// does not introduce a test-only interpreter factory to hide that dependency.
const nha042ServerAlias = "switch.arc_soundbar_crossfade"
const nha042ServerCommand = "- action: switch.turn_on\n  target: {entity_id: " + nha042ServerAlias + "}\n  data: {}\n"

type nha042ServerAuthority struct{ principal authorization.Principal }

func (a *nha042ServerAuthority) Context(ctx context.Context) (context.Context, domain.ActorID, error) {
	if a == nil {
		return nil, domain.ActorID{}, authorization.ErrDenied
	}
	actor, err := a.principal.Actor()
	if err != nil {
		return nil, domain.ActorID{}, err
	}
	admitted, err := authorization.WithPrincipal(ctx, a.principal)
	return admitted, actor, err
}

type nha042ServerEnvironment struct {
	config         config.Config
	account        contracts.AccountID
	principal      authorization.Principal
	observer       *pgx.Conn
	driverObserver *pgx.Conn
}

func nha042ServerEnvironmentFor(t *testing.T) *nha042ServerEnvironment {
	t.Helper()
	cluster := testpg.Start(t)
	configuration := nha030ServerConfig(t, cluster.Port)
	settings, err := configuration.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(context.Background()); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, err := store.BootstrapMFAPolicy(context.Background()); err != nil {
		store.Close()
		t.Fatal(err)
	}
	account := nha034Account(t, store)
	owner, _ := nha034Session(t, store, account, "nha042-server-explicit-caller", time.Now().UTC())
	principal, err := authorization.NativePrincipal(owner.Account, owner.Session)
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	return &nha042ServerEnvironment{config: configuration, account: owner.Account, principal: principal,
		observer: nha030ServerObserver(t, cluster.Port), driverObserver: nha030ServerObserver(t, cluster.Port)}
}

// This is a fake transport effect, not a fake command service. Its independent
// SQL connection verifies that NHA-024 committed before entering the driver.
type nha042ServerDriver struct {
	mu          sync.Mutex
	observer    *pgx.Conn
	ids         []domain.CommandID
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func (d *nha042ServerDriver) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	if !command.ReadyForTransport() {
		return integrations.ErrCommandAuthorizationDenied
	}
	id, _, _, err := command.Identity()
	if err != nil {
		return err
	}
	text, err := id.Value()
	if err != nil {
		return err
	}
	d.mu.Lock()
	queryCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	var state string
	err = d.observer.QueryRow(queryCtx, `SELECT state FROM public.command_intents WHERE command_id=$1`, text).Scan(&state)
	cancel()
	if err != nil || state != "dispatching" {
		d.mu.Unlock()
		return fmt.Errorf("NHA042 effect preceded committed ledger: state=%q: %w", state, err)
	}
	d.ids = append(d.ids, id)
	d.mu.Unlock()
	d.enteredOnce.Do(func() { close(d.entered) })
	// A transport may return after its caller is cancelled. Close must retain
	// the ledger until this call actually leaves; no inverse effect is sent.
	<-d.release
	return nil
}

func (d *nha042ServerDriver) snapshot() []domain.CommandID {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]domain.CommandID(nil), d.ids...)
}
func (d *nha042ServerDriver) unblock() { d.releaseOnce.Do(func() { close(d.release) }) }

type nha042ServerPilot struct {
	mu        sync.Mutex
	ready     chan struct{}
	readyOnce sync.Once
	host      *integrations.Manager
	key       integrations.SourceKey
	driver    *nha042ServerDriver
	session   *integrations.Session
	binding   integrations.Binding
	starts    int
	clock     integrations.CommandClock
}

func (p *nha042ServerPilot) Start(ctx context.Context, source integrations.SessionHost, _ integrations.Tasks) error {
	p.mu.Lock()
	p.starts++
	p.mu.Unlock()
	session, ok := source.(*integrations.Session)
	if !ok {
		return integrations.ErrInvalidInput
	}
	candidate, err := integrations.NewCandidate(p.key, []*registrywire.CapabilityDescription{{
		SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "relay", DisplayName: "NHA042 simulated relay",
		Constraints: &registrywire.CapabilityDescription_Switch{Switch: &registrywire.SwitchConstraints{SetOn: true}},
	}})
	if err != nil {
		return err
	}
	bindings, err := source.Discover(ctx, []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		return errors.Join(integrations.ErrInvalidInput, err)
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		return err
	}
	value := domain.NewSwitchValue(false)
	at := p.clock.Now()
	observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		return err
	}
	if err := source.Publish(ctx, bindings[0], observation); err != nil {
		return err
	}
	if err := p.host.AttachCommandDriver(session, p.driver); err != nil {
		return err
	}
	p.mu.Lock()
	p.session, p.binding = session, bindings[0]
	p.mu.Unlock()
	p.readyOnce.Do(func() { close(p.ready) })
	return nil
}
func (*nha042ServerPilot) Stop(context.Context) error { return nil }
func (p *nha042ServerPilot) emit(t *testing.T, name string) {
	t.Helper()
	// HTTP readiness does not join Supervisor's asynchronous Pilot.Start.
	// Wait for this fixture's real session/binding publication, then copy
	// them under its lock in case a later attempt replaces the session.
	nha034WaitSignal(t, p.ready, "simulated pilot session readiness")
	p.mu.Lock()
	session, binding := p.session, p.binding
	p.mu.Unlock()
	event, err := integrations.NewDeviceEvent(name, p.clock.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Emit(context.Background(), binding, event); err != nil {
		t.Fatal(err)
	}
}

func (p *nha042ServerPilot) startCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.starts
}

type nha042ServerResolver struct {
	entities map[domain.HAAlias]domain.EntityID
}

func (r nha042ServerResolver) ResolveEntity(alias domain.HAAlias) (domain.EntityID, error) {
	if entity, ok := r.entities[alias]; ok {
		return entity, nil
	}
	return domain.EntityID{}, ErrInvalidTriggerComposition
}
func (nha042ServerResolver) ResolveDevice(ir.HADeviceRef) (domain.DeviceID, error) {
	return domain.DeviceID{}, ErrInvalidTriggerComposition
}
func (nha042ServerResolver) ResolveCalendar(domain.HAAlias) (automationtriggers.CalendarSourceID, error) {
	return automationtriggers.CalendarSourceID{}, ErrInvalidTriggerComposition
}

type nha042NoPermanentOutput struct{}

func (nha042NoPermanentOutput) PublishReactiveTemplate(context.Context, *integrations.Manager, reactivetemplate.Update) error {
	return errors.New("NHA042 wait-only service produced a permanent update")
}

func nha042ServerPolicy(ctx context.Context, store *storage.Store, account contracts.AccountID, entity domain.EntityID, omitStateRead bool) error {
	if err := store.EnsureAuthorizationPolicies(ctx); err != nil {
		return err
	}
	current, err := store.GetAuthorizationPolicy(ctx, account, account)
	if err != nil {
		return err
	}
	version, err := current.Version()
	if err != nil {
		return err
	}
	expected, err := authorization.ExpectVersion(version)
	if err != nil {
		return err
	}
	accountText, _ := account.Value()
	entityText, _ := entity.Value()
	text := fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n  - scope: {family: actions, operation: read}\n    objects: {entities: {explicit: [{entity_id: %q, capabilities: {all: {}}}]}}\n  - scope: {family: actions, operation: write}\n    objects: {entities: {explicit: [{entity_id: %q, capabilities: {all: {}}}]}}\n", accountText, entityText, entityText)
	if !omitStateRead {
		text += fmt.Sprintf("  - scope: {family: state_registry, operation: read}\n    objects: {entities: {explicit: [{entity_id: %q, capabilities: {all: {}}}]}}\n", entityText)
	}
	source, err := authorization.AdmitPolicySource(account, []byte(text))
	if err != nil {
		return err
	}
	outcome, err := store.ReplaceAuthorizationPolicy(ctx, account, account, source, expected)
	if err != nil {
		return err
	}
	_, _, err = outcome.Result()
	return err
}

func nha042ServerPrograms(body, child string, scriptsOnly bool) ([]ir.Automation, []ir.Script, error) {
	entity, err := ir.NewEntity(nha042ServerAlias, ir.ReferencePresent, ir.SupportObserveState, ir.SupportSwitchTurnOn)
	if err != nil {
		return nil, nil, err
	}
	sun, err := ir.NewEntity("sun.sun", ir.ReferencePresent, ir.SupportObserveState)
	if err != nil {
		return nil, nil, err
	}
	var events []ir.Event
	for _, name := range []string{"nha042_start", "nha042_release", "nha042_probe"} {
		event, err := ir.NewEvent(name, ir.ReferencePresent)
		if err != nil {
			return nil, nil, err
		}
		events = append(events, event)
	}
	catalogue, err := ir.NewCatalogue([]ir.Entity{entity, sun}, nil, events)
	if err != nil {
		return nil, nil, err
	}
	parentID, err := ir.ParseScriptID("script.nha042_parent")
	if err != nil {
		return nil, nil, err
	}
	ids := []ir.ScriptID{parentID}
	if child != "" {
		id, err := ir.ParseScriptID("script.nha042_child")
		if err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
	}
	catalogue, err = catalogue.WithScripts(ids)
	if err != nil {
		return nil, nil, err
	}
	var scripts []ir.Script
	for index, sequence := range []string{body, child} {
		if index == 1 && child == "" {
			break
		}
		name, _ := ids[index].Value()
		file, err := parser.NewSourceFile(fmt.Sprintf("nha042-server-%d.yaml", index), []byte("mode: parallel\nsequence:\n"+sequence))
		if err != nil {
			return nil, nil, err
		}
		compiled, err := ir.CompileScriptSources(name, file.Path(), []parser.SourceFile{file}, catalogue)
		if err != nil || len(compiled.Accepted()) != 1 || len(compiled.Rejected()) != 0 {
			return nil, nil, fmt.Errorf("NHA042 server script %s: %v rejected=%+v", name, err, compiled.Rejected())
		}
		scripts = append(scripts, compiled.Accepted()[0])
	}
	if scriptsOnly {
		return nil, scripts, nil
	}
	file, err := parser.NewSourceFile("nha042-server-automation.yaml", []byte("id: nha042_server\ntriggers:\n- trigger: event\n  event_type: nha042_start\nactions:\n- action: script.nha042_parent\n  data: {}\n"))
	if err != nil {
		return nil, nil, err
	}
	compiled, err := ir.CompileSources("automation.nha042_server", file.Path(), []parser.SourceFile{file}, catalogue)
	if err != nil || len(compiled.Accepted()) != 1 || len(compiled.Rejected()) != 0 {
		return nil, nil, fmt.Errorf("NHA042 server automation: %v rejected=%+v", err, compiled.Rejected())
	}
	automations := compiled.Accepted()
	probe, err := parser.NewSourceFile("nha042-server-probe.yaml", []byte("id: nha042_probe\ntriggers:\n- trigger: event\n  event_type: nha042_probe\nactions:\n- stop: causal_probe\n"))
	if err != nil {
		return nil, nil, err
	}
	barrier, err := ir.CompileSources("automation.nha042_probe", probe.Path(), []parser.SourceFile{probe}, catalogue)
	if err != nil || len(barrier.Accepted()) != 1 || len(barrier.Rejected()) != 0 {
		return nil, nil, fmt.Errorf("NHA042 causal probe: %v rejected=%+v", err, barrier.Rejected())
	}
	return append(automations, barrier.Accepted()...), scripts, nil
}

type nha042ServerOptions struct {
	dependencies   *runDependencies
	sharedJobs     bool
	sceneCatalogue *scenes.Catalogue
	omitStateRead  bool
	blockedDriver  bool
	scriptsOnly    bool
	clock          automationtriggers.Clock
	solar          *solar.Settings
	solarClock     automationtriggers.Clock
	mqttConflict   bool
	change         func(*RuntimePlan)
}
type nha042ServerRig struct {
	run            *nha034RunHandle
	store          *storage.Store
	host           *integrations.Manager
	identity       *nha044Identity
	composition    *AutomationRunsComposition
	commands       *DurableCommandsComposition
	jobComposition *DurableJobsComposition
	jobServices    DurableJobsServices
	pilot          *nha042ServerPilot
	driver         *nha042ServerDriver
}

// The job handler borrows the actual Run sequencer after its registry has
// attached, not a hard-coded epoch or a second projection.
type nha042ServerEpoch struct{ rig *nha042ServerRig }

func (e nha042ServerEpoch) CurrentAdmissionEpoch() (string, error) {
	if e.rig == nil || e.rig.identity == nil || e.rig.identity.Sequence() == nil {
		return "", integrations.ErrInvalidInput
	}
	return e.rig.identity.Sequence().CurrentAdmissionEpoch()
}

func nha042StartServer(t *testing.T, environment *nha042ServerEnvironment, body, child string, options nha042ServerOptions) *nha042ServerRig {
	t.Helper()
	clock := options.clock
	if clock == nil {
		clock = automationtriggers.RealClock{}
	}
	rig := &nha042ServerRig{driver: &nha042ServerDriver{observer: environment.driverObserver, entered: make(chan struct{}), release: make(chan struct{})}}
	if !options.blockedDriver {
		rig.driver.unblock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	rig.run = &nha034RunHandle{cancel: cancel, ready: make(chan struct{}), done: make(chan struct{})}
	factory := func(ctx context.Context, store *storage.Store) (RuntimePlan, error) {
		rig.store = store
		manifest, err := registry.ParseManifest(scope.ManifestV1())
		if err != nil {
			return RuntimePlan{}, err
		}
		repository, err := registry.NewRepository(store, manifest)
		if err != nil {
			return RuntimePlan{}, err
		}
		instance, err := domain.ParseIntegrationInstanceID("nha042-simulated-pilot")
		if err != nil {
			return RuntimePlan{}, err
		}
		key, err := integrations.NewSourceKey("nha042-simulated-device", "relay")
		if err != nil {
			return RuntimePlan{}, err
		}
		resolved, err := repository.ConfigureBinding(ctx, instance, key, nha042ServerAlias)
		if err != nil {
			return RuntimePlan{}, err
		}
		if err := nha042ServerPolicy(ctx, store, environment.account, resolved.Entity, options.omitStateRead); err != nil {
			return RuntimePlan{}, err
		}
		alias, _ := domain.ParseHAAlias(nha042ServerAlias)
		resolver := nha042ServerResolver{entities: map[domain.HAAlias]domain.EntityID{alias: resolved.Entity}}
		automations, scripts, err := nha042ServerPrograms(body, child, options.scriptsOnly)
		if err != nil {
			return RuntimePlan{}, err
		}
		runs, err := automationruns.NewPlanWithScripts(automations, scripts)
		if err != nil {
			return RuntimePlan{}, err
		}
		if options.sharedJobs {
			rig.jobComposition, err = NewDurableJobsComposition(DurableJobsInput{
				Commands: &DurableJobCommandInput{Generation: nha094Generation{}, Epoch: nha042ServerEpoch{rig}},
				Clock:    clock, Timer: jobs.RealTimer{},
				Configure: func(ctx context.Context, services DurableJobsServices) error {
					if commands, present := services.Commands(); !present || commands == nil {
						return errors.New("NHA042 job did not borrow commands")
					}
					// This callback precedes Supervisor.Start: the exact Busy guard
					// must refuse a second runtime before any second SQL authority.
					if _, err := integrations.NewDurableCommandServiceWithClock(ctx, rig.host, nha094Generation{}, store, clock); !errors.Is(err, integrations.ErrDurableRuntimeBusy) {
						return fmt.Errorf("NHA042 pre-start duplicate runtime: %v", err)
					}
					rig.jobServices = services
					return nil
				},
			})
			if err != nil {
				return RuntimePlan{}, err
			}
			rig.commands = rig.jobComposition.CommandComposition()
		} else {
			rig.commands, err = NewDurableCommandsComposition(DurableCommandsInput{Generation: nha094Generation{}, Clock: clock})
			if err != nil {
				return RuntimePlan{}, err
			}
		}
		rig.composition, err = NewScriptRunsComposition(ScriptRunsInput{Plan: runs, Commands: rig.commands,
			Authority: &nha042ServerAuthority{environment.principal}, Clock: clock, Zone: time.UTC, CommandDeadline: time.Minute, Scenes: options.sceneCatalogue})
		if err != nil {
			return RuntimePlan{}, err
		}
		reactivePlan, err := (reactivetemplate.Plan{}).WithScriptWaits(time.UTC)
		if err != nil {
			return RuntimePlan{}, err
		}
		reactive, err := NewReactiveTemplateComposition(reactivePlan, nha042NoPermanentOutput{}, reactivetemplate.RealClock{})
		if err != nil {
			return RuntimePlan{}, err
		}
		rig.pilot = &nha042ServerPilot{key: key, driver: rig.driver, clock: clock, ready: make(chan struct{})}
		spec, err := integrations.NewPilotSpec(instance, func() integrations.Pilot { return rig.pilot })
		if err != nil {
			return RuntimePlan{}, err
		}
		plan := RuntimePlan{Identity: repository, Pilots: []integrations.PilotSpec{spec}, ReactiveTemplates: reactive,
			AutomationRuns: rig.composition, DurableCommands: rig.commands, DurableJobs: rig.jobComposition,
			Configure: func(_ context.Context, host *integrations.Manager, _ *storage.Store) error {
				rig.host, rig.pilot.host = host, host
				return nil
			}}
		input := automationtriggers.PlanInput{Automations: automations, Scripts: scripts, Resolver: resolver, Zone: "UTC"}
		if options.solar != nil {
			definitions, err := solar.ConfiguredBindings()
			if err != nil {
				return RuntimePlan{}, err
			}
			solarInstance, err := solar.InstanceID()
			if err != nil {
				return RuntimePlan{}, err
			}
			for _, definition := range definitions {
				text, err := definition.Alias().Value()
				if err != nil {
					return RuntimePlan{}, err
				}
				bound, err := repository.ConfigureBinding(ctx, solarInstance, definition.SourceKey(), text)
				if err != nil {
					return RuntimePlan{}, err
				}
				resolver.entities[definition.Alias()] = bound.Entity
			}
			solarClock := clock
			if options.solarClock != nil {
				solarClock = options.solarClock
			}
			if err := addConfiguredSolar(&plan, *options.solar, solarClock); err != nil {
				return RuntimePlan{}, err
			}
			input, err = solar.ConfigureTriggerInput(*options.solar, input)
			if err != nil {
				return RuntimePlan{}, err
			}
		}
		if options.mqttConflict {
			settings, err := mqtt.ParseSettings(mqtt.Input{Profile: "5", Broker: "mqtt://127.0.0.1:1883", Instance: "nha042-never-connect",
				Mode: "passive", UsernameSecret: "synthetic-user", PasswordSecret: "synthetic-password", Subscriptions: []string{"synthetic/wait-only"}})
			if err != nil {
				return RuntimePlan{}, err
			}
			filter, err := mqtt.ParseTopicFilter("synthetic/wait-only")
			if err != nil {
				return RuntimePlan{}, err
			}
			proof, err := mqtt.NewTriggerEdgePilot(settings, repository, []mqtt.TopicFilter{filter})
			if err != nil {
				return RuntimePlan{}, err
			}
			input.MQTTSettings, input.MQTTIngress = settings, proof
			mqttInstance, err := settings.Instance()
			if err != nil {
				return RuntimePlan{}, err
			}
			conflicting, err := integrations.NewPilotSpec(mqttInstance, func() integrations.Pilot { return rig.pilot })
			if err != nil {
				return RuntimePlan{}, err
			}
			plan.Pilots = append(plan.Pilots, conflicting)
		}
		triggers, err := automationtriggers.NewPlan(input)
		if err != nil {
			return RuntimePlan{}, err
		}
		plan.AutomationTriggers = &triggers
		rig.identity = &nha044Identity{repo: repository}
		plan.Identity = rig.identity
		if options.change != nil {
			options.change(&plan)
		}
		return plan, nil
	}
	go func() {
		if options.dependencies != nil {
			dependencies := *options.dependencies
			if options.clock != nil {
				dependencies.sequenceClock, dependencies.solarClock = options.clock, options.clock
			}
			rig.run.err = runWithDependencies(ctx, environment.config, (&net.ListenConfig{}).Listen,
				func(net.Addr) { close(rig.run.ready) }, factory, dependencies)
		} else if options.clock != nil {
			rig.run.err = runWithDependencies(ctx, environment.config, (&net.ListenConfig{}).Listen,
				func(net.Addr) { close(rig.run.ready) }, factory, runDependencies{sequenceClock: options.clock, solarClock: options.clock})
		} else {
			rig.run.err = RunWithFactory(ctx, environment.config, (&net.ListenConfig{}).Listen, func(net.Addr) { close(rig.run.ready) }, factory)
		}
		close(rig.run.done)
	}()
	t.Cleanup(func() {
		if t.Failed() && rig.composition != nil {
			traces, _ := rig.composition.NodeTraces()
			for _, trace := range traces[max(0, len(traces)-32):] {
				t.Logf("NHA042 node run=%d source=%s kind=%d stage=%d failure=%d", trace.RunID(), trace.Source().Object(), trace.Kind(), trace.Stage(), trace.Failure())
			}
		}
		rig.driver.unblock()
		rig.run.cancel()
		select {
		case <-rig.run.done:
		case <-time.After(12 * time.Second):
			t.Error("NHA042 Run did not join during cleanup")
		}
	})
	return rig
}

func nha042ServerWait(t *testing.T, label string, predicate func() bool) {
	t.Helper()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !predicate() {
		select {
		case <-deadline.C:
			t.Fatalf("NHA042 did not observe %s", label)
		case <-tick.C:
		}
	}
}
func nha042ServerNode(composition *AutomationRunsComposition, object string, kind ir.ActionKind, stage automationruns.NodeStage) bool {
	traces, _ := composition.NodeTraces()
	return slices.ContainsFunc(traces, func(trace automationruns.NodeTrace) bool {
		return trace.Source().Object() == object && trace.Kind() == kind && trace.Stage() == stage && trace.RunID() != 0 && trace.Generation() != 0
	})
}

// The probe is an ordinary compiled automation, not another ResultReader or
// observer engine. Its stopped node proves that this Run consumed the input
// through 040, prepared/admitted it in 041 and ran the concrete interpreter.
func nha042ServerProbe(t *testing.T, rig *nha042ServerRig) {
	t.Helper()
	var previous automationruns.RunID
	before, _ := rig.composition.NodeTraces()
	for _, trace := range before {
		if trace.Source().Object() == "automation.nha042_probe" {
			previous = max(previous, trace.RunID())
		}
	}
	rig.pilot.emit(t, "nha042_probe")
	nha042ServerWait(t, "new runtime's causal probe", func() bool {
		traces, _ := rig.composition.NodeTraces()
		return slices.ContainsFunc(traces, func(trace automationruns.NodeTrace) bool {
			return trace.Source().Object() == "automation.nha042_probe" && trace.Kind() == ir.ActionStop &&
				trace.Stage() == automationruns.NodeStopped && trace.RunID() > previous
		})
	})
}

func nha042ServerNoRoutineResumed(t *testing.T, rig *nha042ServerRig) {
	t.Helper()
	traces, _ := rig.composition.NodeTraces()
	for _, trace := range traces {
		if trace.Source().Object() != "automation.nha042_probe" {
			t.Fatal("reopen executed an ordinary routine before its new trigger")
		}
	}
	if len(rig.driver.snapshot()) != 0 {
		t.Fatal("reopen emitted an effect without a new routine trigger")
	}
}
func nha042ServerNoJobs(t *testing.T, observer *pgx.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var definitions, jobs int
	err := observer.QueryRow(ctx, `SELECT (SELECT count(*) FROM public.scheduled_job_definitions),count(*) FROM public.scheduled_jobs`).Scan(&definitions, &jobs)
	if err != nil || definitions != 0 || jobs != 0 {
		t.Fatalf("scripts created jobs: definitions=%d jobs=%d err=%v", definitions, jobs, err)
	}
}
func nha042ServerIntent(t *testing.T, observer *pgx.Conn, id domain.CommandID) (string, *actions.CommandResponse) {
	t.Helper()
	text, err := id.Value()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var state string
	var bytes []byte
	if err := observer.QueryRow(ctx, `SELECT state,response FROM public.command_intents WHERE command_id=$1`, text).Scan(&state, &bytes); err != nil {
		t.Fatal(err)
	}
	var response actions.CommandResponse
	if len(bytes) != 0 {
		if err := proto.Unmarshal(bytes, &response); err != nil {
			t.Fatal(err)
		}
	}
	return state, &response
}

func nha042ServerSQL(t *testing.T, observer *pgx.Conn, query string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := observer.Exec(ctx, query); err != nil {
		t.Fatal(err)
	}
}

// This is the causal counterpart to the positive Run assembly: action rights
// alone cannot expose the state/capability required for command preparation.
func TestNHA042RunRefusesUnreadableCommandStateBeforeEffect(t *testing.T) {
	environment := nha042ServerEnvironmentFor(t)
	rig := nha042StartServer(t, environment, nha042ServerCommand, "", nha042ServerOptions{omitStateRead: true})
	rig.run.waitReady(t)
	rig.pilot.emit(t, "nha042_start")
	nha042ServerWait(t, "typed refusal of unreadable command state", func() bool {
		return nha042ServerNode(rig.composition, "script.nha042_parent", ir.ActionCall, automationruns.NodeFailed)
	})
	if len(rig.driver.snapshot()) != 0 {
		t.Fatal("unreadable command state reached Driver")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var count int
	if err := environment.observer.QueryRow(ctx, `SELECT count(*) FROM public.command_intents`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unreadable preparation acquired a command intention: %d %v", count, err)
	}
	rig.run.cancel()
	if err := rig.run.waitDone(t); err != nil {
		t.Fatal(err)
	}
	nha034WaitSignal(t, rig.composition.Done(), "denied script join")
	nha034WaitRunStoreClosed(t, rig.store)
}

func TestNHA042RunUsesConcreteInterpreterAndSharedAuthorities(t *testing.T) {
	environment := nha042ServerEnvironmentFor(t)
	body := nha042ServerCommand + "- action: script.turn_on\n  target: {entity_id: script.nha042_child}\n  data: {}\n- wait_template: '{{ false }}'\n"
	child := "- wait_for_trigger:\n  - trigger: event\n    event_type: nha042_release\n" + nha042ServerCommand
	rig := nha042StartServer(t, environment, body, child, nha042ServerOptions{})
	rig.run.waitReady(t)
	rig.pilot.emit(t, "nha042_start")
	nha042ServerWait(t, "concrete trigger and template waits", func() bool {
		return nha042ServerNode(rig.composition, "script.nha042_parent", ir.ActionWaitTemplate, automationruns.NodeWaiting) &&
			nha042ServerNode(rig.composition, "script.nha042_child", ir.ActionWait, automationruns.NodeWaiting)
	})
	if ids := rig.driver.snapshot(); len(ids) != 1 {
		t.Fatalf("concrete initial effects=%d", len(ids))
	}
	// After Supervisor.Start, Host freezes runtime registration. A second
	// service is refused there, before the earlier pre-start Busy guard.
	if _, err := integrations.NewDurableCommandService(context.Background(), rig.host, nha094Generation{}, rig.store); !errors.Is(err, integrations.ErrSupervisorState) {
		t.Fatalf("running Host admitted another command runtime: %v", err)
	}
	nha042ServerNoJobs(t, environment.observer)
	rig.pilot.emit(t, "nha042_release")
	nha042ServerWait(t, "detached child's real command", func() bool { return len(rig.driver.snapshot()) == 2 })
	rig.run.cancel()
	if err := rig.run.waitDone(t); err != nil {
		t.Fatal(err)
	}
	nha034WaitSignal(t, rig.composition.Done(), "actual parent and detached child join")
	nha034WaitRunStoreClosed(t, rig.store)
	for _, id := range rig.driver.snapshot() {
		state, response := nha042ServerIntent(t, environment.observer, id)
		if state != "result" || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
			t.Fatalf("cancel fabricated inverse effect: %s %v", state, response.Status)
		}
	}
}

func TestNHA042RunRestartCancelsWaitWithoutResumeOrUndo(t *testing.T) {
	for _, step := range []struct {
		name   string
		source string
		kind   ir.ActionKind
		stage  automationruns.NodeStage
	}{
		{"delay", "- delay: {minutes: 60}\n", ir.ActionDelay, automationruns.NodeEntered},
		{"template", "- wait_template: '{{ false }}'\n", ir.ActionWaitTemplate, automationruns.NodeWaiting},
	} {
		t.Run(step.name, func(t *testing.T) {
			environment := nha042ServerEnvironmentFor(t)
			clock := &nha105RunClock{wall: time.Now().UTC().Add(24 * time.Hour)}
			options := nha042ServerOptions{clock: clock}
			body := nha042ServerCommand + step.source + nha042ServerCommand
			first := nha042StartServer(t, environment, body, "", options)
			first.run.waitReady(t)
			first.pilot.emit(t, "nha042_start")
			nha042ServerWait(t, "real first wait", func() bool { return nha042ServerNode(first.composition, "script.nha042_parent", step.kind, step.stage) })
			before := first.driver.snapshot()
			if len(before) != 1 {
				t.Fatalf("effects before restart=%d", len(before))
			}
			first.run.cancel()
			if err := first.run.waitDone(t); err != nil {
				t.Fatal(err)
			}
			nha034WaitSignal(t, first.composition.Done(), "first script join")
			nha034WaitRunStoreClosed(t, first.store)
			second := nha042StartServer(t, environment, body, "", options)
			second.run.waitReady(t)
			// Advance the actual clock shared by the old/new runtime beyond the
			// original 60-minute delay, then await a real consumed probe. An
			// immediate empty snapshot after emit would not establish this.
			clock.advance(61 * time.Minute)
			nha042ServerProbe(t, second)
			nha042ServerNoRoutineResumed(t, second)
			state, response := nha042ServerIntent(t, environment.observer, before[0])
			if state != "result" || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
				t.Fatal("reopen changed prior dispatch")
			}
			second.pilot.emit(t, "nha042_start")
			nha042ServerWait(t, "new run after explicit trigger", func() bool {
				return nha042ServerNode(second.composition, "script.nha042_parent", step.kind, step.stage) && len(second.driver.snapshot()) == 1
			})
			if second.driver.snapshot()[0] == before[0] {
				t.Fatal("new process reused an old command occurrence")
			}
			second.run.cancel()
			if err := second.run.waitDone(t); err != nil {
				t.Fatal(err)
			}
			if len(first.driver.snapshot()) != 1 || len(second.driver.snapshot()) != 1 {
				t.Fatal("wait emitted a later or inverse action")
			}
			nha042ServerNoJobs(t, environment.observer)
		})
	}
}

func TestNHA042RunRetainsStoreUntilDispatchedScriptActuallyDrains(t *testing.T) {
	environment := nha042ServerEnvironmentFor(t)
	rig := nha042StartServer(t, environment, nha042ServerCommand+nha042ServerCommand, "", nha042ServerOptions{blockedDriver: true})
	rig.run.waitReady(t)
	rig.pilot.emit(t, "nha042_start")
	nha034WaitSignal(t, rig.driver.entered, "actual dispatched effect")
	rig.run.cancel()
	if err := rig.run.waitDone(t); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run lost shared shutdown deadline: %v", err)
	}
	select {
	case <-rig.composition.Done():
		t.Fatal("script Done preceded actual Driver return")
	default:
	}
	if err := nha034ProbeRunStore(rig.store); err != nil {
		t.Fatalf("Run closed live dispatch Store: %v", err)
	}
	ids := rig.driver.snapshot()
	if len(ids) != 1 {
		t.Fatalf("blocked dispatch effects=%d", len(ids))
	}
	state, _ := nha042ServerIntent(t, environment.observer, ids[0])
	if state != "dispatching" {
		t.Fatalf("live effect prematurely finalized: %s", state)
	}
	rig.driver.unblock()
	nha034WaitSignal(t, rig.composition.Done(), "late script join")
	nha034WaitRunStoreClosed(t, rig.store)
	state, response := nha042ServerIntent(t, environment.observer, ids[0])
	if state != "result" || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || len(rig.driver.snapshot()) != 1 {
		t.Fatalf("late effect changed or second action ran: %s %v", state, response.Status)
	}
	reopened := nha042StartServer(t, environment, nha042ServerCommand+nha042ServerCommand, "", nha042ServerOptions{})
	reopened.run.waitReady(t)
	nha042ServerProbe(t, reopened)
	nha042ServerNoRoutineResumed(t, reopened)
}

func TestNHA042RunRejectedSQLFinalizationReopensIndeterminateWithoutResend(t *testing.T) {
	environment := nha042ServerEnvironmentFor(t)
	rig := nha042StartServer(t, environment, nha042ServerCommand+nha042ServerCommand, "", nha042ServerOptions{})
	rig.run.waitReady(t)
	// This injects a refused SQL finalization after the fake effect. It is
	// not a lost network response or an uncertain COMMIT acknowledgement.
	nha042ServerSQL(t, environment.observer, `CREATE FUNCTION public.nha042_refused_finalization() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'NHA042 refused command finalization'; END $$;
CREATE TRIGGER nha042_refused_finalization BEFORE UPDATE ON public.command_intents FOR EACH ROW WHEN (OLD.state='dispatching' AND NEW.state<>'dispatching') EXECUTE FUNCTION public.nha042_refused_finalization()`)
	rig.pilot.emit(t, "nha042_start")
	nha042ServerWait(t, "typed command failure after fake effect", func() bool {
		return nha042ServerNode(rig.composition, "script.nha042_parent", ir.ActionCall, automationruns.NodeFailed)
	})
	rig.run.cancel()
	if err := rig.run.waitDone(t); err != nil {
		t.Fatal(err)
	}
	nha034WaitRunStoreClosed(t, rig.store)
	ids := rig.driver.snapshot()
	if len(ids) != 1 {
		t.Fatalf("refused SQL finalization effects=%d", len(ids))
	}
	state, _ := nha042ServerIntent(t, environment.observer, ids[0])
	if state != "dispatching" {
		t.Fatalf("fault did not preserve recovery seam: %s", state)
	}
	nha042ServerSQL(t, environment.observer, `DROP TRIGGER nha042_refused_finalization ON public.command_intents; DROP FUNCTION public.nha042_refused_finalization()`)
	reopened := nha042StartServer(t, environment, nha042ServerCommand+nha042ServerCommand, "", nha042ServerOptions{})
	reopened.run.waitReady(t)
	nha042ServerProbe(t, reopened)
	nha042ServerNoRoutineResumed(t, reopened)
	state, response := nha042ServerIntent(t, environment.observer, ids[0])
	if state != "indeterminate" || response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || len(reopened.driver.snapshot()) != 0 {
		t.Fatalf("recovery resent or cancelled possible effect: %s %v", state, response.Status)
	}
}

func TestNHA042RunAdmissionAndBootstrapBoundaries(t *testing.T) {
	for _, test := range []struct {
		name    string
		body    string
		options nha042ServerOptions
	}{
		{"missing024", nha042ServerCommand, nha042ServerOptions{change: func(plan *RuntimePlan) { plan.DurableCommands = nil }}},
		{"missing044", "- wait_template: '{{ false }}'\n", nha042ServerOptions{change: func(plan *RuntimePlan) { plan.ReactiveTemplates = nil }}},
		{"missing040", "- wait_for_trigger:\n  - trigger: event\n    event_type: nha042_release\n", nha042ServerOptions{change: func(plan *RuntimePlan) { plan.AutomationTriggers = nil }}},
		{"competing-reader", nha042ServerCommand, nha042ServerOptions{change: func(plan *RuntimePlan) {
			plan.ConfigureAutomationTriggerResults = func(context.Context, AutomationTriggerResults) error { return nil }
		}}},
		{"mismatched-IDs", nha042ServerCommand, nha042ServerOptions{change: func(plan *RuntimePlan) {
			other, _ := nha041ServerPlans(t, "single")
			plan.AutomationTriggers = &other
		}}},
		{"duplicate-MQTT", "- wait_for_trigger:\n  - trigger: mqtt\n    topic: synthetic/wait-only\n", nha042ServerOptions{mqttConflict: true}},
		{"bootstrap-failure", nha042ServerCommand, nha042ServerOptions{change: func(plan *RuntimePlan) {
			previous := plan.Configure
			plan.Configure = func(ctx context.Context, host *integrations.Manager, store *storage.Store) error {
				if err := previous(ctx, host, store); err != nil {
					return err
				}
				return ErrInvalidTriggerComposition
			}
		}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			environment := nha042ServerEnvironmentFor(t)
			rig := nha042StartServer(t, environment, test.body, "", test.options)
			if err := rig.run.waitDone(t); err == nil {
				t.Fatal("invalid assembly reached serving")
			}
			select {
			case <-rig.run.ready:
				t.Fatal("invalid assembly exposed listener")
			default:
			}
			if len(rig.driver.snapshot()) != 0 || rig.pilot != nil && rig.pilot.startCount() != 0 {
				t.Fatal("bootstrap rejection started a physical path")
			}
			if rig.composition != nil {
				if traces, _ := rig.composition.NodeTraces(); len(traces) != 0 {
					t.Fatal("failed bootstrap executed nodes")
				}
			}
			if rig.store != nil {
				nha034WaitRunStoreClosed(t, rig.store)
			}
		})
	}
	t.Run("scripts-only-without-jobs", func(t *testing.T) {
		environment := nha042ServerEnvironmentFor(t)
		rig := nha042StartServer(t, environment, "- wait_template: '{{ false }}'\n", "", nha042ServerOptions{scriptsOnly: true})
		rig.run.waitReady(t)
		nha042ServerNoJobs(t, environment.observer)
		id, err := ir.ParseScriptID("script.nha042_parent")
		if err != nil {
			t.Fatal(err)
		}
		handle, err := rig.composition.runtime.InvokeScript(context.Background(), id, nil, automationruns.Run{}, ir.ScriptCallDetached)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := handle.Startup(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := prepareScriptRuns(context.Background(), rig.composition, scriptRunServices{}); !errors.Is(err, ErrInvalidTriggerComposition) {
			t.Fatalf("composition prepared twice: %v", err)
		}
		rig.run.cancel()
		if err := rig.run.waitDone(t); err != nil {
			t.Fatal(err)
		}
		nha034WaitSignal(t, handle.Done(), "script-only handle join")
	})
}

// Only the production solar pilot receives this wrapper. Its tracked timers
// share the same wall/monotonic clock as 040, but cannot be confused with the
// trigger engine's own timers when establishing the next publication barrier.
type nha042SolarRunClock struct {
	base   *nha105RunClock
	mu     sync.Mutex
	timers []*nha105RunTimer
}

func (c *nha042SolarRunClock) Now() time.Time         { return c.base.Now() }
func (c *nha042SolarRunClock) MonoNow() time.Duration { return c.base.MonoNow() }
func (c *nha042SolarRunClock) NewTimer(delay time.Duration) automationtriggers.Timer {
	timer := c.base.NewTimer(delay).(*nha105RunTimer)
	c.mu.Lock()
	c.timers = append(c.timers, timer)
	c.mu.Unlock()
	return timer
}
func (c *nha042SolarRunClock) armed(at time.Time) bool {
	c.mu.Lock()
	timers := append([]*nha105RunTimer(nil), c.timers...)
	c.mu.Unlock()
	c.base.mu.Lock()
	defer c.base.mu.Unlock()
	wanted := c.base.tick + at.Sub(c.base.wall)
	for _, timer := range timers {
		if !timer.stopped && timer.due == wanted && timer.due > c.base.tick {
			return true
		}
	}
	return false
}

func nha042SolarPublicationBarrier(t *testing.T, rig *nha042ServerRig, clock *nha042SolarRunClock, calculator *solar.Calculator, at time.Time) domain.SolarValue {
	t.Helper()
	computed, err := calculator.Compute(context.Background(), at)
	if err != nil {
		t.Fatal(err)
	}
	want, captured, _, deadline, err := computed.Components()
	if err != nil || !captured.Equal(at) {
		t.Fatalf("unexpected synthetic solar computation: %v", err)
	}
	if position, present, err := computed.NextPosition(); err != nil {
		t.Fatal(err)
	} else if present && position.Before(deadline) {
		deadline = position
	}
	nha042ServerWait(t, "native solar publication and its exact timer", func() bool {
		sequence := rig.identity.Sequence()
		if sequence == nil {
			return false
		}
		snapshot, err := sequence.Capture()
		if err != nil {
			return false
		}
		for id, record := range snapshot.Registry {
			_, alias, _, err := record.Aliases()
			if err != nil {
				continue
			}
			text, err := alias.Value()
			if err != nil || text != "sun.sun" {
				continue
			}
			state, exists := snapshot.States[id]
			if !exists {
				return false
			}
			current, present := state.CurrentValue()
			if !present {
				return false
			}
			value, present := current.Solar()
			return present && value == want && clock.armed(deadline)
		}
		return false
	})
	return want
}

func TestNHA042RunSolarWaitUsesDeliveredSourceAndHorizonProjection(t *testing.T) {
	environment := nha042ServerEnvironmentFor(t)
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), now.Day()+1, 12, 0, 0, 0, time.UTC)
	clock := &nha105RunClock{wall: start}
	solarClock := &nha042SolarRunClock{base: clock}
	latitude, longitude := 0.0, 0.0
	settings, err := solar.ParseSettings(solar.Input{Latitude: &latitude, Longitude: &longitude, Timezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	input, err := solar.ConfigureTriggerInput(settings, automationtriggers.PlanInput{})
	if err != nil {
		t.Fatal(err)
	}
	next, err := input.SunSource.NextSunset(context.Background(), start, input.Zone, input.SolarPosition)
	if err != nil {
		t.Fatal(err)
	}
	sunset, availability, err := next.Components()
	if err != nil || availability != automationtriggers.SunPresent || !sunset.After(start.Add(time.Hour)) {
		t.Fatalf("unexpected equatorial sunset: %v %v", next, err)
	}
	body := "- wait_for_trigger:\n  - trigger: sun\n    event: sunset\n    offset: '-01:00:00'\n" + nha042ServerCommand +
		"- wait_for_trigger:\n  - trigger: state\n    entity_id: sun.sun\n    from: above_horizon\n    to: below_horizon\n" + nha042ServerCommand
	calculator, err := solar.NewCalculator(settings)
	if err != nil {
		t.Fatal(err)
	}
	rig := nha042StartServer(t, environment, body, "", nha042ServerOptions{clock: clock, solar: &settings, solarClock: solarClock})
	rig.run.waitReady(t)
	initial := nha042SolarPublicationBarrier(t, rig, solarClock, calculator, start)
	initialInput, err := initial.Components()
	if err != nil || initialInput.Horizon != domain.SolarAboveHorizon {
		t.Fatal("synthetic start is not above horizon")
	}
	rig.pilot.emit(t, "nha042_start")
	nha042ServerWait(t, "sunset wait registration", func() bool {
		return nha042ServerNode(rig.composition, "script.nha042_parent", ir.ActionWait, automationruns.NodeWaiting)
	})
	clock.advance(sunset.Add(-time.Hour).Sub(start))
	nha042ServerWait(t, "real solar source wait effect", func() bool { return len(rig.driver.snapshot()) == 1 })
	nha042ServerWait(t, "horizon wait registration", func() bool {
		traces, _ := rig.composition.NodeTraces()
		count := 0
		for _, trace := range traces {
			if trace.Source().Object() == "script.nha042_parent" && trace.Kind() == ir.ActionWait && trace.Stage() == automationruns.NodeWaiting {
				count++
			}
		}
		return count == 2
	})
	// Complete the publication caused by the first jump before another one;
	// HTTP readiness/node traces alone do not prove a pilot timer is armed.
	nha042SolarPublicationBarrier(t, rig, solarClock, calculator, sunset.Add(-time.Hour))
	// The astronomical sunset instant and the apparent-elevation horizon are
	// separate observations. Require the real calculator to prove our second
	// synthetic instant is below; no astronomical tolerance is widened.
	belowAt := sunset.Add(15 * time.Minute)
	below, err := calculator.Compute(context.Background(), belowAt)
	if err != nil {
		t.Fatal(err)
	}
	belowValue, _, _, _, err := below.Components()
	if err != nil {
		t.Fatal(err)
	}
	belowInput, err := belowValue.Components()
	if err != nil || belowInput.Horizon != domain.SolarBelowHorizon {
		t.Fatal("synthetic second instant is not below horizon")
	}
	clock.advance(belowAt.Sub(clock.Now()))
	nha042ServerWait(t, "above_horizon to below_horizon through common matcher", func() bool { return len(rig.driver.snapshot()) == 2 })
	rig.run.cancel()
	if err := rig.run.waitDone(t); err != nil {
		t.Fatal(err)
	}
	nha034WaitSignal(t, rig.composition.Done(), "solar script join")
}

func TestNHA042ScriptCompositionRejectsZeroAndTypedNilPorts(t *testing.T) {
	automations, scripts, err := nha042ServerPrograms("- stop: done\n", "", true)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := automationruns.NewPlanWithScripts(automations, scripts)
	if err != nil {
		t.Fatal(err)
	}
	var generation *nha034Generation
	var authority *nha042ServerAuthority
	var clock *nha105RunClock
	for _, input := range []DurableCommandsInput{{}, {Generation: generation, Clock: automationtriggers.RealClock{}}, {Generation: nha094Generation{}, Clock: clock}} {
		if _, err := NewDurableCommandsComposition(input); !errors.Is(err, ErrInvalidTriggerComposition) {
			t.Fatalf("invalid durable declaration admitted: %v", err)
		}
	}
	commands, err := NewDurableCommandsComposition(DurableCommandsInput{Generation: nha094Generation{}, Clock: automationtriggers.RealClock{}})
	if err != nil {
		t.Fatal(err)
	}
	input := ScriptRunsInput{Plan: plan, Commands: commands, Authority: authority, Clock: automationtriggers.RealClock{}, Zone: time.UTC, CommandDeadline: time.Minute}
	if _, err := NewScriptRunsComposition(input); !errors.Is(err, ErrInvalidTriggerComposition) {
		t.Fatal("typed nil authority admitted")
	}
	input.Authority = &nha042ServerAuthority{}
	for _, mutate := range []func(*ScriptRunsInput){
		func(input *ScriptRunsInput) { input.Plan = automationruns.Plan{} },
		func(input *ScriptRunsInput) { input.Commands = &DurableCommandsComposition{} },
		func(input *ScriptRunsInput) { input.Clock = clock },
		func(input *ScriptRunsInput) { input.Zone = nil },
		func(input *ScriptRunsInput) { input.CommandDeadline = 0 },
		func(input *ScriptRunsInput) { input.CommandDeadline = 24*time.Hour + time.Nanosecond },
	} {
		copy := input
		mutate(&copy)
		if _, err := NewScriptRunsComposition(copy); !errors.Is(err, ErrInvalidTriggerComposition) {
			t.Fatalf("invalid script declaration admitted: %v", err)
		}
	}
	var composition *AutomationRunsComposition
	if traces, evicted := composition.NodeTraces(); len(traces) != 0 || evicted != 0 {
		t.Fatal("nil composition fabricated traces")
	}
}

var _ automationruns.ExecutionAuthority = (*nha042ServerAuthority)(nil)
var _ integrations.CommandDriver = (*nha042ServerDriver)(nil)
var _ integrations.Pilot = (*nha042ServerPilot)(nil)
var _ ReactiveTemplateOutput = nha042NoPermanentOutput{}

func TestNHA042RunSharesCommandsWithJobsAndTracksLateSceneClose(t *testing.T) {
	environment := nha042ServerEnvironmentFor(t)
	selection, err := logicselection.LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := scenes.CompileSelected(selection, scope.LogicMVPSourcesV1())
	if err != nil {
		t.Fatal(err)
	}
	rig := nha042StartServer(t, environment, "- wait_template: '{{ false }}'\n", "", nha042ServerOptions{
		blockedDriver: true, sharedJobs: true, sceneCatalogue: &catalogue,
	})
	rig.run.waitReady(t)
	rig.pilot.emit(t, "nha042_start")
	nha042ServerWait(t, "concrete script wait beside shared jobs", func() bool {
		return nha042ServerNode(rig.composition, "script.nha042_parent", ir.ActionWaitTemplate, automationruns.NodeWaiting)
	})
	if rig.commands != rig.jobComposition.CommandComposition() {
		t.Fatal("jobs and scripts did not share the admitted descriptor")
	}
	if commands, present := rig.jobServices.Commands(); !present || commands == nil {
		t.Fatal("configured jobs have no command service")
	}
	runtime := scriptScenesRuntime(rig.composition)
	if runtime == nil || runtime.service == nil {
		t.Fatal("Run did not compose the admitted scene catalogue")
	}
	sending, actor, err := (&nha042ServerAuthority{environment.principal}).Context(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := domain.ParseHAAlias(nha042ServerAlias)
	member, err := scenes.NewExplicitSwitch(alias, true)
	if err != nil {
		t.Fatal(err)
	}
	sceneID, _ := domain.ParseHAAlias("scene.nha042_runtime_shared")
	ref, omissions, err := runtime.service.CreateTemporary(sending, sceneID, []scenes.ExplicitMember{member}, nil, time.Minute, nil)
	if err != nil || len(omissions) != 0 {
		t.Fatalf("Run scene preparation: %v omissions=%d", err, len(omissions))
	}
	invocation, _ := domain.ParseCommandID("nha042-run-shared-scene")
	slot, err := scenes.NewPreparationSlot(runtime.service, ref, actor, invocation, nil, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := slot.PrepareOnce(sending)
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result scenes.ApplyResult
		err    error
	}
	applied := make(chan outcome, 1)
	go func() {
		result, err := runtime.service.ApplyPrepared(sending, prepared)
		applied <- outcome{result, err}
	}()
	nha034WaitSignal(t, rig.driver.entered, "Run-owned scene actual fake dispatch")
	rig.run.cancel()
	if err := rig.run.waitDone(t); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("late scene lost shared shutdown budget: %v", err)
	}
	nha034WaitSignal(t, rig.composition.Done(), "ordinary script wait drain")
	select {
	case <-runtime.done:
		t.Fatal("scene Close joined before its real effect returned")
	default:
	}
	if err := nha034ProbeRunStore(rig.store); err != nil {
		t.Fatalf("Run closed Store beneath its scene owner: %v", err)
	}
	ids := rig.driver.snapshot()
	if len(ids) != 1 {
		t.Fatalf("late scene effects=%d", len(ids))
	}
	state, _ := nha042ServerIntent(t, environment.observer, ids[0])
	if state != "dispatching" {
		t.Fatalf("live scene effect finalized early: %s", state)
	}
	rig.driver.unblock()
	select {
	case result := <-applied:
		// Close cancels the scene context. Its final authorization must
		// refuse the response; the already dispatched effect remains in SQL.
		if !errors.Is(result.err, integrations.ErrCommandAuthorizationDenied) {
			t.Fatalf("late scene response after Close: %v", result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scene caller did not return after actual effect")
	}
	nha034WaitSignal(t, runtime.done, "Run-owned scene Close completion")
	nha034WaitRunStoreClosed(t, rig.store)
	state, response := nha042ServerIntent(t, environment.observer, ids[0])
	if state != "result" || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || len(rig.driver.snapshot()) != 1 {
		t.Fatalf("shared scene ledger fabricated cancellation or repeat: %s %v", state, response.Status)
	}
}

func TestNHA042RunRejectsDifferentCommandDeclarationsBeforeStart(t *testing.T) {
	environment := nha042ServerEnvironmentFor(t)
	other, err := NewDurableCommandsComposition(DurableCommandsInput{Generation: nha094Generation{}, Clock: automationtriggers.RealClock{}})
	if err != nil {
		t.Fatal(err)
	}
	rig := nha042StartServer(t, environment, nha042ServerCommand, "", nha042ServerOptions{sharedJobs: true, change: func(plan *RuntimePlan) {
		plan.DurableCommands = other
	}})
	if err := rig.run.waitDone(t); !errors.Is(err, ErrInvalidTriggerComposition) {
		t.Fatalf("different private command declarations admitted: %v", err)
	}
	select {
	case <-rig.run.ready:
		t.Fatal("mismatched declarations reached serving")
	default:
	}
	if len(rig.driver.snapshot()) != 0 || rig.pilot.startCount() != 0 {
		t.Fatal("invalid declarations started the driver")
	}
	nha034WaitRunStoreClosed(t, rig.store)
}
