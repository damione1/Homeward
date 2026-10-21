package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/compat/ha/template"
	statewire "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/reactivetemplate"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/view"
)

const nha029OwnerParent = "- action: script.turn_on\n  target: {entity_id: script.nha042_child}\n  data: {}\n- wait_template: '{{ false }}'\n"
const nha029OwnerChild = "- wait_for_trigger:\n  - trigger: event\n    event_type: nha042_release\n" + nha042ServerCommand

type nha029OwnerRig struct {
	observation *observationRuntime
	*nha042ServerRig
	environment *nha042ServerEnvironment
	owner       *configactivation.Owner
	path        string
	raw         []byte
	views       *view.Service
}

// This is the existing Run/PG/fake-driver fixture with a physical workspace
// and the actual activation hook. It creates no alternate execution engine.
func nha029StartOwner(t *testing.T, body, child string, change func(*RuntimePlan)) *nha029OwnerRig {
	t.Helper()
	environment := nha042ServerEnvironmentFor(t)
	return nha029StartOwnerEnvironment(t, environment, body, child, change)
}
func nha029StartOwnerEnvironment(t *testing.T, environment *nha042ServerEnvironment, body, child string, change func(*RuntimePlan), sharedJobs ...bool) *nha029OwnerRig {
	t.Helper()
	if len(sharedJobs) > 1 {
		t.Fatal("multiple shared command fixture choices")
	}
	shareCommands := len(sharedJobs) == 1 && sharedJobs[0]
	source, err := environment.config.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	raw := source.Bytes()
	path := filepath.Join(t.TempDir(), "configuration.yaml")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	environment.config.startup = startup
	result := &nha029OwnerRig{environment: environment, path: path, raw: raw}
	ownerReady := make(chan *configactivation.Owner, 1)
	result.nha042ServerRig = nha042StartServer(t, environment, body, child, nha042ServerOptions{sharedJobs: shareCommands, observeRuntime: func(actual *observationRuntime) { result.observation = actual }, change: func(plan *RuntimePlan) {
		identity := plan.Identity.(*nha044Identity)
		// Production uses the actual SQL repository. Preserve only the old
		// fixture's read-only observation of the actual sequencer for its helpers.
		plan.Identity = identity.repo
		plan.ConfigureAuthorization = func(ctx context.Context, composition AuthorizationComposition) error {
			actual := composition.(runtimeAuthorizationComposition)
			identity.mu.Lock()
			identity.sequence = actual.views.source
			identity.mu.Unlock()
			var err error
			// Run forces the configuration authority to its real Owner.
			result.views, err = composition.NewViewService(nil, sequencer.RealClock{})
			return err
		}
		plan.ConfigureActivation = func(_ context.Context, owner *configactivation.Owner) error { ownerReady <- owner; return nil }
		if change != nil {
			change(plan)
		}
	}})
	result.run.waitReady(t)
	nha034WaitSignal(t, result.pilot.ready, "owner fake pilot startup")
	select {
	case result.owner = <-ownerReady:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not expose its actual activation owner")
	}
	return result
}

