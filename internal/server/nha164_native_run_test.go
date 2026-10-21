package server

// Exercise Run's native authentication, configuration owner and Recorder with
// local fake integrations and disposable PostgreSQL only.
import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	history "github.com/damione1/Homeward/gen/home/history/v1"
	"github.com/damione1/Homeward/gen/home/history/v1/historyv1connect"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/reactivetemplate"
	"github.com/damione1/Homeward/internal/recordermaintenance"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Admission-time control only; authentication/session clocks remain real. This
// creates the known032 weighted hour without sleeping or rewriting SQL rows.
type nha164NativeClock struct {
	mu   sync.Mutex
	at   time.Time
	tick time.Duration
}

func (c *nha164NativeClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *nha164NativeClock) Tick() time.Duration { c.mu.Lock(); defer c.mu.Unlock(); return c.tick }
func (c *nha164NativeClock) set(at time.Time) {
	c.mu.Lock()
	c.tick += at.Sub(c.at)
	c.at = at
	c.mu.Unlock()
}

func TestNHA164NativeRunTLSHistoryOwnerGenerationAndRevocation(t *testing.T) {
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	cfg := nha049Config(t, cluster.Port, data)
	captured, err := cfg.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	raw := captured.Bytes()
	path := filepath.Join(t.TempDir(), "configuration.yaml")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	admission := &nha164NativeClock{at: at}
	maintenanceClock := nha032Clock()
	maintenanceClock.releaseStop()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var store *storage.Store
	var host *integrations.Manager
	var owner *configactivation.Owner
	var actual *observationRuntime
	var maintenance *recordermaintenance.Worker
	var meterEntity domain.EntityID
	var pilot *nha042ServerPilot
	var composition *AutomationRunsComposition
	switchInstance, _ := domain.ParseIntegrationInstanceID("nha164-native-switch")
	switchKey, _ := integrations.NewSourceKey("native-switch", "relay")
	meterInstance, _ := domain.ParseIntegrationInstanceID("nha164-native-meter")
	meterKey, _ := integrations.NewSourceKey("native-meter", "power")
	observer := nha030ServerObserver(t, cluster.Port)
	driver := &nha042ServerDriver{observer: observer, entered: make(chan struct{}), release: make(chan struct{})}
	driver.unblock()
	factory := func(ctx context.Context, s *storage.Store) (RuntimePlan, error) {
		store = s
		manifest, err := registry.ParseManifest(scope.ManifestV1())
		if err != nil {
			return RuntimePlan{}, err
		}
		repository, err := registry.NewRepository(s, manifest)
		if err != nil {
			return RuntimePlan{}, err
		}
		target, err := repository.ConfigureBinding(ctx, switchInstance, switchKey, nha042ServerAlias)
		if err != nil {
			return RuntimePlan{}, err
		}
		meter, err := repository.ConfigureBinding(ctx, meterInstance, meterKey, "sensor.chambre_eugenie_power")
		if err != nil {
			return RuntimePlan{}, err
		}
		meterEntity = meter.Entity
		automations, scripts, err := nha042ServerPrograms("- stop: native_history_done\n", "", false)
		if err != nil {
			return RuntimePlan{}, err
		}
		runs, err := automationruns.NewPlanWithScripts(automations, scripts)
		if err != nil {
			return RuntimePlan{}, err
		}
		// This test assembles synthetic IR. Bind that initial image to the
		// authority Run actually admitted; a later workspace-only activation
		// retains this image and must not relabel its traces as the new config.
		active, present, err := s.ReadActiveConfig(ctx)
		if err != nil {
			return RuntimePlan{}, err
		}
		if !present {
			return RuntimePlan{}, configactivation.ErrInvalidStartup
		}
		provenance, err := automationruns.NewBoundProgram(active.Revision(), active.ManifestSHA256())
		if err != nil {
			return RuntimePlan{}, err
		}
		runs, err = runs.WithProvenance(provenance)
		if err != nil {
			return RuntimePlan{}, err
		}
		commands, err := NewDurableCommandsComposition(DurableCommandsInput{Generation: nha094Generation{}, Clock: automationtriggers.RealClock{}})
		if err != nil {
			return RuntimePlan{}, err
		}
		composition, err = NewScriptRunsComposition(ScriptRunsInput{Plan: runs, Commands: commands, Clock: automationtriggers.RealClock{}, Zone: time.UTC, CommandDeadline: time.Minute})
		if err != nil {
			return RuntimePlan{}, err
		}
		templatePlan, err := (reactivetemplate.Plan{}).WithScriptWaits(time.UTC)
		if err != nil {
			return RuntimePlan{}, err
		}
		reactive, err := NewReactiveTemplateComposition(templatePlan, nha042NoPermanentOutput{}, reactivetemplate.RealClock{})
		if err != nil {
			return RuntimePlan{}, err
		}
		alias, _ := domain.ParseHAAlias(nha042ServerAlias)
		triggers, err := automationtriggers.NewPlan(automationtriggers.PlanInput{Automations: automations, Scripts: scripts, Resolver: nha042ServerResolver{entities: map[domain.HAAlias]domain.EntityID{alias: target.Entity}}, Zone: "UTC"})
		if err != nil {
			return RuntimePlan{}, err
		}
		pilot = &nha042ServerPilot{key: switchKey, driver: driver, clock: automationtriggers.RealClock{}, ready: make(chan struct{})}
		spec, err := integrations.NewPilotSpec(switchInstance, func() integrations.Pilot { return pilot })
		if err != nil {
			return RuntimePlan{}, err
		}
		return RuntimePlan{Identity: repository, Pilots: []integrations.PilotSpec{spec}, AutomationRuns: composition, DurableCommands: commands, ReactiveTemplates: reactive, AutomationTriggers: &triggers,
			Configure: func(_ context.Context, h *integrations.Manager, _ *storage.Store) error {
				host = h
				pilot.host = h
				return nil
			},
			// The ordinary authorization composition attaches this real SQL registry
			// to Run's sequencer before either fake source discovers its catalogue.
			ConfigureAuthorization: func(context.Context, AuthorizationComposition) error { return nil },
			ConfigureActivation:    func(_ context.Context, o *configactivation.Owner) error { owner = o; return nil }}, nil
	}
	runCtx, stop := context.WithCancel(ctx)
	run := &nha162Running{stop: stop, done: make(chan error, 1)}
	ready := make(chan net.Addr, 1)
	dependencies := runDependencies{sequenceClock: admission, maintenanceClock: maintenanceClock, observeRuntime: func(r *observationRuntime) { actual = r }, observeRecorderMaintenance: func(w *recordermaintenance.Worker) { maintenance = w }}
	go func() {
		run.done <- runWithDependencies(runCtx, startup, (&net.ListenConfig{}).Listen, func(address net.Addr) { ready <- address }, factory, dependencies)
	}()
	t.Cleanup(func() {
		if !run.joined {
			if err := run.join(t); err != nil {
				t.Error(err)
			}
		}
	})
	select {
	case run.address = <-ready:
	case err := <-run.done:
		run.joined = true
		t.Fatal("native Run before ready", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	nha034WaitSignal(t, pilot.ready, "native fake pilot ready")
	if owner == nil || actual == nil || store == nil || maintenance == nil {
		t.Fatal("Run did not compose native owners")
	}
	stamp, err := owner.CurrentConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	revision, manifestSHA, err := stamp.Components()
	if err != nil {
		t.Fatal(err)
	}
	active, present, err := store.ReadActiveConfig(ctx)
	if err != nil || !present || uint64(active.Revision().Number()) != revision || active.ManifestSHA256() != manifestSHA {
		t.Fatal("owner and active SQL generation disagree", err)
	}

	client := nha162RunClient(t, roots, run.address)
	base := "https://" + run.address.String()
	authAPI := authv1connect.NewAuthServiceClient(client, base)
	policyAPI := authv1connect.NewAuthorizationPolicyServiceClient(client, base)
	api := historyv1connect.NewHistoryServiceClient(client, base)
	const password = "NHA164 native synthetic password"
	nha162RunBootstrap(t, ctx, data, "NHA164 native admin", password)
	admin := nha162RunLogin(t, ctx, authAPI, "NHA164 native admin", password)
	created, err := authv1connect.NewAccountServiceClient(client, base).CreateAccount(ctx, nha162Bearer(&auth.CreateAccountRequest{LoginName: "NHA164 native reader", DisplayName: "NHA164 native reader", InitialPassword: password, Role: auth.AccountRole_ACCOUNT_ROLE_USER}, admin.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	reader := nha162RunLogin(t, ctx, authAPI, "NHA164 native reader", password)
	period := &history.Period{Start: timestamppb.New(at.Add(-time.Hour)), End: timestamppb.New(time.Now().UTC().Add(time.Hour))}
	name, _ := meterEntity.Value()
	request := func(token string) *history.QueryObservationsRequest {
		return &history.QueryObservationsRequest{EntityId: name, Period: proto.Clone(period).(*history.Period), Resolution: history.Resolution_RESOLUTION_RAW, PageSize: proto.Uint32(1), PageToken: token}
	}
	if response, err := api.QueryObservations(ctx, nha162Bearer(request(""), reader.AccessToken)); response != nil || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("native equipment denial failed", err)
	}
	historyGrant := "  - scope: {family: history, operation: read}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n"
	nha162RunPolicy(t, ctx, policyAPI, created.Msg.Account.Id, admin.AccessToken, historyGrant)
	if response, err := api.QueryExecutionTraces(ctx, nha162Bearer(&history.QueryExecutionTracesRequest{Period: period}, reader.AccessToken)); response != nil || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("history grant admitted global traces", err)
	}

	// Real Host → sequencer → Recorder. Three numeric transitions create the
	// reference032 hour:10 for15min,30 for45min, then50 at the next boundary.
	meterSession, err := host.OpenSession(meterInstance)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := integrations.NewCandidate(meterKey, []*registrywire.CapabilityDescription{{SchemaId: contracts.MeasurementSchema, Major: 1, DisplayName: "Synthetic native power", Endpoint: "power", Constraints: &registrywire.CapabilityDescription_Measurement{Measurement: &registrywire.MeasurementConstraints{Unit: "W", Precision: 2, Semantic: registrywire.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT, Aggregation: registrywire.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT}}}})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := meterSession.Discover(ctx, []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		t.Fatal("native measurement discovery", err)
	}
	unit, err := domain.NewUnitCatalog().Parse("W")
	if err != nil {
		t.Fatal(err)
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	for _, point := range []struct {
		offset time.Duration
		value  float64
	}{{0, 10}, {15 * time.Minute, 30}, {time.Hour, 50}} {
		pointAt := at.Add(point.offset)
		admission.set(pointAt)
		measurement, err := domain.NewMeasurement(point.value, unit, 2, domain.SemanticMeasurement, domain.AggregationCurrent)
		if err != nil {
			t.Fatal(err)
		}
		value, err := domain.NewMeasurementValue(measurement)
		if err != nil {
			t.Fatal(err)
		}
		observation, err := integrations.NewStateObservation("power", quality, &value, &pointAt, pointAt, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = meterSession.Publish(ctx, bindings[0], observation); err != nil {
			t.Fatal(err)
		}
	}
	capture, err := actual.sequence.CaptureEntityCatalogue(meterEntity, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	_, through, err := capture.Cursor().Components()
	if err != nil {
		t.Fatal(err)
	}
	nha042ServerWait(t, "actual Recorder prefix committed", func() bool {
		status, err := actual.recording.Status()
		return err == nil && status.Checkpoint >= through
	})
	maintenanceClock.advance()
	nha042ServerWait(t, "ordinary Run maintenance produced nonempty aggregate", func() bool {
		rows, err := store.ReadRecorderHourlyAggregates(ctx, name, at, at.Add(time.Hour))
		return err == nil && len(rows) == 1
	})
	first, err := api.QueryObservations(ctx, nha162Bearer(request(""), reader.AccessToken))
	if err != nil || contracts.AdmitHistoryResponse(first.Msg) != nil || len(first.Msg.GetPage().Points) != 1 {
		t.Fatal("native observation route", err)
	}
	token := first.Msg.GetPage().Continuation.GetNextPageToken()
	if token == "" {
		t.Fatal("native raw continuation missing")
	}
	aggregates, err := api.QueryAggregates(ctx, nha162Bearer(&history.QueryAggregatesRequest{EntityId: name, Period: &history.Period{Start: timestamppb.New(at), End: timestamppb.New(at.Add(time.Hour))}, Resolution: history.Resolution_RESOLUTION_HOUR}, reader.AccessToken))
	if err != nil || contracts.AdmitHistoryResponse(aggregates.Msg) != nil || len(aggregates.Msg.GetPage().Points) != 1 {
		t.Fatal("native nonempty aggregate TLS", err)
	}
	aggregate := aggregates.Msg.GetPage().Points[0]
	if aggregate.Mean == nil || *aggregate.Mean != 25 || aggregate.DurationNanos != uint64(time.Hour) {
		t.Fatal("032 weighted value lost through native TLS")
	}

	// A real029 publication changes the stamp used by164, invalidating the old
	// cursor without injecting a currentConfig callback or authorizing an action.
	if err = os.WriteFile(path, append(append([]byte(nil), raw...), []byte("\n# native164 publication\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	author, _ := storage.NewConfigAuthor("nha164:native-test")
	source, _ := storage.NewConfigSource("nha164:physical-workspace")
	activation, err := configactivation.NewWorkspaceRequest(configactivation.RetainDefinitions(), author, source, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	published, err := owner.Activate(ctx, activation)
	if err != nil || published.Outcome() != configactivation.Published {
		t.Fatal("native owner publication", err)
	}
	stamp, err = owner.CurrentConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	nextRevision, nextManifest, err := stamp.Components()
	if err != nil || nextRevision <= revision || nextManifest == manifestSHA {
		t.Fatal("native generation did not advance", err)
	}
	stale, err := api.QueryObservations(ctx, nha162Bearer(request(token), reader.AccessToken))
	if err != nil || stale.Msg.GetStaleCursor() == nil {
		t.Fatal("owner publication did not stale cursor", err)
	}
	pilot.emit(t, "nha042_start")
	// Keep this a true native Run observation. The owner published B, but the
	// request retained the A program; provenance belongs to the admitted IR.
	// Inspect each bounded page, since the physical history reader caps at31.
	type traceProgress struct {
		terminalRuns                                               int
		pages, points, losses                                      int
		runPoints, matchedA, mislabeledB, unbound, otherProvenance int
		revisionA, revisionB                                       uint64
		checkpoint                                                 uint64
		rpcCode                                                    connect.Code
		result                                                     string
	}
	var progress traceProgress
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		progress = traceProgress{revisionA: revision, revisionB: nextRevision}
		var runID uint64
		traces, _ := composition.Traces()
		for _, trace := range traces {
			if trace.Automation == "nha042_server" && trace.RunID != 0 && trace.Decision == automationruns.DecisionFinished {
				progress.terminalRuns++
				runID = uint64(trace.RunID)
			}
		}
		if runID != 0 {
			token := ""
			for pageNumber := 0; pageNumber < 8; pageNumber++ {
				response, err := api.QueryExecutionTraces(ctx, nha162Bearer(&history.QueryExecutionTracesRequest{Period: period, PageSize: proto.Uint32(historyquery.MaxTracePoints), PageToken: token}, admin.AccessToken))
				if err != nil {
					progress.rpcCode = connect.CodeOf(err)
					break
				}
				if response == nil {
					progress.result = "missing_response"
					break
				}
				if err := contracts.AdmitHistoryResponse(response.Msg); err != nil {
					progress.result = "invalid_response"
					break
				}
				page := response.Msg.GetPage()
				if page == nil {
					progress.result = "non_page"
					break
				}
				progress.pages++
				progress.points += len(page.Points)
				progress.losses += len(page.Losses)
				progress.checkpoint = page.Checkpoint
				for _, point := range page.Points {
					if point.RunId == nil || *point.RunId != runID {
						continue
					}
					progress.runPoints++
					switch {
					case point.ConfigRevision == nil && len(point.ManifestSha256) == 0:
						progress.unbound++
					case point.ConfigRevision != nil && *point.ConfigRevision == revision && bytes.Equal(point.ManifestSha256, manifestSHA[:]):
						progress.matchedA++
					case point.ConfigRevision != nil && *point.ConfigRevision == nextRevision && bytes.Equal(point.ManifestSha256, nextManifest[:]):
						progress.mislabeledB++
					default:
						progress.otherProvenance++
					}
				}
				token = page.Continuation.GetNextPageToken()
				if token == "" {
					break
				}
			}
			if token != "" {
				progress.result = "pagination_limit"
			}
			if progress.mislabeledB != 0 || progress.unbound != 0 || progress.otherProvenance != 0 {
				t.Fatalf("retained A run has inconsistent provenance: %+v", progress)
			}
			if progress.matchedA != 0 && progress.result == "" && progress.rpcCode == 0 {
				break
			}
		}
		select {
		case <-deadline.C:
			t.Fatalf("native trace not durably readable with retained A provenance: %+v", progress)
		case <-tick.C:
		}
	}
	if effects := driver.snapshot(); len(effects) != 0 {
		t.Fatal("pure native routine produced a simulated device action")
	}

	lock, err := observer.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(context.Background())
	if _, err = lock.Exec(ctx, `LOCK TABLE public.state_history IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	var blocker int
	if err = lock.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		response, err := api.QueryObservations(ctx, nha162Bearer(request(""), reader.AccessToken))
		if response != nil {
			result <- context.Canceled
			return
		}
		result <- err
	}()
	nha162WaitSQLBlockedBy(t, ctx, store, blocker, "%FROM public.state_history%")
	nha162RunPolicy(t, ctx, policyAPI, created.Msg.Account.Id, admin.AccessToken, "  []\n")
	nha162RunPolicy(t, ctx, policyAPI, created.Msg.Account.Id, admin.AccessToken, historyGrant)
	if err = lock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatal("native revoke/regrant rescued in-flight SQL", err)
		}
	case <-ctx.Done():
		t.Fatal("revoked native read failed to join", ctx.Err())
	}
	if err = run.join(t); err != nil {
		t.Fatal(err)
	}
	nha034WaitSignal(t, owner.Done(), "native029 owner joined")
	nha034WaitSignal(t, maintenance.Done(), "native032 worker joined")
	nha034WaitSignal(t, actual.recording.Done(), "native Recorder joined")
	nha034WaitRunStoreClosed(t, store)
}
