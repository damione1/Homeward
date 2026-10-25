package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

type nha041ServerPreparer struct{}

func (nha041ServerPreparer) Prepare(_ context.Context, _ ir.Automation, _ automationruns.Trigger) (automationruns.Preparation, error) {
	return automationruns.Preparation{Outcome: automationruns.PreparationReady}, nil
}

type nha041ServerPrepareFunc func(context.Context, ir.Automation, automationruns.Trigger) (automationruns.Preparation, error)

func (f nha041ServerPrepareFunc) Prepare(ctx context.Context, automation ir.Automation, trigger automationruns.Trigger) (automationruns.Preparation, error) {
	return f(ctx, automation, trigger)
}

type nha041ServerExecutor func(context.Context, ir.Automation, automationruns.Run) error

func (f nha041ServerExecutor) Execute(ctx context.Context, automation ir.Automation, run automationruns.Run) error {
	return f(ctx, automation, run)
}

func nha041ServerAutomation(t *testing.T, mode string) ir.Automation {
	return nha041ServerAutomationID(t, mode, "nha041_runtime")
}

func nha041ServerAutomationID(t *testing.T, mode, id string) ir.Automation {
	t.Helper()
	entity, err := ir.NewEntity("switch.synthetic", ir.ReferencePresent, ir.SupportObserveState, ir.SupportSwitchTurnOn)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue([]ir.Entity{entity}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.NewSourceFile("nha041-runtime.yaml", []byte(fmt.Sprintf(
		"id: %s\nmode: %s\nmax: 2\ntriggers:\n- trigger: time_pattern\n  seconds: '*'\nactions:\n- action: switch.turn_on\n  target: {entity_id: switch.synthetic}\n  data: {}\n", id, mode)))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := ir.CompileSources("automation."+id, "nha041-runtime.yaml", []parser.SourceFile{file}, catalogue)
	if err != nil || len(compiled.Accepted()) != 1 {
		t.Fatalf("compile %s: %v rejected=%+v", mode, err, compiled.Rejected())
	}
	return compiled.Accepted()[0]
}

func nha041ServerPlans(t *testing.T, mode string) (automationtriggers.Plan, automationruns.Plan) {
	t.Helper()
	automation := nha041ServerAutomation(t, mode)
	triggers, err := automationtriggers.NewPlan(automationtriggers.PlanInput{
		Automations: []ir.Automation{automation}, Zone: "America/Toronto", Resolver: nha040RuntimeResolver{},
	})
	if err != nil {
		t.Fatal(err)
	}
	runs, err := automationruns.NewPlan([]ir.Automation{automation})
	if err != nil {
		t.Fatal(err)
	}
	return triggers, runs
}

func nha041ServerWait(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for automation run decision")
}

func nha041ServerHas(composition *AutomationRunsComposition, decision automationruns.Decision) bool {
	traces, _ := composition.Traces()
	for _, trace := range traces {
		if trace.Decision == decision {
			return true
		}
	}
	return false
}

func TestNHA041RunWithFactoryAllModes(t *testing.T) {
	for _, mode := range []string{"single", "restart", "queued", "parallel"} {
		t.Run(mode, func(t *testing.T) {
			cluster := testpg.Start(t)
			cfg := nha030ServerConfig(t, cluster.Port)
			triggerPlan, runPlan := nha041ServerPlans(t, mode)
			started := make(chan automationruns.Run, 32)
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseRuns := func() { releaseOnce.Do(func() { close(release) }) }
			defer releaseRuns()
			composition, err := NewAutomationRunsComposition(AutomationRunsInput{
				Plan: runPlan, Preparer: nha041ServerPreparer{},
				Executor: nha041ServerExecutor(func(ctx context.Context, _ ir.Automation, run automationruns.Run) error {
					started <- run
					if mode == "restart" && run.ID() == 1 {
						<-ctx.Done()
						<-release // Observe that the next run waits for the old one to join.
						return ctx.Err()
					}
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			factory := RuntimeFactory(func(context.Context, *storage.Store) (RuntimePlan, error) {
				return RuntimePlan{Identity: unconfiguredIdentity{}, AutomationTriggers: &triggerPlan, AutomationRuns: composition}, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ready := make(chan struct{}, 1)
			done := make(chan error, 1)
			go func() {
				done <- RunWithStartupFactory(ctx, cfg.startup, (&net.ListenConfig{}).Listen, func(net.Addr) { ready <- struct{}{} }, factory)
			}()
			select {
			case <-ready:
			case err := <-done:
				t.Fatalf("Run exited before ready: %v", err)
			case <-time.After(8 * time.Second):
				t.Fatal("Run failed to start")
			}
			var first automationruns.Run
			select {
			case first = <-started:
			case err := <-done:
				t.Fatalf("Run exited before first automation: %v", err)
			case <-time.After(4 * time.Second):
				t.Fatal("real trigger reader did not reach executor double")
			}
			if first.ID() == 0 || first.Trigger().Automation() != "nha041_runtime" {
				t.Fatalf("lost trigger/run identity: id=%d automation=%q", first.ID(), first.Trigger().Automation())
			}
			switch mode {
			case "single":
				nha041ServerWait(t, func() bool { return nha041ServerHas(composition, automationruns.DecisionSingleBusy) })
				select {
				case <-started:
					t.Fatal("single started a second execution")
				default:
				}
			case "queued":
				nha041ServerWait(t, func() bool { return nha041ServerHas(composition, automationruns.DecisionQueued) })
				select {
				case <-started:
					t.Fatal("queued started before its predecessor finished")
				default:
				}
				releaseRuns()
				select {
				case <-started:
				case <-time.After(2 * time.Second):
					t.Fatal("queued run did not advance after join")
				}
			case "parallel":
				select {
				case second := <-started:
					if second.ID() == first.ID() || second.Environment() == first.Environment() {
						t.Fatal("parallel run did not get private identity and environment")
					}
				case <-time.After(4 * time.Second):
					t.Fatal("parallel did not start concurrently")
				}
			case "restart":
				nha041ServerWait(t, func() bool { return nha041ServerHas(composition, automationruns.DecisionRestartPending) })
				select {
				case <-started:
					t.Fatal("restart overlapped the old execution")
				default:
				}
				releaseRuns()
				select {
				case <-started:
				case <-time.After(2 * time.Second):
					t.Fatal("restart did not start after old run joined")
				}
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(8 * time.Second):
				t.Fatal("Run did not join automation controller")
			}
			select {
			case <-composition.Done():
			default:
				t.Fatal("automation controller Done remained open after Run")
			}
		})
	}
}

func TestNHA041SingleTriggerConsumer(t *testing.T) {
	cluster := testpg.Start(t)
	cfg := nha030ServerConfig(t, cluster.Port)
	triggerPlan, runPlan := nha041ServerPlans(t, "single")
	composition, err := NewAutomationRunsComposition(AutomationRunsInput{
		Plan: runPlan, Preparer: nha041ServerPreparer{},
		Executor: nha041ServerExecutor(func(context.Context, ir.Automation, automationruns.Run) error {
			return errors.New("unexpected execution")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	callbackCalled := false
	factory := RuntimeFactory(func(context.Context, *storage.Store) (RuntimePlan, error) {
		return RuntimePlan{Identity: unconfiguredIdentity{}, AutomationTriggers: &triggerPlan, AutomationRuns: composition,
			ConfigureAutomationTriggerResults: func(context.Context, AutomationTriggerResults) error {
				callbackCalled = true
				return nil
			}}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	err = RunWithStartupFactory(ctx, cfg.startup, (&net.ListenConfig{}).Listen, nil, factory)
	if !errors.Is(err, ErrInvalidTriggerComposition) || callbackCalled {
		t.Fatalf("second trigger reader was admitted: err=%v callback=%v", err, callbackCalled)
	}
}

func TestNHA041RunRejectsMismatchedAutomationIDs(t *testing.T) {
	cluster := testpg.Start(t)
	cfg := nha030ServerConfig(t, cluster.Port)
	triggerPlan, _ := nha041ServerPlans(t, "single")
	runPlan, err := automationruns.NewPlan([]ir.Automation{nha041ServerAutomationID(t, "single", "other")})
	if err != nil {
		t.Fatal(err)
	}
	composition, err := NewAutomationRunsComposition(AutomationRunsInput{
		Plan: runPlan, Preparer: nha041ServerPreparer{},
		Executor: nha041ServerExecutor(func(context.Context, ir.Automation, automationruns.Run) error {
			return errors.New("unexpected execution")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	listened := false
	factory := RuntimeFactory(func(context.Context, *storage.Store) (RuntimePlan, error) {
		return RuntimePlan{Identity: unconfiguredIdentity{}, AutomationTriggers: &triggerPlan, AutomationRuns: composition}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	err = RunWithStartupFactory(ctx, cfg.startup, func(context.Context, string, string) (net.Listener, error) {
		listened = true
		return nil, errors.New("listener unexpectedly reached")
	}, nil, factory)
	if !errors.Is(err, ErrInvalidTriggerComposition) || listened || composition.Done() != nil {
		t.Fatalf("mismatched IDs entered Run: err=%v listened=%v done=%v", err, listened, composition.Done())
	}
}

func TestNHA041ConditionBeforeModeAdmission(t *testing.T) {
	cluster := testpg.Start(t)
	cfg := nha030ServerConfig(t, cluster.Port)
	triggerPlan, runPlan := nha041ServerPlans(t, "single")
	var preparations atomic.Int32
	started := make(chan struct{}, 1)
	composition, err := NewAutomationRunsComposition(AutomationRunsInput{
		Plan: runPlan,
		Preparer: nha041ServerPrepareFunc(func(context.Context, ir.Automation, automationruns.Trigger) (automationruns.Preparation, error) {
			if preparations.Add(1) == 1 {
				return automationruns.Preparation{Outcome: automationruns.PreparationReady}, nil
			}
			return automationruns.Preparation{Outcome: automationruns.PreparationConditionFalse}, nil
		}),
		Executor: nha041ServerExecutor(func(ctx context.Context, _ ir.Automation, _ automationruns.Run) error {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	factory := RuntimeFactory(func(context.Context, *storage.Store) (RuntimePlan, error) {
		return RuntimePlan{Identity: unconfiguredIdentity{}, AutomationTriggers: &triggerPlan, AutomationRuns: composition}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunWithStartupFactory(ctx, cfg.startup, (&net.ListenConfig{}).Listen, nil, factory) }()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("Run exited before first eligible execution: %v", err)
	case <-time.After(4 * time.Second):
		t.Fatal("eligible trigger did not start")
	}
	nha041ServerWait(t, func() bool { return nha041ServerHas(composition, automationruns.DecisionConditionFalse) })
	if nha041ServerHas(composition, automationruns.DecisionSingleBusy) {
		t.Fatal("false condition consumed single admission or cancelled prior run")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Run did not join after false condition")
	}
}

func TestNHA041RunBootstrapFailureNeverStartsPreparedController(t *testing.T) {
	for _, failure := range []string{"invalid-public-after-construction", "invalid-supervisor-before-admissions"} {
		t.Run(failure, func(t *testing.T) {
			cluster := testpg.Start(t)
			cfg := nha030ServerConfig(t, cluster.Port)
			triggerPlan, runPlan := nha041ServerPlans(t, "single")
			var preparations, executions atomic.Int32
			composition, err := NewAutomationRunsComposition(AutomationRunsInput{
				Plan: runPlan,
				Preparer: nha041ServerPrepareFunc(func(context.Context, ir.Automation, automationruns.Trigger) (automationruns.Preparation, error) {
					preparations.Add(1)
					return automationruns.Preparation{Outcome: automationruns.PreparationReady}, nil
				}),
				Executor: nha041ServerExecutor(func(context.Context, ir.Automation, automationruns.Run) error {
					executions.Add(1)
					return nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			var badPublic *PublicPlan
			locked := false
			if failure == "invalid-public-after-construction" {
				badPublic = &PublicPlan{}
				badPublic.mu.Lock()
				locked = true
				defer func() {
					if locked {
						badPublic.mu.Unlock()
					}
				}()
			}
			var capturedStore *storage.Store
			factory := RuntimeFactory(func(_ context.Context, store *storage.Store) (RuntimePlan, error) {
				capturedStore = store
				plan := RuntimePlan{Identity: unconfiguredIdentity{}, AutomationTriggers: &triggerPlan, AutomationRuns: composition, Public: badPublic}
				if failure == "invalid-supervisor-before-admissions" {
					plan.Pilots = []integrations.PilotSpec{{}}
				}
				return plan, nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- RunWithStartupFactory(ctx, cfg.startup, (&net.ListenConfig{}).Listen, nil, factory) }()
			if badPublic != nil {
				nha041ServerWait(t, func() bool { return composition.Done() != nil })
				// The former eager Start would execute a time-pattern trigger while
				// the invalid public plan blocks the rest of bootstrap.
				time.Sleep(1500 * time.Millisecond)
				if preparations.Load() != 0 || executions.Load() != 0 {
					t.Fatal("Prepare/Execute ran before bootstrap admissions opened")
				}
				badPublic.mu.Unlock()
				locked = false
			}
			select {
			case err := <-done:
				want := ErrInvalidPublicPlan
				if failure == "invalid-supervisor-before-admissions" {
					want = integrations.ErrInvalidInput
				}
				if !errors.Is(err, want) {
					t.Fatalf("bootstrap failure lost: %v want %v", err, want)
				}
			case <-time.After(8 * time.Second):
				t.Fatal("bootstrap failure retained a never-started controller")
			}
			if preparations.Load() != 0 || executions.Load() != 0 {
				t.Fatalf("bootstrap failure invoked ports: prepares=%d executes=%d", preparations.Load(), executions.Load())
			}
			select {
			case <-composition.Done():
			default:
				t.Fatal("never-started controller Done remained open")
			}
			probeCtx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if err := capturedStore.InTransaction(probeCtx, func(tx pgx.Tx) error {
				return tx.QueryRow(probeCtx, "SELECT 1").Scan(new(int))
			}); err == nil {
				t.Fatal("Store remained open after pre-start controller closed")
			}
			observer := nha030ServerObserver(t, cluster.Port)
			if _, clean, err := nha030EpochState(observer); err != nil || !clean {
				t.Fatalf("Recorder epoch not closed after bootstrap failure: clean=%v err=%v", clean, err)
			}
		})
	}
}

func TestNHA041PreparationSaturationAndJoin(t *testing.T) {
	cluster := testpg.Start(t)
	cfg := nha030ServerConfig(t, cluster.Port)
	triggerPlan, runPlan := nha041ServerPlans(t, "parallel")
	entered := make(chan struct{}, automationruns.MaxPreparing)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releasePrepare := func() { releaseOnce.Do(func() { close(release) }) }
	defer releasePrepare()
	composition, err := NewAutomationRunsComposition(AutomationRunsInput{
		Plan: runPlan,
		Preparer: nha041ServerPrepareFunc(func(ctx context.Context, _ ir.Automation, _ automationruns.Trigger) (automationruns.Preparation, error) {
			entered <- struct{}{}
			<-release // Deliberately test a port that delays after cancellation.
			return automationruns.Preparation{}, ctx.Err()
		}),
		Executor: nha041ServerExecutor(func(context.Context, ir.Automation, automationruns.Run) error {
			return errors.New("unexpected execution")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	factory := RuntimeFactory(func(context.Context, *storage.Store) (RuntimePlan, error) {
		return RuntimePlan{Identity: unconfiguredIdentity{}, AutomationTriggers: &triggerPlan, AutomationRuns: composition}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- RunWithStartupFactory(ctx, cfg.startup, (&net.ListenConfig{}).Listen, func(net.Addr) { ready <- struct{}{} }, factory)
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Run exited before ready: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("Run did not start")
	}
	defer func() {
		if t.Failed() {
			traces, dropped := composition.Traces()
			t.Logf("preparation saturation: entered=%d dropped=%d traces=%+v", len(entered), dropped, traces)
		}
	}()
	nha041ServerWait(t, func() bool { return nha041ServerHas(composition, automationruns.DecisionPreparationSaturated) })
	if len(entered) != automationruns.MaxPreparing {
		t.Fatalf("preparation count exceeded/underused cap: %d", len(entered))
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("Run returned before preparing ports joined: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case <-composition.Done():
		t.Fatal("controller Done closed before Prepare returned")
	default:
	}
	releasePrepare()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Run did not join delayed preparations")
	}
}