func (r *nha029OwnerRig) request(t *testing.T, image *configactivation.ProgramImage, policies []configactivation.PolicyChange, mutate func(*registry.ConfigWriter) error) configactivation.Request {
	t.Helper()
	author, err := storage.NewConfigAuthor("nha029:trusted-test-composition")
	if err != nil {
		t.Fatal(err)
	}
	source, err := storage.NewConfigSource("nha029:physical-workspace")
	if err != nil {
		t.Fatal(err)
	}
	request, err := configactivation.NewWorkspaceRequest(configactivation.RetainDefinitions(), author, source, policies, image, mutate)
	if err != nil {
		t.Fatal(err)
	}
	return request
}
func (r *nha029OwnerRig) edit(t *testing.T, suffix string) {
	t.Helper()
	if err := os.WriteFile(r.path, append(append([]byte(nil), r.raw...), []byte(suffix)...), 0600); err != nil {
		t.Fatal(err)
	}
}
func (r *nha029OwnerRig) controller() *automationruns.Controller {
	r.composition.mu.Lock()
	defer r.composition.mu.Unlock()
	return r.composition.runtime
}
func (r *nha029OwnerRig) image(t *testing.T, body, child string, replacement ...reactivetemplate.Plan) configactivation.ProgramImage {
	t.Helper()
	automations, scripts, err := nha042ServerPrograms(body, child, false)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := automationruns.NewPlanWithScripts(automations, scripts)
	if err != nil {
		t.Fatal(err)
	}
	r.pilot.mu.Lock()
	binding := r.pilot.binding
	r.pilot.mu.Unlock()
	_, entity, err := binding.IDs()
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := domain.ParseHAAlias(nha042ServerAlias)
	triggers, err := automationtriggers.NewPlan(automationtriggers.PlanInput{Automations: automations, Scripts: scripts, Resolver: nha042ServerResolver{entities: map[domain.HAAlias]domain.EntityID{alias: entity}}, Zone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	templates, err := (reactivetemplate.Plan{}).WithScriptWaits(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(replacement) > 1 {
		t.Fatal("multiple template replacements")
	}
	if len(replacement) == 1 {
		templates = replacement[0]
	}
	image, err := configactivation.NewProgramImage(runs, triggers, templates, nil)
	if err != nil {
		t.Fatal(err)
	}
	return image
}
func (r *nha029OwnerRig) generation(t *testing.T) uint64 {
	t.Helper()
	stamp, err := r.owner.CurrentConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	revision, _, err := stamp.Components()
	if err != nil {
		t.Fatal(err)
	}
	return revision
}
func (r *nha029OwnerRig) waitBoth(t *testing.T) {
	t.Helper()
	r.pilot.emit(t, "nha042_start")
	nha042ServerWait(t, "causal A 040 and 044 waits", func() bool {
		return nha042ServerNode(r.composition, "script.nha042_parent", ir.ActionWaitTemplate, automationruns.NodeWaiting) && nha042ServerNode(r.composition, "script.nha042_child", ir.ActionWait, automationruns.NodeWaiting)
	})
}
func (r *nha029OwnerRig) close(t *testing.T) {
	t.Helper()
	r.run.cancel()
	if err := r.run.waitDone(t); err != nil {
		t.Fatal(err)
	}
	nha034WaitSignal(t, r.owner.Done(), "actual activation owner joined")
	nha034WaitRunStoreClosed(t, r.store)
}
func nha029OwnerAwait(t *testing.T, result <-chan struct {
	result configactivation.Result
	err    error
}) (configactivation.Result, error) {
	t.Helper()
	select {
	case outcome := <-result:
		return outcome.result, outcome.err
	case <-time.After(10 * time.Second):
		t.Fatal("activation did not join")
		return configactivation.Result{}, context.DeadlineExceeded
	}
}
func nha029OwnerActivate(r *nha029OwnerRig, request configactivation.Request) <-chan struct {
	result configactivation.Result
	err    error
} {
	result := make(chan struct {
		result configactivation.Result
		err    error
	}, 1)
	go func() {
		value, err := r.owner.Activate(context.Background(), request)
		result <- struct {
			result configactivation.Result
			err    error
		}{value, err}
	}()
	return result
}

// Both waits are actual services in Run. Rename keeps the old alias resolving
// to the same entity; explicit human policy revocation cannot own/cancel this
// trusted household script. Its old 040 wait finishes while SQL is in flight.
func TestNHA029OwnerRunOperationalOnlyPreservesCausalWaitsAndPausesNewOccurrence(t *testing.T) {
	r := nha029StartOwner(t, nha029OwnerParent, nha029OwnerChild, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	before := r.generation(t)
	r.waitBoth(t)
	original := r.composition.scripts.interpreter
	copied, _ := original.NodeTraces()
	r.pilot.mu.Lock()
	binding := r.pilot.binding
	r.pilot.mu.Unlock()
	_, entity, err := binding.IDs()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := r.store.GetAuthorizationPolicy(ctx, r.environment.account, r.environment.account)
	if err != nil {
		t.Fatal(err)
	}
	oldVersion, err := policy.Version()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := authorization.ExpectVersion(oldVersion)
	if err != nil {
		t.Fatal(err)
	}
	account, _ := r.environment.account.Value()
	source, err := authorization.AdmitPolicySource(r.environment.account, []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants: []\n", account)))
	if err != nil {
		t.Fatal(err)
	}
	change, err := configactivation.NewPolicyChange(source, expected)
	if err != nil {
		t.Fatal(err)
	}
	r.edit(t, "# operational-only B\n")
	entered, release := make(chan struct{}), make(chan struct{})
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	request := r.request(t, nil, []configactivation.PolicyChange{change}, func(writer *registry.ConfigWriter) error {
		if err := writer.Rename(entity, "switch.nha029_display_name"); err != nil {
			return err
		}
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	outcome := nha029OwnerActivate(r, request)
	nha034WaitSignal(t, entered, "real ConfigTx Registry callback")
	if _, err := r.owner.CurrentConfig(ctx); !errors.Is(err, configactivation.ErrActivationUnavailable) {
		t.Fatalf("open stamp during SQL cut: %v", err)
	}
	if _, err := r.identity.Sequence().Capture(); err == nil {
		t.Fatal("external capture exposed a partial publication")
	}
	var alias string
	entityText, _ := entity.Value()
	if err := r.environment.observer.QueryRow(ctx, `SELECT alias FROM public.registry_aliases WHERE entity_id=$1 AND current`, entityText).Scan(&alias); err != nil || alias != nha042ServerAlias {
		t.Fatalf("uncommitted rename escaped SQL: %q %v", alias, err)
	}
	id, _ := ir.ParseScriptID("script.nha042_parent")
	short, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	if _, err := r.controller().InvokeScript(short, id, nil, automationruns.Run{}, ir.ScriptCallDirect); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("new occurrence failed to wait cancellably before capture: %v", err)
	}
	stop()
	r.pilot.emit(t, "nha042_release")
	nha042ServerWait(t, "old 040 wait finished during SQL", func() bool {
		return nha042ServerNode(r.composition, "script.nha042_child", ir.ActionCall, automationruns.NodeEntered)
	})
	if len(r.driver.snapshot()) != 0 {
		t.Fatal("paused action dispatched before COMMIT")
	}
	close(release)
	released = true
	result, err := nha029OwnerAwait(t, outcome)
	if err != nil || result.Outcome() != configactivation.Published || result.AttemptOutcome() != storage.TransactionCommitted || uint64(result.Revision().Number()) <= before {
		t.Fatalf("coherent publication: outcome=%v transaction=%v revision=%v err=%v", result.Outcome(), result.AttemptOutcome(), result.Revision(), err)
	}
	nha042ServerWait(t, "same A wait dispatched exactly once after B", func() bool { return len(r.driver.snapshot()) == 1 })
	if r.pilot.startCount() != 1 {
		t.Fatal("intact fake pilot restarted")
	}
	after, _ := original.NodeTraces()
	if !slices.ContainsFunc(after, func(trace automationruns.NodeTrace) bool {
		return trace.Source().Object() == "script.nha042_parent" && trace.Stage() == automationruns.NodeWaiting
	}) || len(copied) == 0 {
		t.Fatal("unchanged A parent lost its actual 044 wait")
	}
	durable, found, err := r.store.ReadActivationAttempt(ctx, result.Attempt())
	if err != nil || !found || durable.Target() != result.Revision() {
		t.Fatalf("exact transaction attempt missing: %t %v", found, err)
	}
	active, found, err := r.store.ReadActiveConfig(ctx)
	if err != nil || !found || active.Revision() != result.Revision() {
		t.Fatalf("SQL authority differs from stamp: %t %v", found, err)
	}
	if uint64(active.Revision().Number()) != r.generation(t) {
		t.Fatal("visible generation differs from SQL")
	}
	policyAfter, err := r.store.GetAuthorizationPolicy(ctx, r.environment.account, r.environment.account)
	if err != nil {
		t.Fatal(err)
	}
	version, err := policyAfter.Version()
	if err != nil {
		t.Fatal(err)
	}
	oldRevision, _, _ := oldVersion.Values()
	newRevision, _, _ := version.Values()
	if newRevision <= oldRevision {
		t.Fatal("human policy did not commit with the activation")
	}
	next, err := r.controller().InvokeScript(ctx, id, nil, automationruns.Run{}, ir.ScriptCallDirect)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := next.Startup(ctx); err != nil {
		t.Fatal(err)
	}
	traces, _ := r.composition.NodeTraces()
	if !slices.ContainsFunc(traces, func(trace automationruns.NodeTrace) bool {
		return trace.RunID() == next.ID() && trace.Generation() > copied[0].Generation()
	}) {
		t.Fatal("new occurrence did not capture B")
	}
	// Copy before shutdown: A's real parent owns the ring until this point.
	finalA := append([]automationruns.NodeTrace(nil), after...)
	r.close(t)
	if len(finalA) == 0 {
		t.Fatal("lost copied A observations")
	}
	if released, _ := original.NodeTraces(); len(released) != 0 {
		t.Fatal("released A ring was retained outside its pool")
	}
}

func TestNHA029OwnerRunSelectivelyJoinsChangedParentAndKeepsDetachedAChild(t *testing.T) {
	r := nha029StartOwner(t, nha029OwnerParent, nha029OwnerChild, nil)
	r.waitBoth(t)
	original := r.composition.scripts.interpreter
	before, _ := original.NodeTraces()
	var parent, child automationruns.RunID
	for _, trace := range before {
		if trace.Source().Object() == "script.nha042_parent" {
			parent = trace.RunID()
		}
		if trace.Source().Object() == "script.nha042_child" {
			child = trace.RunID()
		}
	}
	if parent == 0 || child == 0 || parent == child {
		t.Fatal("missing independent causal runs")
	}
	nextBody := nha042ServerCommand + nha029OwnerParent
	image := r.image(t, nextBody, nha029OwnerChild)
	r.edit(t, "# B changes only parent behavior\n")
	result, err := r.owner.Activate(context.Background(), r.request(t, &image, nil, nil))
	if err != nil || result.Outcome() != configactivation.Published {
		t.Fatalf("publish selective B: %v %v", result.Outcome(), err)
	}
	after, _ := original.NodeTraces()
	if !slices.ContainsFunc(after, func(trace automationruns.NodeTrace) bool {
		return trace.RunID() == parent && trace.Stage() == automationruns.NodeCancelled
	}) {
		t.Fatal("changed parent was not actually canceled/joined")
	}
	for _, trace := range after {
		if trace.RunID() == child && trace.Stage() == automationruns.NodeCancelled {
			t.Fatal("intact detached A child canceled")
		}
	}
	// The child's real image reference, not a test hold, owns these snapshots.
	r.pilot.emit(t, "nha042_release")
	nha042ServerWait(t, "preserved A child action after B", func() bool { return len(r.driver.snapshot()) == 1 })
	r.pilot.emit(t, "nha042_start")
	nha042ServerWait(t, "new B parent's own action", func() bool { return len(r.driver.snapshot()) == 2 })
	if r.pilot.startCount() != 1 {
		t.Fatal("selective publication restarted intact pilot")
	}
	copied := append([]automationruns.NodeTrace(nil), after...)
	r.close(t)
	if len(copied) <= len(before) {
		t.Fatal("terminal A observations were not copied before final release")
	}
}

func TestNHA029OwnerRunPhysicalEditAtCASRollsBackWithoutCancel(t *testing.T) {
	r := nha029StartOwner(t, nha029OwnerParent, nha029OwnerChild, nil)
	r.waitBoth(t)
	old := r.generation(t)
	r.edit(t, "# prepared B\n")
	entered, release := make(chan struct{}), make(chan struct{})
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	result := nha029OwnerActivate(r, r.request(t, nil, nil, func(*registry.ConfigWriter) error { close(entered); <-release; return nil }))
	nha034WaitSignal(t, entered, "SQL transaction in flight")
	r.edit(t, "# C edit arrived before B CAS\n")
	close(release)
	released = true
	outcome, err := nha029OwnerAwait(t, result)
	if err == nil || outcome.Outcome() != configactivation.NotCommitted || outcome.AttemptOutcome() != storage.TransactionNotCommitted {
		t.Fatalf("physical CAS accepted stale B: %v %v", outcome.Outcome(), err)
	}
	if r.generation(t) != old {
		t.Fatal("failed CAS changed visible generation")
	}
	if _, found, err := r.store.ReadActivationAttempt(context.Background(), outcome.Attempt()); err != nil || found {
		t.Fatalf("rolled back attempt persisted: %t %v", found, err)
	}
	observations, _ := r.composition.NodeTraces()
	for _, trace := range observations {
		if trace.Stage() == automationruns.NodeCancelled {
			t.Fatal("precommit refusal canceled a live A run")
		}
	}
	r.pilot.emit(t, "nha042_release")
	nha042ServerWait(t, "same A resumed after refusal", func() bool { return len(r.driver.snapshot()) == 1 })
	r.close(t)
}

// This second SQL producer uses the public protected-source/ConfigTx format.
// It does not publish a runtime and never invents Logic1 persistence. The
// fixture deliberately has no declarative source to retain or reconstruct.
func (r *nha029OwnerRig) commitCurrentWorkspace(t *testing.T) storage.ConfigGenerationRevision {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	admitted, _, err := configactivation.ReconcileStartup(ctx, r.store, r.environment.config.startup)
	if err != nil {
		t.Fatal(err)
	}
	if _, hasLegacy := admitted.LegacyCandidate(); hasLegacy {
		t.Fatal("concurrent writer fixture unexpectedly has a declarative bundle")
	}
	secrets, err := admitted.Secrets()
	if err != nil {
		t.Fatal(err)
	}
	replay, err := storage.NewConfigReplayRepository(r.store, secrets)
	if err != nil {
		t.Fatal(err)
	}
	captured, err := r.environment.config.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	document, err := storage.NewConfigDocumentID("operational.main")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := storage.NewConfigDocumentBinding(document, captured.LogicalPath())
	if err != nil {
		t.Fatal(err)
	}
	author, err := storage.NewConfigAuthor("nha029:concurrent-SQL-producer")
	if err != nil {
		t.Fatal(err)
	}
	source, err := storage.NewConfigSource("nha029:current-workspace")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := storage.NewConfigVersionMetadata(author, source)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := storage.ExpectConfigActive(admitted.Revision(), admitted.ManifestSHA256())
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := storage.RandomActivationAttemptID()
	if err != nil {
		t.Fatal(err)
	}
	// Compiled-operational/1's documented source-only branch. Real replay
	// independently authenticates and reparses this exact protected source.
	hash := sha256.New()
	hash.Write([]byte("homeward.compiled-operational/1\x00"))
	sourceHash := captured.SHA256()
	hash.Write(sourceHash[:])
	hash.Write([]byte{0})
	var compiled [32]byte
	copy(compiled[:], hash.Sum(nil))
	var target storage.ConfigGenerationRevision
	outcome, err := r.store.InConfigTransactionOutcome(ctx, func(tx *storage.ConfigTx) error {
		ref, err := tx.SaveProtectedOperationalSource(replay, binding, captured.Bytes(), captured.SHA256(), metadata)
		if err != nil {
			return err
		}
		target, err = tx.RegisterCompiledGeneration(captured.LogicalPath(), compiled, []storage.ConfigVersionRef{ref})
		if err != nil {
			return err
		}
		if err := captured.ConfirmPhysicalSource(); err != nil {
			return err
		}
		if err := tx.ActivateGeneration(expected, target); err != nil {
			return err
		}
		return tx.RecordActivationAttempt(attempt, metadata)
	})
	if err != nil || outcome != storage.TransactionCommitted {
		t.Fatalf("concurrent C did not commit: %v %v", outcome, err)
	}
	return target
}

// The protocol proxy forwards actual PostgreSQL frames and withholds the
// completed COMMIT (or completed ROLLBACK) reply. C then durably supersedes
// the uncertain B before the actual Run owner reconciles. No fake SQL error,
// replacement runtime or replayed action manufactures this uncertainty.
func TestNHA029OwnerRunLostACKReconcilesCurrentSQLWinnerWithoutWriteOrReplay(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(fmt.Sprintf("B_rolled_back_%t", rollback), func(t *testing.T) {
			environment := nha042ServerEnvironmentFor(t)
			backendPort := int(environment.observer.Config().Port)
			proxy := nha029OwnerStartCommitACKProxy(t, backendPort)
			proxy.abort = rollback
			captured, err := environment.config.startup.CaptureWorkspace()
			if err != nil {
				t.Fatal(err)
			}
			raw := strings.Replace(string(captured.Bytes()), "  port: "+strconv.Itoa(backendPort)+"\n", "  port: "+strconv.Itoa(proxy.port(t))+"\n", 1)
			if raw == string(captured.Bytes()) {
				t.Fatal("proxy port did not replace loopback PostgreSQL locator")
			}
			environment.config = nha029RunFixtureFromRaw(t, []byte(raw))
			r := nha029StartOwnerEnvironment(t, environment, nha029OwnerParent, nha029OwnerChild, nil)
			before := r.generation(t)
			r.waitBoth(t)
			r.edit(t, "# uncertain B\n")
			proxy.armed.Store(true)
			unresolved := nha029OwnerActivate(r, r.request(t, nil, nil, nil))
			nha034WaitSignal(t, proxy.held, "real completed PostgreSQL transaction ACK withheld")
			r.edit(t, "# durable C wins before reconcile\n")
			winner := r.commitCurrentWorkspace(t)
			if uint64(winner.Number()) <= before {
				t.Fatal("C did not advance SQL authority")
			}
			proxy.allowDrop()
			uncertain, err := nha029OwnerAwait(t, unresolved)
			if uncertain.Outcome() != configactivation.CommitUnknown || err == nil {
				t.Fatalf("lost ACK did not preserve quarantine: %v %v", uncertain.Outcome(), err)
			}
			if _, err := r.owner.CurrentConfig(context.Background()); !errors.Is(err, configactivation.ErrActivationUnavailable) {
				t.Fatalf("unknown outcome reopened A: %v", err)
			}
			if _, err := r.identity.Sequence().Capture(); err == nil {
				t.Fatal("unknown outcome reopened capture")
			}
			// Finish an intact old wait while still closed. Its command cannot claim
			// a SQL ID until the coherent C publication opens the same DCS.
			r.pilot.emit(t, "nha042_release")
			nha042ServerWait(t, "A waited command blocked under quarantine", func() bool {
				return nha042ServerNode(r.composition, "script.nha042_child", ir.ActionCall, automationruns.NodeEntered)
			})
			if len(r.driver.snapshot()) != 0 {
				t.Fatal("uncertain activation dispatched a command")
			}
			var generationsBefore, attemptsBefore int
			if err := environment.observer.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM public.config_generations),(SELECT count(*) FROM public.config_activation_attempts)`).Scan(&generationsBefore, &attemptsBefore); err != nil {
				t.Fatal(err)
			}
			result, err := r.owner.Reconcile(context.Background())
			if err != nil || result.Outcome() != configactivation.Published || result.Revision() != winner {
				t.Fatalf("reconcile did not resume current C: %v revision=%v winner=%v err=%v", result.Outcome(), result.Revision(), winner, err)
			}
			expectedOutcome := storage.TransactionCommitted
			if rollback {
				expectedOutcome = storage.TransactionNotCommitted
			}
			if result.AttemptOutcome() != expectedOutcome {
				t.Fatalf("attempt B status confused with C publication: %v", result.AttemptOutcome())
			}
			if r.generation(t) != uint64(winner.Number()) {
				t.Fatal("visible stamp did not resume C")
			}
			nha042ServerWait(t, "original A waited command after C", func() bool { return len(r.driver.snapshot()) == 1 })
			var generationsAfter, attemptsAfter int
			if err := environment.observer.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM public.config_generations),(SELECT count(*) FROM public.config_activation_attempts)`).Scan(&generationsAfter, &attemptsAfter); err != nil {
				t.Fatal(err)
			}
			if generationsAfter != generationsBefore || attemptsAfter != attemptsBefore {
				t.Fatal("reconcile performed another configuration write")
			}
			if r.pilot.startCount() != 1 {
				t.Fatal("reconcile restarted an intact pilot")
			}
			r.close(t)
			if len(r.driver.snapshot()) != 1 {
				t.Fatal("reconcile replayed or inverted the waited action")
			}
		})
	}
}

func TestNHA029OwnerRunKeepsStoreUntilSQLCallbackReallyLeaves(t *testing.T) {
	r := nha029StartOwner(t, nha029OwnerParent, nha029OwnerChild, nil)
	r.waitBoth(t)
	r.edit(t, "# shutdown while SQL is owned\n")
	entered, release := make(chan struct{}), make(chan struct{})
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	operation := nha029OwnerActivate(r, r.request(t, nil, nil, func(*registry.ConfigWriter) error {
		close(entered)
		<-release // deliberately ignores cancellation; Done must mean actual exit
		return nil
	}))
	nha034WaitSignal(t, entered, "actual SQL callback owns transaction")
	r.run.cancel()
	if err := r.run.waitDone(t); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run did not return its shared close deadline: %v", err)
	}
	select {
	case <-r.owner.Done():
		t.Fatal("Owner.Done preceded actual SQL callback exit")
	default:
	}
	if err := nha034ProbeRunStore(r.store); err != nil {
		t.Fatalf("Store closed while SQL callback still owned it: %v", err)
	}
	close(release)
	released = true
	result, err := nha029OwnerAwait(t, operation)
	if err == nil || result.Outcome() != configactivation.NotCommitted {
		t.Fatalf("shutdown restored or published canceled transaction: %v %v", result.Outcome(), err)
	}
	nha034WaitSignal(t, r.owner.Done(), "Owner.Done after real SQL join")
	nha034WaitRunStoreClosed(t, r.store)
	if len(r.driver.snapshot()) != 0 {
		t.Fatal("shutdown replayed an action")
	}
}

type nha029Forwarded struct {
	text                    string
	publication, generation uint64
}
type nha029OwnerOutput struct {
	owner       atomic.Pointer[configactivation.Owner]
	entered     chan struct{}
	release     chan struct{}
	once        sync.Once
	unblockOnce sync.Once
	mu          sync.Mutex
	updates     []nha029Forwarded
}

func (o *nha029OwnerOutput) unblock() { o.unblockOnce.Do(func() { close(o.release) }) }
func (o *nha029OwnerOutput) PublishReactiveTemplate(ctx context.Context, _ *integrations.Manager, update reactivetemplate.Update) error {
	result, present := update.CurrentResult()
	if !present {
		return nil
	}
	first := false
	o.once.Do(func() { first = true; close(o.entered) })
	if first {
		select {
		case <-o.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var revision uint64
	if owner := o.owner.Load(); owner != nil {
		stamp, err := owner.CurrentConfig(ctx)
		if err == nil {
			revision, _, err = stamp.Components()
			if err != nil {
				return err
			}
		} else if !first {
			return err
		}
	}
	o.mu.Lock()
	o.updates = append(o.updates, nha029Forwarded{result.Text(), update.Publication(), revision})
	o.mu.Unlock()
	return nil
}
func (o *nha029OwnerOutput) snapshot() []nha029Forwarded {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]nha029Forwarded(nil), o.updates...)
}
func nha029PermanentTemplate(t *testing.T, text string) reactivetemplate.Plan {
	t.Helper()
	program, err := template.Compile(text)
	if err != nil {
		t.Fatal(err)
	}
	id, err := reactivetemplate.ParseProgramID("nha029-forwarded")
	if err != nil {
		t.Fatal(err)
	}
	definition, err := reactivetemplate.NewDefinition(id, program, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := reactivetemplate.NewPlan([]reactivetemplate.Definition{definition}, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = plan.WithScriptWaits(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestNHA029OwnerRunJoinsDequeuedAOutputBeforePublishingBStamp(t *testing.T) {
	output := &nha029OwnerOutput{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(output.unblock)
	a := nha029PermanentTemplate(t, "A")
	r := nha029StartOwner(t, nha029OwnerParent, nha029OwnerChild, func(plan *RuntimePlan) {
		composition, err := NewReactiveTemplateComposition(a, output, reactivetemplate.RealClock{})
		if err != nil {
			t.Fatal(err)
		}
		plan.ReactiveTemplates = composition
	})
	// Run's cleanup was registered later, so unblock before its actual drain.
	t.Cleanup(output.unblock)
	output.owner.Store(r.owner)
	nha034WaitSignal(t, output.entered, "actual pump removed A from its existing queue")
	r.waitBoth(t)
	before := r.generation(t)
	b := nha029PermanentTemplate(t, "B")
	image := r.image(t, nha029OwnerParent, nha029OwnerChild, b)
	r.edit(t, "# B permanent template output\n")
	operation := nha029OwnerActivate(r, r.request(t, &image, nil, nil))
	nha042ServerWait(t, "B committed while A callback remains dequeued", func() bool {
		var revision uint64
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return r.environment.observer.QueryRow(ctx, `SELECT generation_revision FROM public.config_active WHERE singleton`).Scan(&revision) == nil && revision > before
	})
	if _, err := r.owner.CurrentConfig(context.Background()); !errors.Is(err, configactivation.ErrActivationUnavailable) {
		t.Fatalf("B stamp preceded actual A callback join: %v", err)
	}
	select {
	case result := <-operation:
		t.Fatalf("publication returned before actual callback exit: %v %v", result.result.Outcome(), result.err)
	default:
	}
	output.unblock()
	result, err := nha029OwnerAwait(t, operation)
	if err != nil || result.Outcome() != configactivation.Published {
		t.Fatalf("drained output publication: %v %v", result.Outcome(), err)
	}
	nha042ServerWait(t, "B output under coherent B stamp", func() bool {
		return slices.ContainsFunc(output.snapshot(), func(update nha029Forwarded) bool {
			return update.text == "B" && update.generation == uint64(result.Revision().Number()) && update.publication > 0
		})
	})
	for _, update := range output.snapshot() {
		if update.text == "A" && update.generation == uint64(result.Revision().Number()) {
			t.Fatal("already dequeued A was forwarded as B")
		}
	}
	r.pilot.emit(t, "nha042_release")
	nha042ServerWait(t, "unchanged A wait remained live across 044 replacement", func() bool { return len(r.driver.snapshot()) == 1 })
	r.close(t)
}

type nha029OwnerBlockedSun struct {
	entered chan struct{}
	once    sync.Once
}

func (s *nha029OwnerBlockedSun) NextSunset(ctx context.Context, _ time.Time, _ string, _ automationtriggers.SolarPosition) (automationtriggers.SunResult, error) {
	s.once.Do(func() { close(s.entered) })
	<-ctx.Done()
	return automationtriggers.SunResult{}, ctx.Err()
}
func (r *nha029OwnerRig) imageWithBlockedPermanentSun(t *testing.T, sun *nha029OwnerBlockedSun) configactivation.ProgramImage {
	t.Helper()
	automations, scripts, err := nha042ServerPrograms(nha029OwnerParent, nha029OwnerChild, false)
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
	file, err := parser.NewSourceFile("nha029-blocked-sun.yaml", []byte("id: nha029_blocked_sun\ntriggers:\n- trigger: sun\n  event: sunset\nactions:\n- stop: fixture_sun\n"))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := ir.CompileSources("automation.nha029_blocked_sun", file.Path(), []parser.SourceFile{file}, catalogue)
	if err != nil || len(compiled.Accepted()) != 1 || len(compiled.Rejected()) != 0 {
		t.Fatalf("sun fixture compilation: %v %+v", err, compiled.Rejected())
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
	templates, err := (reactivetemplate.Plan{}).WithScriptWaits(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	image, err := configactivation.NewProgramImage(runs, triggers, templates, nil)
	if err != nil {
		t.Fatal(err)
	}
	return image
}

// A real post-COMMIT solar callback times out after 040B is published but
// before the final 041 swap. C supersedes SQL in that exact window. Recovery
// discards B's real retained plan and keeps A's actual transient wait alive.
func TestNHA029OwnerRunRetargetsAfterPartial040PublicationAndReturnsCandidateB(t *testing.T) {
	r := nha029StartOwner(t, nha029OwnerParent, nha029OwnerChild, nil)
	r.waitBoth(t)
	original := r.composition.scripts.interpreter
	before, _ := original.NodeTraces()
	sun := &nha029OwnerBlockedSun{entered: make(chan struct{})}
	b := r.imageWithBlockedPermanentSun(t, sun)
	c := r.image(t, nha029OwnerParent, nha029OwnerChild)
	r.edit(t, "# B introduces a real blocked solar schedule\n")
	operation := nha029OwnerActivate(r, r.request(t, &b, nil, nil))
	nha034WaitSignal(t, sun.entered, "B040 actual bounded solar callback")
	r.edit(t, "# C supersedes the partial publication\n")
	winner := r.commitCurrentWorkspace(t)
	partial, err := nha029OwnerAwait(t, operation)
	if err == nil || partial.Outcome() != configactivation.CommittedReconciling {
		t.Fatalf("blocked source did not retain a retryable committed cut: %v %v", partial.Outcome(), err)
	}
	if _, err := r.owner.CurrentConfig(context.Background()); !errors.Is(err, configactivation.ErrActivationUnavailable) {
		t.Fatalf("partial B reopened authority: %v", err)
	}
	active, found, err := r.store.ReadActiveConfig(context.Background())
	if err != nil || !found || active.Revision() != winner {
		t.Fatalf("lost SQL winner C: %t %v", found, err)
	}
	result, err := r.owner.ReconcileWithProgram(context.Background(), winner, active.ManifestSHA256(), c)
	if err != nil || result.Outcome() != configactivation.Published || result.Revision() != winner {
		t.Fatalf("C did not replace partial040B: %v %v", result.Outcome(), err)
	}
	observations, _ := original.NodeTraces()
	for _, trace := range observations {
		if trace.Stage() == automationruns.NodeCancelled {
			t.Fatal("C recovery canceled intact A")
		}
	}
	if len(observations) < len(before) {
		t.Fatal("old image was released before its real A owners completed")
	}
	r.pilot.emit(t, "nha042_release")
	nha042ServerWait(t, "A040 wait fires after B discard and C publication", func() bool { return len(r.driver.snapshot()) == 1 })
	r.close(t)
}

func TestNHA029OwnerRunViewStampAndOldReplayInvalidateTogether(t *testing.T) {
	r := nha029StartOwner(t, nha029OwnerParent, nha029OwnerChild, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	actor, err := r.environment.principal.Actor()
	if err != nil {
		t.Fatal(err)
	}
	viewCtx, err := authorization.WithPrincipal(ctx, r.environment.principal)
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.views.OpenView(viewCtx, actor, view.All(), 16)
	if err != nil {
		t.Fatal(err)
	}
	oldWire, err := view.EncodePage(first)
	if err != nil {
		t.Fatal(err)
	}
	watch, err := r.views.WatchView(viewCtx, actor, &statewire.WatchViewRequest{Cursor: oldWire.Cursor, Filter: &statewire.ViewFilter{}})
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()
	r.edit(t, "# comment-only coherent B for views\n")
	result, err := r.owner.Activate(ctx, r.request(t, nil, nil, nil))
	if err != nil || result.Outcome() != configactivation.Published {
		t.Fatalf("view publication: %v %v", result.Outcome(), err)
	}
	// The previously idle subscription cannot send an old-generation frame
	// after this publication, even when the Registry contents did not change.
	frame, err := watch.Next(viewCtx)
	if err != nil || frame.GetResyncRequired().GetReason() != statewire.ResyncReason_RESYNC_REASON_CONFIGURATION_CHANGED {
		t.Fatalf("old idle view was not invalidated: %v %v", frame, err)
	}
	replay, err := r.views.WatchView(viewCtx, actor, &statewire.WatchViewRequest{Cursor: oldWire.Cursor, Filter: &statewire.ViewFilter{}})
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	frame, err = replay.Next(viewCtx)
	if err != nil || frame.GetResyncRequired().GetReason() != statewire.ResyncReason_RESYNC_REASON_CONFIGURATION_CHANGED {
		t.Fatalf("old replay crossed the configuration stamp: %v %v", frame, err)
	}
	current, err := r.views.OpenView(viewCtx, actor, view.All(), 16)
	if err != nil {
		t.Fatal(err)
	}
	_, stamp, _, _, err := current.Cursor.Components()
	if err != nil {
		t.Fatal(err)
	}
	revision, hash, err := stamp.Components()
	if err != nil {
		t.Fatal(err)
	}
	active, found, err := r.store.ReadActiveConfig(ctx)
	if err != nil || !found || revision != uint64(result.Revision().Number()) || active.Revision() != result.Revision() || hash != active.ManifestSHA256() {
		t.Fatalf("view/SQL stamp mismatch: revision=%d active=%v err=%v", revision, active.Revision(), err)
	}
	r.close(t)
}
