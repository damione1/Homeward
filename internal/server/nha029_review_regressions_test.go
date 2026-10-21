package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/helpers"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/reactivetemplate"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
)

func nha029ReviewQueuedScript(t *testing.T, body string) ir.Script {
	t.Helper()
	entity, err := ir.NewEntity(nha042ServerAlias, ir.ReferencePresent, ir.SupportSwitchTurnOn)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue([]ir.Entity{entity}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ir.ParseScriptID("script.nha042_child")
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err = catalogue.WithScripts([]ir.ScriptID{id})
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.NewSourceFile("nha029-review-queued.yaml", []byte("mode: queued\nmax: 4\nsequence:\n"+body))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := ir.CompileScriptSources("script.nha042_child", file.Path(), []parser.SourceFile{file}, catalogue)
	if err != nil || len(compiled.Accepted()) != 1 || len(compiled.Rejected()) != 0 {
		t.Fatalf("queued child admission: %v %+v", err, compiled.Rejected())
	}
	return compiled.Accepted()[0]
}

// Bind the fixture's real initial Controller to the authority already admitted
// by Run before its factory. This does not claim durable Logic1 compilation.
func nha029ReviewBoundPlan(t *testing.T, environment *nha042ServerEnvironment, automations []ir.Automation, scripts []ir.Script) automationruns.Plan {
	t.Helper()
	settings, err := environment.config.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	active, present, err := store.ReadActiveConfig(context.Background())
	if err != nil || !present {
		t.Fatalf("Run's SQL startup authority: %t %v", present, err)
	}
	stamp, err := automationruns.NewBoundProgram(active.Revision(), active.ManifestSHA256())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := automationruns.NewPlanWithScripts(automations, scripts)
	if err == nil {
		plan, err = plan.WithProvenance(stamp)
	}
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestNHA029ReviewQueuedChildCancellationPersistsCausalA(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	environment := nha042ServerEnvironmentFor(t)
	childBody := "- wait_template: '{{ false }}'\n" + nha042ServerCommand
	parentBody := "- action: script.turn_on\n  target: {entity_id: script.nha042_child}\n- wait_template: '{{ false }}'\n"
	r := nha029StartOwnerEnvironment(t, environment, parentBody, childBody, func(plan *RuntimePlan) {
		automations, scripts, err := nha042ServerPrograms(parentBody, childBody, false)
		if err != nil {
			t.Fatal(err)
		}
		scripts[1] = nha029ReviewQueuedScript(t, childBody)
		image := nha029ReviewBoundPlan(t, environment, automations, scripts)
		plan.AutomationRuns.plan = image
		plan.AutomationRuns.scripts.input.Plan = image
	})
	controller, original := r.controller(), r.composition.scripts.interpreter
	observation, err := controller.RetainNativeObservation(original)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observation.Release() })
	a := nha029C5HistoryActive(t, ctx, r)
	childID, _ := ir.ParseScriptID("script.nha042_child")
	active, err := controller.InvokeScript(ctx, childID, nil, automationruns.Run{}, ir.ScriptCallDirect)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := active.Startup(ctx); err != nil {
		t.Fatal(err)
	}
	parentID, _ := ir.ParseScriptID("script.nha042_parent")
	parent, err := controller.InvokeScript(ctx, parentID, nil, automationruns.Run{}, ir.ScriptCallDirect)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parent.Startup(ctx); err != nil {
		t.Fatal(err)
	}
	var queued automationruns.Trace
	nha042ServerWait(t, "actual A child queued behind active wait", func() bool {
		traces, _ := controller.Traces()
		for _, trace := range traces {
			if trace.Script == childID && trace.Decision == automationruns.DecisionQueued && trace.ParentID == parent.ID() {
				queued = trace
				return true
			}
		}
		return false
	})
	if queued.RunID == 0 || queued.RunID == active.ID() || queued.ParentID == 0 {
		t.Fatal("missing actual queued child/parent identities")
	}
	revision, manifest, bound := queued.Provenance.Bound()
	if !bound || revision != a.revision || manifest != a.manifest {
		t.Fatal("queued child did not capture actual A")
	}
	nextChild := nha029ReviewQueuedScript(t, "- stop: B_replaces_child\n")
	automations, scripts, err := nha042ServerPrograms(parentBody, childBody, false)
	if err != nil {
		t.Fatal(err)
	}
	scripts[1] = nextChild
	runs, err := automationruns.NewPlanWithScripts(automations, scripts)
	if err != nil {
		t.Fatal(err)
	}
	// Build B with the same real trigger and template plans as the fixture.
	r.pilot.mu.Lock()
	binding := r.pilot.binding
	r.pilot.mu.Unlock()
	_, target, err := binding.IDs()
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := domain.ParseHAAlias(nha042ServerAlias)
	triggers, err := automationtriggers.NewPlan(automationtriggers.PlanInput{Automations: automations, Scripts: scripts, Resolver: nha042ServerResolver{entities: map[domain.HAAlias]domain.EntityID{alias: target}}, Zone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	templates, err := (reactivetemplate.Plan{}).WithScriptWaits(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	base, err := configactivation.NewProgramImage(runs, triggers, templates, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.edit(t, "# R2 changes queued child while retaining its real parent ancestry\n")
	result, err := r.owner.Activate(ctx, r.request(t, &base, nil, nil))
	if err != nil || result.Outcome() != configactivation.Published {
		t.Fatalf("publish B: %v %v", result.Outcome(), err)
	}
	var terminal automationruns.Trace
	traces, _ := controller.Traces()
	for _, trace := range traces {
		if trace.RunID == queued.RunID && trace.Decision == automationruns.DecisionCancelled {
			terminal = trace
		}
	}
	if terminal.RunID != queued.RunID || terminal.ParentID != queued.ParentID || terminal.Provenance != queued.Provenance || terminal.Generation != queued.Generation {
		t.Fatalf("queued cancellation lost causal A: queued=%+v terminal=%+v", queued, terminal)
	}
	nodes, _ := original.NodeTraces()
	for _, node := range nodes {
		if node.RunID() == queued.RunID {
			t.Fatal("queued child executed before selective cancellation")
		}
	}
	if len(r.driver.snapshot()) != 0 {
		t.Fatal("queued cancellation dispatched a command")
	}
	epochText, err := r.identity.Sequence().CurrentAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := storage.ParseHistoryEpoch(epochText)
	if err != nil {
		t.Fatal(err)
	}
	r.close(t) // joins real history.Done before Store.Close
	if err := observation.Release(); err != nil {
		t.Fatal(err)
	}
	if ring, _ := original.NodeTraces(); len(ring) != 0 {
		t.Fatal("released native observation retained A")
	}
	settings, err := environment.config.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows, losses := nha186ReadAllWithCoverage(t, reopened, epoch)
	cancelled, linked := 0, false
	for _, row := range rows {
		if row.Kind == storage.HistoryScriptLink && row.RunID == uint64(queued.RunID) && row.ParentRunID == uint64(queued.ParentID) {
			linked = true
		}
		if row.Kind == storage.HistoryDecision && row.RunID == uint64(queued.RunID) && row.Decision == uint8(automationruns.DecisionCancelled) {
			cancelled++
			if row.ParentRunID != uint64(queued.ParentID) || row.ControllerGeneration != uint64(queued.Generation) || row.Provenance != storage.HistoryBoundProgram || row.ConfigRevision != a.revision.Number() || row.ManifestSHA256 != a.manifest {
				t.Fatalf("durable queued terminal substituted B or lost ancestry: %+v", row)
			}
		}
	}
	if cancelled != 1 || !linked {
		t.Fatalf("durable terminal/link missing: cancelled=%d linked=%t", cancelled, linked)
	}
	for _, loss := range losses {
		if loss.Reason == storage.HistoryLossInvalidMetadata {
			t.Fatalf("invalid-metadata loss after real sink drain: %+v", loss)
		}
	}
}

// The actual 040 scheduler callback is after transfer/join and before the
// final Owner stamp publication. Its release is explicit, never a sleep.
type nha029ReviewSun struct {
	entered, release chan struct{}
	once             sync.Once
}

func (s *nha029ReviewSun) NextSunset(ctx context.Context, _ time.Time, _ string, _ automationtriggers.SolarPosition) (automationtriggers.SunResult, error) {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
		return automationtriggers.NoSunEvent(), nil
	case <-ctx.Done():
		return automationtriggers.SunResult{}, ctx.Err()
	}
}

func nha029ReviewImage(t *testing.T, r *nha029OwnerRig, body, child string, templates reactivetemplate.Plan, sun *nha029ReviewSun) configactivation.ProgramImage {
	t.Helper()
	automations, scripts, err := nha042ServerPrograms(body, child, false)
	if err != nil {
		t.Fatal(err)
	}
	entity, err := ir.NewEntity("sun.sun", ir.ReferencePresent, ir.SupportObserveState)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue([]ir.Entity{entity}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.NewSourceFile("nha029-review-sun.yaml", []byte("id: nha029_review_sun\ntriggers:\n- trigger: sun\n  event: sunset\nactions:\n- stop: review_publication_barrier\n"))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := ir.CompileSources("automation.nha029_review_sun", file.Path(), []parser.SourceFile{file}, catalogue)
	if err != nil || len(compiled.Accepted()) != 1 || len(compiled.Rejected()) != 0 {
		t.Fatalf("review sun admission: %v %+v", err, compiled.Rejected())
	}
	automations = append(automations, compiled.Accepted()...)
	runs, err := automationruns.NewPlanWithScripts(automations, scripts)
	if err != nil {
		t.Fatal(err)
	}
	r.pilot.mu.Lock()
	binding := r.pilot.binding
	r.pilot.mu.Unlock()
	_, target, err := binding.IDs()
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := domain.ParseHAAlias(nha042ServerAlias)
	position, err := automationtriggers.NewSolarPosition(43, -79)
	if err != nil {
		t.Fatal(err)
	}
	triggers, err := automationtriggers.NewPlan(automationtriggers.PlanInput{Automations: automations, Scripts: scripts, Resolver: nha042ServerResolver{entities: map[domain.HAAlias]domain.EntityID{alias: target}}, Zone: "UTC", SunSource: sun, SolarPosition: position})
	if err != nil {
		t.Fatal(err)
	}
	image, err := configactivation.NewProgramImage(runs, triggers, templates, nil)
	if err != nil {
		t.Fatal(err)
	}
	return image
}

// This source simulates only a compiler-admitted boolean observation. It
// derives the real Helper capability from that definition and publishes via
// the actual Host/session; no alternate projection or command port is added.
type nha029ReviewHelperPilot struct {
	key        integrations.SourceKey
	definition helpers.Definition
	binding    integrations.Binding
	ready      chan struct{}
}

func (p *nha029ReviewHelperPilot) Start(ctx context.Context, source integrations.SessionHost, _ integrations.Tasks) error {
	description, err := helpers.Description(p.definition, scope.HelperButtonPolicy{})
	if err != nil {
		return err
	}
	candidate, err := integrations.NewCandidate(p.key, []*registrywire.CapabilityDescription{description})
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
	value, err := domain.NewHelperStateValue(domain.NewHelperBoolean(false), 1)
	if err != nil {
		return err
	}
	at := time.Now()
	observation, err := integrations.NewStateObservation("value", quality, &value, &at, at, nil)
	if err != nil {
		return err
	}
	if err := source.Publish(ctx, bindings[0], observation); err != nil {
		return err
	}
	p.binding = bindings[0]
	close(p.ready)
	return nil
}
func (*nha029ReviewHelperPilot) Stop(context.Context) error { return nil }

func TestNHA029ReviewDynamicAndDirectGroupWaitsJoinBeforePublication(t *testing.T) {
	for _, group := range []bool{false, true} {
		name := "resolved_dynamic_target"
		if group {
			name = "unchanged_direct_group_member"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			environment := nha042ServerEnvironmentFor(t)
			affectedAlias := "switch.basement_relay"
			if group {
				affectedAlias = "input_boolean.away_mode"
			}
			body := "- variables: {target: '" + affectedAlias + "'}\n- wait_template: \"{{ is_state(target, 'on') }}\"\n" + nha042ServerCommand
			if group {
				body = "- wait_template: \"{{ expand('group.nha029_review') | map(attribute='entity_id') | list }}\"\n" + nha042ServerCommand
			}
			// This dynamic target is resolved too, but its binding is unchanged.
			child := "- variables: {target: '" + nha042ServerAlias + "'}\n- wait_template: \"{{ is_state(target, 'on') }}\"\n- stop: intact_A_finished\n"
			var groups []bundle.Group
			var helperDefinition helpers.Definition
			if group {
				// The declarative profile permits only members declared in this
				// candidate. Use its admitted boolean, never an external switch.
				file, err := parser.NewSourceFile("nha029-review-groups.yaml", []byte("input_boolean:\n  away_mode:\n    name: Review boolean member\n    initial: false\ngroup:\n  nha029_review:\n    entities: [input_boolean.away_mode]\n"))
				if err != nil {
					t.Fatal(err)
				}
				candidate, err := bundle.CompileSources(file.Path(), []parser.SourceFile{file})
				if err != nil {
					t.Fatal(err)
				}
				definitions, err := helpers.Definitions(candidate)
				if err != nil || len(definitions) != 1 || len(candidate.Groups()) != 1 {
					t.Fatalf("real boolean/group admission: definitions=%d err=%v", len(definitions), err)
				}
				helperDefinition, groups = definitions[0], candidate.Groups()
			}
			templates, err := reactivetemplate.NewPlan(nil, groups, time.UTC)
			if err == nil {
				templates, err = templates.WithScriptWaits(time.UTC)
			}
			if err != nil {
				t.Fatal(err)
			}
			instance, _ := domain.ParseIntegrationInstanceID("nha029-review-affected-pilot")
			endpoint := "relay"
			if group {
				endpoint = "value"
			}
			key, _ := integrations.NewSourceKey("nha029-review-affected-member", endpoint)
			driver := &nha042ServerDriver{observer: environment.driverObserver, entered: make(chan struct{}), release: make(chan struct{})}
			driver.unblock()
			pilot := &nha042ServerPilot{key: key, driver: driver, clock: automationtriggers.RealClock{}, ready: make(chan struct{})}
			groupPilot := &nha029ReviewHelperPilot{key: key, definition: helperDefinition, ready: make(chan struct{})}
			ready := pilot.ready
			if group {
				ready = groupPilot.ready
			}
			r := nha029StartOwnerEnvironment(t, environment, body, child, func(plan *RuntimePlan) {
				if _, err := plan.Identity.(*registry.Repository).ConfigureBinding(ctx, instance, key, affectedAlias); err != nil {
					t.Fatal(err)
				}
				spec, err := integrations.NewPilotSpec(instance, func() integrations.Pilot {
					if group {
						return groupPilot
					}
					return pilot
				})
				if err != nil {
					t.Fatal(err)
				}
				plan.Pilots = append(plan.Pilots, spec)
				previous := plan.Configure
				plan.Configure = func(ctx context.Context, host *integrations.Manager, store *storage.Store) error {
					pilot.host = host
					return previous(ctx, host, store)
				}
				plan.ReactiveTemplates, err = NewReactiveTemplateComposition(templates, nha042NoPermanentOutput{}, reactivetemplate.RealClock{})
				if err != nil {
					t.Fatal(err)
				}
			})
			nha034WaitSignal(t, ready, "real affected fake binding A")
			original, controller := r.composition.scripts.interpreter, r.controller()
			authorityA := r.generation(t)
			parentID, _ := ir.ParseScriptID("script.nha042_parent")
			parent, err := controller.InvokeScript(ctx, parentID, nil, automationruns.Run{}, ir.ScriptCallDirect)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parent.Startup(ctx); err != nil {
				t.Fatal(err)
			}
			childID, _ := ir.ParseScriptID("script.nha042_child")
			intact, err := controller.InvokeScript(ctx, childID, nil, automationruns.Run{}, ir.ScriptCallDirect)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := intact.Startup(ctx); err != nil {
				t.Fatal(err)
			}
			var affectedWait, before automationruns.NodeTrace
			nha042ServerWait(t, "both actual 044 waits armed", func() bool {
				traces, _ := original.NodeTraces()
				a, b := false, false
				for _, trace := range traces {
					if trace.Kind() != ir.ActionWaitTemplate || trace.Stage() != automationruns.NodeWaiting {
						continue
					}
					if trace.RunID() == parent.ID() {
						affectedWait = trace
						a = true
					}
					if trace.RunID() == intact.ID() {
						before, b = trace, true
					}
				}
				return a && b
			})
			pilot.mu.Lock()
			oldBinding := pilot.binding
			pilot.mu.Unlock()
			if group {
				oldBinding = groupPilot.binding
			}
			_, retired, err := oldBinding.IDs()
			if err != nil {
				t.Fatal(err)
			}
			sun := &nha029ReviewSun{entered: make(chan struct{}), release: make(chan struct{})}
			var release sync.Once
			t.Cleanup(func() { release.Do(func() { close(sun.release) }) })
			image := nha029ReviewImage(t, r, body, child, templates, sun)
			r.edit(t, "# R1 retires one real binding; scripts and direct group remain identical\n")
			operation := nha029OwnerActivate(r, r.request(t, &image, nil, func(writer *registry.ConfigWriter) error { return writer.Tombstone(retired) }))
			nha034WaitSignal(t, sun.entered, "real 040 callback before final publication")
			retiredText, _ := retired.Value()
			var tombstoned bool
			if err := environment.observer.QueryRow(ctx, `SELECT tombstoned FROM public.registry_entities WHERE id=$1`, retiredText).Scan(&tombstoned); err != nil || !tombstoned {
				t.Fatalf("real target retirement did not commit: retired=%t err=%v", tombstoned, err)
			}
			sqlB, present, err := r.store.ReadActiveConfig(ctx)
			if err != nil || !present || uint64(sqlB.Revision().Number()) <= authorityA {
				t.Fatalf("barrier preceded durable B: present=%t err=%v", present, err)
			}
			// Tombstone is an admitted mutation. ConfigureBinding intentionally
			// forbids rebinding an allocated manifest alias; no fixture bypass.
			select {
			case <-parent.Done():
			default:
				t.Fatal("affected 044 run not joined before final B publication")
			}
			// A cooperatively cancelled wait returns flowStop with no execution
			// error. Its authoritative terminal decision and exact wait exit
			// distinguish cancellation from a successful or failed completion.
			if err := parent.Result(ctx); err != nil {
				t.Fatalf("affected wait failed instead of stopping cooperatively: %v", err)
			}
			if ctx.Err() != nil {
				t.Fatal("caller cancellation could explain the affected run terminal")
			}
			decisions, _ := controller.Traces()
			terminals := 0
			for _, trace := range decisions {
				if trace.RunID != parent.ID() {
					continue
				}
				switch trace.Decision {
				case automationruns.DecisionCancelled:
					if trace.Generation != affectedWait.Generation() || trace.Provenance != affectedWait.Provenance() || trace.ParentID != affectedWait.ParentID() {
						t.Fatal("configuration change cancelled a different run image or ancestry")
					}
					terminals++
				case automationruns.DecisionFinished, automationruns.DecisionExecutionError, automationruns.DecisionClosed:
					t.Fatalf("affected run has an incompatible terminal decision: %v", trace.Decision)
				}
			}
			if terminals != 1 {
				t.Fatalf("configuration-change join lacks one authoritative cancelled terminal: %d", terminals)
			}
			select {
			case <-intact.Done():
				t.Fatal("unrelated resolved dynamic wait cancelled")
			default:
			}
			after, _ := original.NodeTraces()
			cancelled, sameWait := false, 0
			for _, trace := range after {
				if trace.RunID() == parent.ID() && trace.Stage() == automationruns.NodeCancelled {
					if trace.Kind() != ir.ActionWaitTemplate || trace.Visit() != affectedWait.Visit() || trace.Generation() != affectedWait.Generation() || trace.Provenance() != affectedWait.Provenance() || trace.ParentID() != affectedWait.ParentID() || trace.WaitExit() != automationruns.WaitCancelled {
						t.Fatal("cancelled node does not identify the original A wait and its cancellation exit")
					}
					cancelled = true
				}
				if trace.RunID() == intact.ID() && trace.Stage() == automationruns.NodeCancelled {
					t.Fatal("intact A environment cancelled")
				}
				if trace.RunID() == intact.ID() && trace.Stage() == automationruns.NodeWaiting {
					if trace.Visit() != before.Visit() || trace.Generation() != before.Generation() || trace.Provenance() != before.Provenance() {
						t.Fatal("intact A wait recreated or rebound to B")
					}
					sameWait++
				}
			}
			if !cancelled || sameWait != 1 {
				t.Fatalf("selective transfer incomplete: cancelled=%t sameWait=%d", cancelled, sameWait)
			}
			if _, err := r.owner.CurrentConfig(ctx); !errors.Is(err, configactivation.ErrActivationUnavailable) {
				t.Fatalf("B stamp published before barrier: %v", err)
			}
			if len(r.driver.snapshot()) != 0 || len(driver.snapshot()) != 0 {
				t.Fatal("transfer dispatched a future command")
			}
			release.Do(func() { close(sun.release) })
			result, err := nha029OwnerAwait(t, operation)
			if err != nil || result.Outcome() != configactivation.Published {
				t.Fatalf("publish B after join: %v %v", result.Outcome(), err)
			}
			// A real B-era stimulus completes only the unchanged dynamic wait.
			r.pilot.mu.Lock()
			session, binding := r.pilot.session, r.pilot.binding
			r.pilot.mu.Unlock()
			quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
			value, at := domain.NewSwitchValue(true), time.Now()
			state, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := session.Publish(ctx, binding, state); err != nil {
				t.Fatal(err)
			}
			select {
			case <-intact.Done():
			case <-ctx.Done():
				t.Fatal("preserved A dynamic wait did not resume under B")
			}
			if err := intact.Result(ctx); err != nil {
				t.Fatalf("intact A terminal: %v", err)
			}
			if len(r.driver.snapshot()) != 0 || len(driver.snapshot()) != 0 {
				t.Fatal("cancelled affected run executed a later command")
			}
			var intents int64
			if err := environment.observer.QueryRow(ctx, `SELECT count(*) FROM public.command_intents`).Scan(&intents); err != nil || intents != 0 {
				t.Fatalf("selective transfer created a command intent: count=%d err=%v", intents, err)
			}
			r.close(t)
			if ring, _ := original.NodeTraces(); len(ring) != 0 {
				t.Fatal("completed A retained its image ring")
			}
		})
	}
}

func TestNHA029ReviewHealthFollowsOnlyPublishedSQLGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r := nha029StartOwner(t, nha029OwnerParent, nha029OwnerChild, nil)
	r.waitBoth(t)
	if r.observation == nil {
		t.Fatal("actual Run observation owner was not retained")
	}
	original := r.composition.scripts.interpreter
	waits := nha029C5HistoryWaits(t, original)
	assertHealth := func(revision storage.ConfigGenerationRevision) {
		t.Helper()
		facts, err := r.observation.health.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		server, err := facts.Server()
		if err != nil {
			t.Fatal(err)
		}
		if server.Readiness != obs.ServerReadiness_SERVER_READINESS_READY || server.Generation.GetPresent().GetValue() != uint64(revision.Number()) {
			t.Fatalf("health is not the actually published generation: %+v", server)
		}
	}
	a := nha029C5HistoryActive(t, ctx, r)
	assertHealth(a.revision)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	r.edit(t, "# B health publication waits for real SQL commit\n")
	operation := nha029OwnerActivate(r, r.request(t, nil, nil, func(*registry.ConfigWriter) error {
		close(entered)
		<-release
		return nil
	}))
	nha034WaitSignal(t, entered, "real B transaction before commit")
	if visible := nha029C5HistoryActive(t, ctx, r); visible.revision != a.revision || visible.manifest != a.manifest {
		t.Fatal("B escaped its transaction before commit")
	}
	assertHealth(a.revision)
	releaseOnce.Do(func() { close(release) })
	bResult, err := nha029OwnerAwait(t, operation)
	if err != nil || bResult.Outcome() != configactivation.Published {
		t.Fatalf("B publication: %v %v", bResult.Outcome(), err)
	}
	b := nha029C5HistoryActive(t, ctx, r)
	if b.revision != bResult.Revision() || b.revision.Number() <= a.revision.Number() || b.manifest == a.manifest {
		t.Fatal("B health oracle lacks a new real SQL generation")
	}
	assertHealth(b.revision)
	r.edit(t, "# C health publication advances after B\n")
	cResult, err := r.owner.Activate(ctx, r.request(t, nil, nil, nil))
	if err != nil || cResult.Outcome() != configactivation.Published {
		t.Fatalf("C publication: %v %v", cResult.Outcome(), err)
	}
	c := nha029C5HistoryActive(t, ctx, r)
	if c.revision != cResult.Revision() || c.revision.Number() <= b.revision.Number() || c.manifest == b.manifest {
		t.Fatal("C health oracle lacks a later real SQL generation")
	}
	assertHealth(c.revision)
	r.edit(t, "# rejected D must not publish health\n")
	refusal := errors.New("nha029-health-precommit-refusal")
	dResult, err := r.owner.Activate(ctx, r.request(t, nil, nil, func(*registry.ConfigWriter) error { return refusal }))
	if err == nil || dResult.Outcome() != configactivation.NotCommitted || dResult.AttemptOutcome() != storage.TransactionNotCommitted {
		t.Fatalf("D did not roll back before publication: %v %v", dResult.Outcome(), err)
	}
	if visible := nha029C5HistoryActive(t, ctx, r); visible.revision != c.revision || visible.manifest != c.manifest {
		t.Fatal("precommit refusal changed SQL authority")
	}
	assertHealth(c.revision)
	if nha029C5HistoryWaits(t, original) != waits || len(r.driver.snapshot()) != 0 {
		t.Fatal("health publication disturbed intact A waits or sent a command")
	}
	r.close(t)
	facts, err := r.observation.health.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	server, err := facts.Server()
	if err != nil || server.Readiness != obs.ServerReadiness_SERVER_READINESS_DRAINING || server.Generation.GetPresent().GetValue() != uint64(c.revision.Number()) {
		t.Fatalf("shutdown lost the published generation or restored Ready: %+v %v", server, err)
	}
}
