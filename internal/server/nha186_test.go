package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/logicselection"
	"github.com/damione1/Homeward/internal/scenes"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
)

type nha186Lights struct {
	host   *integrations.Manager
	driver *nha042ServerDriver
	keys   []integrations.SourceKey
	ready  chan struct{}
}

func (p *nha186Lights) Start(ctx context.Context, source integrations.SessionHost, _ integrations.Tasks) error {
	var candidates []integrations.Candidate
	for _, key := range p.keys {
		candidate, err := integrations.NewCandidate(key, []*registrywire.CapabilityDescription{{
			SchemaId: contracts.LightSchema, Major: 1, Endpoint: "state", DisplayName: "NHA186 isolated scene lamp",
			Constraints: &registrywire.CapabilityDescription_Light{Light: &registrywire.LightConstraints{SetOn: true, BrightnessPercent: &registrywire.NumberBounds{Minimum: 0, Maximum: 100}}},
		}})
		if err != nil {
			return err
		}
		encoding, err := integrations.NewMQTTLightEncoding(254, sha256.Sum256([]byte("nha186-isolated-lamp-scale254")), false)
		if err != nil {
			return err
		}
		candidate, err = candidate.WithLightEncoding(encoding)
		if err != nil {
			return err
		}
		candidates = append(candidates, candidate)
	}
	bindings, err := source.Discover(ctx, candidates)
	if err != nil {
		return err
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	level := 0.0
	light, err := domain.NewLightValue(false, &level, nil)
	if err != nil {
		return err
	}
	value, err := domain.NewLightStateValue(light)
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		at := time.Now()
		observation, err := integrations.NewStateObservation("state", quality, &value, &at, at, nil)
		if err != nil {
			return err
		}
		if err := source.Publish(ctx, binding, observation); err != nil {
			return err
		}
	}
	session, ok := source.(*integrations.Session)
	if !ok {
		return integrations.ErrInvalidInput
	}
	if err := p.host.AttachCommandDriver(session, p.driver); err != nil {
		return err
	}
	close(p.ready)
	return nil
}

func (*nha186Lights) Stop(context.Context) error { return nil }

func nha186AddLights(plan *RuntimePlan, environment *nha042ServerEnvironment, lights *nha186Lights) error {
	instance, err := domain.ParseIntegrationInstanceID("nha186-simulated-lamps")
	if err != nil {
		return err
	}
	for index, alias := range []string{"light.kitchen_lights", "light.kitchen_main_lights", "light.kitchen_sink_light"} {
		key, err := integrations.NewSourceKey(fmt.Sprintf("nha186-lamp-%d", index), "state")
		if err != nil {
			return err
		}
		if _, err := plan.Identity.(*nha044Identity).repo.ConfigureBinding(context.Background(), instance, key, alias); err != nil {
			return err
		}
		lights.keys = append(lights.keys, key)
	}
	spec, err := integrations.NewPilotSpec(instance, func() integrations.Pilot { return lights })
	if err != nil {
		return err
	}
	plan.Pilots = append(plan.Pilots, spec)
	previous := plan.Configure
	plan.Configure = func(ctx context.Context, host *integrations.Manager, store *storage.Store) error {
		if err := previous(ctx, host, store); err != nil {
			return err
		}
		lights.host = host
		return nil
	}
	// Factory preparation precedes Run's authority hydration. Persist the
	// synthetic caller's grants here, rather than changing SQL behind a live
	// in-memory authorization snapshot inside the later Configure callback.
	settings, err := environment.config.PostgresSettings()
	if err != nil {
		return err
	}
	ctx := context.Background()
	store, err := storage.Open(ctx, settings)
	if err != nil {
		return err
	}
	defer store.Close()
	current, err := store.GetAuthorizationPolicy(ctx, environment.account, environment.account)
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
	account, _ := environment.account.Value()
	text := fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n", account)
	for _, scope := range []string{"actions, operation: read", "actions, operation: write", "state_registry, operation: read"} {
		text += "  - scope: {family: " + scope + "}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n"
	}
	source, err := authorization.AdmitPolicySource(environment.account, []byte(text))
	if err != nil {
		return err
	}
	outcome, err := store.ReplaceAuthorizationPolicy(ctx, environment.account, environment.account, source, expected)
	if err != nil {
		return err
	}
	_, _, err = outcome.Result()
	if err != nil {
		return err
	}
	return nil
}

func nha186ReadAll(t *testing.T, store *storage.Store, epoch storage.HistoryEpoch) []storage.HistoryRecordInput {
	records, _ := nha186ReadAllWithCoverage(t, store, epoch)
	return records
}

func nha186ReadAllWithCoverage(t *testing.T, store *storage.Store, epoch storage.HistoryEpoch) ([]storage.HistoryRecordInput, []storage.HistoryReadLoss) {
	t.Helper()
	var out []storage.HistoryRecordInput
	var losses []storage.HistoryReadLoss
	query := storage.HistoryPageQuery{Epoch: epoch, Limit: 128}
	for {
		page, err := store.ReadExecutionHistoryPage(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		if query.Cursor == nil {
			losses = page.Losses
		}
		for _, row := range page.Records {
			out = append(out, row.Record.Input())
		}
		if page.Next == nil {
			return out, losses
		}
		query.Cursor = page.Next
	}
}

func nha186SamePath(stored []int32, native []int) bool {
	if len(stored) != len(native) {
		return false
	}
	for i, v := range native {
		if int64(stored[i]) != int64(v) {
			return false
		}
	}
	return true
}

func nha186ServerStamp(t *testing.T, number int64, hash byte) automationruns.DefinitionProvenance {
	t.Helper()
	revision, err := storage.NewConfigGenerationRevision(number)
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := automationruns.NewBoundProgram(revision, [32]byte{hash})
	if err != nil {
		t.Fatal(err)
	}
	return stamp
}

func TestNHA186RunPersistsProducerManifestAcrossReopenWithoutReplay(t *testing.T) {
	environment := nha042ServerEnvironmentFor(t)
	selection, err := logicselection.LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := scenes.CompileSelected(selection, scope.LogicMVPSourcesV1())
	if err != nil {
		t.Fatal(err)
	}
	stampA := nha186ServerStamp(t, 11, 1)
	stampB := nha186ServerStamp(t, 12, 2)
	lights := &nha186Lights{driver: &nha042ServerDriver{observer: environment.driverObserver, entered: make(chan struct{}), release: make(chan struct{})}, ready: make(chan struct{})}
	lights.driver.unblock()
	const secret = "NHA186_VALUE_MUST_NEVER_ENTER_HISTORY"
	child := "- stop: done\n"
	parentBody := nha042ServerCommand + "- action: script.nha042_child\n- action: script.turn_on\n  target: {entity_id: script.nha042_child}\n- action: scene.turn_on\n  target: {entity_id: scene.kitchen_cosy_mode}\n  data: {}\n"
	// The original server fixture declares the same IDs and trigger plan. Replace
	// only its admitted program image before Run constructs the real interpreter.
	rig := nha042StartServer(t, environment, nha042ServerCommand, child, nha042ServerOptions{sceneCatalogue: &catalogue, change: func(plan *RuntimePlan) {
		if e := nha186AddLights(plan, environment, lights); e != nil {
			t.Error(e)
			return
		}
		automations, scripts, e := nha042ServerPrograms(nha042ServerCommand, child, false)
		if e != nil {
			t.Error(e)
			return
		}
		device, _ := ir.NewEntity(nha042ServerAlias, ir.ReferencePresent, ir.SupportSwitchTurnOn)
		scene, _ := ir.NewEntity("scene.kitchen_cosy_mode", ir.ReferencePresent, ir.SupportSceneTurnOn)
		cat, e := ir.NewCatalogue([]ir.Entity{device, scene}, nil, nil)
		if e != nil {
			t.Error(e)
			return
		}
		cat, e = cat.WithScripts(plan.AutomationRuns.plan.ScriptIDs())
		if e != nil {
			t.Error(e)
			return
		}
		file, e := parser.NewSourceFile("nha186-image-A.yaml", []byte("variables: {private_value: '"+secret+"'}\nsequence:\n"+parentBody))
		if e != nil {
			t.Error(e)
			return
		}
		compiled, e := ir.CompileScriptSources("script.nha042_parent", file.Path(), []parser.SourceFile{file}, cat)
		if e != nil || len(compiled.Accepted()) != 1 {
			t.Errorf("image A: %v %+v", e, compiled.Rejected())
			return
		}
		scripts[0] = compiled.Accepted()[0]
		image, e := automationruns.NewPlanWithScripts(automations, scripts)
		if e == nil {
			image, e = image.WithProvenance(stampA)
		}
		if e != nil {
			t.Error(e)
			return
		}
		plan.AutomationRuns.plan = image
		plan.AutomationRuns.scripts.input.Plan = image
	}})
	rig.run.waitReady(t)
	nha034WaitSignal(t, lights.ready, "three simulated scene lamps")
	// Admit the actual scene preparation without dispatching it. A fixture
	// routing/profile mistake now names the member's real refusal immediately.
	preflightCtx, preflightActor, err := (&nha042ServerAuthority{environment.principal}).Context(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	preflightScene, _ := domain.ParseHAAlias("scene.kitchen_cosy_mode")
	preflightRef, err := scriptScenesRuntime(rig.composition).service.Declarative(preflightScene)
	if err != nil {
		t.Fatal(err)
	}
	preflightID, _ := domain.ParseCommandID("nha186-preflight-no-effect")
	preflightSlot, err := scenes.NewPreparationSlot(scriptScenesRuntime(rig.composition).service, preflightRef, preflightActor, preflightID, nil, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	preflight, err := preflightSlot.PrepareOnce(preflightCtx)
	if err != nil {
		t.Fatalf("scene fixture preparation: %v", err)
	}
	for _, member := range preflight.Members() {
		if member.Outcome() != scenes.OutcomePrepared {
			t.Fatalf("scene fixture member=%v code=%v reason=%s", member.Alias(), member.Code(), member.Reason())
		}
	}
	rig.pilot.emit(t, "nha042_start")
	nha042ServerWait(t, "image A scene terminal", func() bool {
		traces, _ := rig.composition.Traces()
		for _, trace := range traces {
			if id, _ := trace.Script.Value(); id == "script.nha042_parent" && trace.Decision == automationruns.DecisionFinished {
				return true
			}
		}
		return false
	})
	if len(rig.driver.snapshot()) != 1 {
		t.Fatalf("fake effects=%d, want one switch effect", len(rig.driver.snapshot()))
	}
	if len(lights.driver.snapshot()) != 3 {
		t.Fatalf("scene fake effects=%d, want3", len(lights.driver.snapshot()))
	}
	epochText, err := rig.identity.Sequence().CurrentAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := storage.ParseHistoryEpoch(epochText)
	if err != nil {
		t.Fatal(err)
	}
	// A new independent technical scene invocation uses B. Existing run records
	// remain stamped A; no active-generation lookup is involved in the writer.
	sceneID, _ := domain.ParseHAAlias("scene.nha186_technical")
	alias, _ := domain.ParseHAAlias(nha042ServerAlias)
	member, err := scenes.NewExplicitSwitch(alias, true)
	if err != nil {
		t.Fatal(err)
	}
	sending, actor, err := (&nha042ServerAuthority{environment.principal}).Context(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	service := scriptScenesRuntime(rig.composition).service
	ref, omissions, err := service.CreateTemporary(sending, sceneID, []scenes.ExplicitMember{member}, nil, time.Minute, nil)
	if err != nil || len(omissions) != 0 {
		t.Fatalf("technical scene: %v omissions=%d", err, len(omissions))
	}
	invocation, _ := domain.ParseCommandID("nha186-technical-B")
	slot, err := scenes.NewPreparationSlot(service, ref, actor, invocation, nil, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := slot.PrepareOnce(sending)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApplyPreparedWithHistory(sending, prepared, scenes.HistoryCause{Run: automationruns.HistoryRunRef{Provenance: stampB}}); err != nil {
		t.Fatal(err)
	}
	rig.run.cancel()
	if err := rig.run.waitDone(t); err != nil {
		t.Fatal(err)
	}
	// Capture only after the real producer Done, including the enclosing
	// automation's terminal. Neither ring may have evicted a native emission.
	nativeNodes, nodesEvicted := rig.composition.NodeTraces()
	nativeDecisions, decisionsEvicted := rig.composition.Traces()
	if nodesEvicted != 0 || decisionsEvicted != 0 {
		t.Fatalf("native manifest evicted nodes=%d decisions=%d", nodesEvicted, decisionsEvicted)
	}
	nha034WaitRunStoreClosed(t, rig.store)
	settings, err := environment.config.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	before, losses := nha186ReadAllWithCoverage(t, reopened, epoch)
	if len(before) == 0 {
		t.Fatal("empty producer history")
	}
	byNative := map[uint64]storage.HistoryRecordInput{}
	links := map[uint8]int{}
	var commands, members, aggregates, technical int
	for _, row := range before {
		if row.ID.Epoch() != epoch || row.ID.Ordinal() == 0 || row.EmissionTime.Location() != time.UTC {
			t.Fatal("invalid persistent identity/time")
		}
		if row.RunID != 0 && (row.ConfigRevision != 11 || row.ManifestSHA256 != [32]byte{1}) {
			t.Fatal("image A was replaced")
		}
		if row.NativeOrdinal != 0 {
			byNative[row.NativeOrdinal] = row
		}
		switch row.Kind {
		case storage.HistoryCommand:
			commands++
			id, _ := rig.driver.snapshot()[0].Value()
			if row.CommandID != id || row.NodeVisit == 0 || row.CommandOrdinal == 0 || row.RunID == 0 {
				t.Fatal("real 024 command link missing")
			}
		case storage.HistoryScriptLink:
			links[row.ScriptMode]++
			if row.RunID == 0 || row.ParentRunID == 0 {
				t.Fatal("child ancestry missing")
			}
		case storage.HistorySceneMember:
			members++
			if row.CommandID == "" || row.SceneInvocation == "" {
				t.Fatal("scene member identities missing")
			}
		case storage.HistorySceneInvocation:
			aggregates++
		}
		if row.SceneInvocation == "nha186-technical-B" {
			technical++
			if row.RunID != 0 || row.ConfigRevision != 12 || row.ManifestSHA256 != [32]byte{2} {
				t.Fatal("technical invocation lost independent B provenance")
			}
		}
	}
	if commands != 1 || links[1] != 2 || links[2] != 1 || members != 4 || aggregates != 2 || technical != 2 {
		t.Fatalf("manifest command=%d links=%v members=%d invocations=%d B=%d", commands, links, members, aggregates, technical)
	}

	// Every admitted non-native emission above is mandatory. Native callbacks
	// may be refused by the documented nonblocking contention boundary, but a
	// missing native emission must have an exact durable count and a covering
	// UTC uncertainty envelope. Invalid metadata/full/oversize remain failures.
	var durableRejected uint64
	for _, loss := range losses {
		if loss.Reason != storage.HistoryLossContention || !loss.UncertainEnvelope || loss.KnownCount == 0 {
			t.Fatalf("unexpected loss in producer fixture: %+v", loss)
		}
		durableRejected += loss.KnownCount
	}
	covers := func(at time.Time) bool {
		emitted := at.UTC().Truncate(time.Microsecond)
		for _, loss := range losses {
			if !emitted.Before(loss.From) && !emitted.After(loss.Through) {
				return true
			}
		}
		return false
	}
	var missing uint64
	for _, node := range nativeNodes {
		ordinal, _ := node.Ordinal().Value()
		row, ok := byNative[ordinal]
		if !ok {
			missing++
			if !covers(node.At()) {
				t.Fatalf("native node ordinal=%d run=%d at=%s missing without durable contention coverage: %+v", ordinal, node.RunID(), node.At(), losses)
			}
			continue
		}
		position := node.Source().At()
		if row.Kind != storage.HistoryNode || row.RunID != uint64(node.RunID()) || row.ParentRunID != uint64(node.ParentID()) || row.ControllerGeneration != uint64(node.Generation()) || row.ObjectID != node.Source().Object() || row.SourcePath != node.Source().Path() || row.SourceFile != position.File || row.SourceLine != position.Line || row.SourceColumn != position.Column || row.NodeStage != uint8(node.Stage()) || row.NodeVisit != node.Visit() || row.ActionKind != uint8(node.Kind()) || row.FailureKind != uint8(node.Failure()) || row.WaitExit != uint8(node.WaitExit()) || !row.EmissionTime.Equal(node.At().UTC().Truncate(time.Microsecond)) || !nha186SamePath(row.Branches, node.Branches()) || !nha186SamePath(row.Iterations, node.Iterations()) {
			t.Fatalf("native node changed: ordinal=%d persisted=%+v", ordinal, row)
		}
	}
	for _, decision := range nativeDecisions {
		ordinal, _ := decision.Ordinal().Value()
		row, ok := byNative[ordinal]
		if !ok {
			missing++
			if !covers(decision.At) {
				t.Fatalf("native decision ordinal=%d run=%d decision=%d at=%s missing without durable contention coverage: %+v", ordinal, decision.RunID, decision.Decision, decision.At, losses)
			}
			continue
		}
		object := decision.Automation
		if script, err := decision.Script.Value(); err == nil {
			object = script
		}
		if row.Kind != storage.HistoryDecision || row.RunID != uint64(decision.RunID) || row.ParentRunID != uint64(decision.ParentID) || row.ControllerGeneration != uint64(decision.Generation) || row.Decision != uint8(decision.Decision) || row.ObjectID != object || row.TriggerKey != decision.TriggerKey || !row.EmissionTime.Equal(decision.At.UTC().Truncate(time.Microsecond)) {
			t.Fatalf("native decision changed: ordinal=%d native=%+v persisted=%+v", ordinal, decision, row)
		}
	}
	if missing != durableRejected {
		t.Fatalf("native missing=%d durable rejection count=%d coverage=%+v", missing, durableRejected, losses)
	}
	if missing != 0 {
		t.Logf("producer history explicitly partial/degraded: %d native emissions missing, exact durable contention coverage=%+v", missing, losses)
	} else {
		t.Log("producer history contains every native emission; no durable loss")
	}

	encoded, _ := json.Marshal(before)
	if strings.Contains(string(encoded), secret) {
		t.Fatal("private variable leaked")
	}
	reopened.Close()
	second := nha042StartServer(t, environment, nha042ServerCommand, child, nha042ServerOptions{})
	second.run.waitReady(t)
	nha042ServerProbe(t, second)
	nha042ServerNoRoutineResumed(t, second)
	after := nha186ReadAll(t, second.store, epoch)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("reopen changed committed identities/content")
	}
}

func TestNHA186RunLateSceneEmitsBeforeHistoryAndStoreClose(t *testing.T) {
	environment := nha042ServerEnvironmentFor(t)
	selection, err := logicselection.LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := scenes.CompileSelected(selection, scope.LogicMVPSourcesV1())
	if err != nil {
		t.Fatal(err)
	}
	rig := nha042StartServer(t, environment, "- stop: done\n", "", nha042ServerOptions{sceneCatalogue: &catalogue, blockedDriver: true})
	rig.run.waitReady(t)
	epochText, err := rig.identity.Sequence().CurrentAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := storage.ParseHistoryEpoch(epochText)
	if err != nil {
		t.Fatal(err)
	}
	sending, actor, err := (&nha042ServerAuthority{environment.principal}).Context(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	runtime := scriptScenesRuntime(rig.composition)
	service := runtime.service
	alias, _ := domain.ParseHAAlias(nha042ServerAlias)
	member, err := scenes.NewExplicitSwitch(alias, true)
	if err != nil {
		t.Fatal(err)
	}
	sceneID, _ := domain.ParseHAAlias("scene.nha186_late")
	ref, _, err := service.CreateTemporary(sending, sceneID, []scenes.ExplicitMember{member}, nil, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	invocation, _ := domain.ParseCommandID("nha186-late-scene")
	slot, err := scenes.NewPreparationSlot(service, ref, actor, invocation, nil, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := slot.PrepareOnce(sending)
	if err != nil {
		t.Fatal(err)
	}
	applied := make(chan error, 1)
	go func() { _, err := service.ApplyPrepared(sending, prepared); applied <- err }()
	nha034WaitSignal(t, rig.driver.entered, "late scene driver")
	rig.run.cancel()
	if err := rig.run.waitDone(t); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shared close budget: %v", err)
	}
	if err := nha034ProbeRunStore(rig.store); err != nil {
		t.Fatal("Store closed under live producer", err)
	}
	select {
	case <-runtime.done:
		t.Fatal("scene Close joined before its real effect returned")
	default:
	}
	lateFrom := time.Now().UTC()
	rig.driver.unblock()
	select {
	case err := <-applied:
		if !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
			t.Fatalf("late scene response after Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late scene did not join")
	}
	lateThrough := time.Now().UTC()
	nha034WaitSignal(t, runtime.done, "late scene Close completion")
	nha034WaitRunStoreClosed(t, rig.store)
	ids := rig.driver.snapshot()
	if len(ids) != 1 {
		t.Fatalf("late fake effects=%d", len(ids))
	}
	state, response := nha042ServerIntent(t, environment.observer, ids[0])
	// This fake driver ignores cancellation and returns nil after release.
	// The real 024 service durably records DISPATCHED; it does not confirm a
	// physical state. This outcome remains mandatory even if history refuses
	// an observation at its nonblocking admission boundary.
	if state != "result" || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("late scene ledger=%s/%s", state, response.Status)
	}
	// Inventory this exact epoch after the real Store owner has closed. There
	// is no routine trigger in this fixture. Prove that there are no other
	// producers before attributing any durable loss to these two observations.
	nha034WaitSignal(t, rig.composition.Done(), "late fixture native producer completion")
	nativeNodes, nodesEvicted := rig.composition.NodeTraces()
	nativeDecisions, decisionsEvicted := rig.composition.Traces()
	if nodesEvicted != 0 || decisionsEvicted != 0 || len(nativeNodes) != 0 || len(nativeDecisions) != 0 {
		t.Fatalf("late scene has unrelated native emissions: nodes=%d decisions=%d evicted=%d/%d", len(nativeNodes), len(nativeDecisions), nodesEvicted, decisionsEvicted)
	}
	settings, err := environment.config.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	records, losses := nha186ReadAllWithCoverage(t, reopened, epoch)
	t.Logf("late scene durable inventory epoch=%s nodes=%d decisions=%d records=%d losses=%+v emission-window=[%s,%s]", epoch, len(nativeNodes), len(nativeDecisions), len(records), losses, lateFrom, lateThrough)
	commandID, err := ids[0].Value()
	if err != nil {
		t.Fatal(err)
	}
	expected := map[storage.HistoryRecordKind]storage.HistoryRecordInput{
		storage.HistorySceneMember: {
			Kind: storage.HistorySceneMember, SceneOutcome: 5, Provenance: storage.HistoryUnboundFixture,
			ObjectID: nha042ServerAlias, CommandID: commandID, SceneInvocation: "nha186-late-scene",
		},
		storage.HistorySceneInvocation: {
			Kind: storage.HistorySceneInvocation, SceneAggregate: 1, Provenance: storage.HistoryUnboundFixture,
			ObjectID: "scene.nha186_late", CommandID: "nha186-late-scene", SceneInvocation: "nha186-late-scene",
		},
	}
	for index, row := range records {
		want, ok := expected[row.Kind]
		if !ok {
			t.Fatalf("duplicate or unrelated late scene row: %+v", row)
		}
		want.ID, err = storage.NewHistoryTraceID(epoch, uint64(index+1))
		if err != nil {
			t.Fatal(err)
		}
		want.EmissionTime = row.EmissionTime
		if row.EmissionTime.Location() != time.UTC || row.EmissionTime.Before(lateFrom.Truncate(time.Microsecond)) || row.EmissionTime.After(lateThrough) || !reflect.DeepEqual(row, want) {
			t.Fatalf("late scene row changed or emitted outside live producer: got=%+v want=%+v", row, want)
		}
		delete(expected, row.Kind)
	}
	// Both facts must be accounted for: a unique exact row or a known durable
	// contention loss. With no other producer, no loss can mask an unrelated
	// missing emission. Closing/full/invalid/oversize losses remain failures;
	// a silent drop or a Store/history owner joining early remains a failure.
	var durableRejected uint64
	for _, loss := range losses {
		from, through := loss.From.UTC(), loss.Through.UTC()
		if loss.Reason != storage.HistoryLossContention || !loss.UncertainEnvelope || loss.KnownCount == 0 || from.After(through) || from.After(lateThrough) || through.Before(lateFrom.Truncate(time.Microsecond)) || loss.KnownCount > 2-durableRejected {
			t.Fatalf("unexpected loss in late scene fixture: %+v", loss)
		}
		durableRejected += loss.KnownCount
	}
	if uint64(len(expected)) != durableRejected {
		t.Fatalf("late scene missing=%d durable contention count=%d records=%+v losses=%+v", len(expected), durableRejected, records, losses)
	}
	t.Logf("late scene accounted records=%d contention=%d ledger=%s/%s effects=%d", len(records), durableRejected, state, response.Status, len(ids))
}

func TestNHA186RunPreparationFailureRetainsNoHistoryOwner(t *testing.T) {
	environment := nha042ServerEnvironmentFor(t)
	rig := nha042StartServer(t, environment, "- stop: done\n", "", nha042ServerOptions{change: func(plan *RuntimePlan) { plan.ReactiveTemplates = nil }})
	if err := rig.run.waitDone(t); !errors.Is(err, ErrInvalidTriggerComposition) {
		t.Fatalf("prepare: %v", err)
	}
	nha034WaitRunStoreClosed(t, rig.store)
	if rig.pilot.startCount() != 0 {
		t.Fatal("failed preparation started a producer")
	}
}

func TestNHA186RunProgressesWhileHistoryPostgresIsLocked(t *testing.T) {
	environment := nha042ServerEnvironmentFor(t)
	rig := nha042StartServer(t, environment, "- stop: done\n", "", nha042ServerOptions{})
	rig.run.waitReady(t)
	tx, err := environment.observer.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var cutoff time.Time
	if err := tx.QueryRow(context.Background(), `SELECT retention_cutoff FROM public.execution_history_head WHERE singleton=true FOR UPDATE`).Scan(&cutoff); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	rig.pilot.emit(t, "nha042_start")
	nha042ServerWait(t, "actual routine terminal before PG unlock", func() bool {
		traces, _ := rig.composition.Traces()
		for _, trace := range traces {
			if trace.Automation == "nha042_server" && trace.RunID != 0 && trace.Decision == automationruns.DecisionFinished {
				return true
			}
		}
		return false
	})
	elapsed := time.Since(started)
	if elapsed > time.Second {
		t.Fatalf("routine exceeded simulated one-second budget: %s", elapsed)
	}
	// The routine finished while the lock is still held. Independently observe
	// its writer waiting on that lock, rather than inferring a SQL outage.
	nha042ServerWait(t, "Run-owned history writer PostgreSQL lock", func() bool {
		var count int
		err := environment.driverObserver.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE '%execution_history_head%'`).Scan(&count)
		if err != nil {
			t.Error(err)
			return false
		}
		return count != 0
	})
	if len(rig.driver.snapshot()) != 0 {
		t.Fatal("pure simulated routine emitted an action")
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	rig.run.cancel()
	if err := rig.run.waitDone(t); err != nil {
		t.Fatal(err)
	}
	nha034WaitRunStoreClosed(t, rig.store)
	var terminal int
	if err := environment.observer.QueryRow(context.Background(), `SELECT count(*) FROM public.execution_history_records WHERE kind=1 AND decision=12 AND object_id='nha042_server'`).Scan(&terminal); err != nil || terminal != 1 {
		t.Fatalf("recovered routine terminal=%d err=%v", terminal, err)
	}
	t.Logf("real Run routine completed in %s before observed PostgreSQL history lock released", elapsed)
}
