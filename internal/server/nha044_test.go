package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/reactivetemplate"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
	"github.com/jackc/pgx/v5"
)

type nha044Identity struct {
	repo     *registry.Repository
	mu       sync.Mutex
	sequence *sequencer.Sequencer
}

func (i *nha044Identity) Resolve(ctx context.Context, instance domain.IntegrationInstanceID, keys []integrations.SourceKey) ([]integrations.ResolvedIdentity, error) {
	return i.repo.Resolve(ctx, instance, keys)
}
func (i *nha044Identity) AttachProjection(ctx context.Context, sink integrations.ProjectionSink) error {
	if err := i.repo.AttachProjection(ctx, sink); err != nil {
		return err
	}
	sequence, ok := sink.(*sequencer.Sequencer)
	if !ok {
		return ErrReactiveTemplateComposition
	}
	i.mu.Lock()
	i.sequence = sequence
	i.mu.Unlock()
	return nil
}
func (i *nha044Identity) Sequence() *sequencer.Sequencer {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.sequence
}

type nha044Output struct {
	instance  domain.IntegrationInstanceID
	key       integrations.SourceKey
	published chan error
	session   *integrations.Session
	binding   integrations.Binding
}

func (o *nha044Output) PublishReactiveTemplate(ctx context.Context, host *integrations.Manager, update reactivetemplate.Update) error {
	result, valid := update.CurrentResult()
	if !valid {
		return nil // No fresh calculated source exists until the fake input publishes.
	}
	if update.Diagnostic() != reactivetemplate.DiagnosticHealthy || (result.Text() != "5" && result.Text() != "6") {
		return ErrReactiveTemplateComposition
	}
	amount := 5.0
	if result.Text() == "6" {
		amount = 6
	}
	if o.session == nil {
		session, err := host.OpenSession(o.instance)
		if err != nil {
			return err
		}
		candidate, err := nha044MeasurementCandidate(o.key)
		if err != nil {
			return err
		}
		bindings, err := session.Discover(ctx, []integrations.Candidate{candidate})
		if err != nil {
			return err
		}
		o.session, o.binding = session, bindings[0]
	}
	unit, _ := domain.NewUnitCatalog().Parse("W")
	measurement, err := domain.NewMeasurement(amount, unit, 0, domain.SemanticMeasurement, domain.AggregationCurrent)
	if err != nil {
		return err
	}
	value, err := domain.NewMeasurementValue(measurement)
	if err != nil {
		return err
	}
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC).Add(time.Duration(amount-5) * time.Second)
	observation, err := integrations.NewStateObservation("power", update.Quality(), &value, &at, at, nil)
	if err != nil {
		return err
	}
	err = o.session.Publish(ctx, o.binding, observation)
	select {
	case o.published <- err:
	default:
	}
	return err
}

func nha044MeasurementCandidate(key integrations.SourceKey) (integrations.Candidate, error) {
	description := &wire.CapabilityDescription{SchemaId: contracts.MeasurementSchema, Major: 1, DisplayName: "Synthetic calculated power", Endpoint: "power", Constraints: &wire.CapabilityDescription_Measurement{Measurement: &wire.MeasurementConstraints{Unit: "W", Precision: 0, Semantic: wire.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT, Aggregation: wire.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT}}}
	return integrations.NewCandidate(key, []*wire.CapabilityDescription{description})
}

type nha044ServerNilContext struct{ context.Context }

func TestNHA044TypedNilCompositionBoundaries(t *testing.T) {
	program, err := template.Compile("{{ 'ready' }}")
	if err != nil {
		t.Fatal(err)
	}
	id, err := reactivetemplate.ParseProgramID("typed-nil")
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
	var nilOutput *nha044BlockedOutput
	if _, err := NewReactiveTemplateComposition(plan, nilOutput, reactivetemplate.RealClock{}); !errors.Is(err, ErrReactiveTemplateComposition) {
		t.Fatalf("typed nil output: %v", err)
	}
	if _, err := NewReactiveTemplateComposition(reactivetemplate.Plan{}, &nha044BlockedOutput{}, reactivetemplate.RealClock{}); !errors.Is(err, ErrReactiveTemplateComposition) {
		t.Fatalf("zero plan: %v", err)
	}
	var absentOutput ReactiveTemplateOutput
	if _, err := NewReactiveTemplateComposition(plan, absentOutput, reactivetemplate.RealClock{}); !errors.Is(err, ErrReactiveTemplateComposition) {
		t.Fatalf("absent output: %v", err)
	}
	var nilClock *reactivetemplate.RealClock
	output := &nha044BlockedOutput{}
	if _, err := NewReactiveTemplateComposition(plan, output, nilClock); !errors.Is(err, ErrReactiveTemplateComposition) {
		t.Fatalf("typed nil clock: %v", err)
	}
	composition, err := NewReactiveTemplateComposition(plan, output, reactivetemplate.RealClock{})
	if err != nil {
		t.Fatal(err)
	}
	var nilCtx *nha044ServerNilContext
	if _, err := startReactiveTemplates(nilCtx, &nha044Identity{}, &integrations.Manager{}, &sequencer.Sequencer{}, composition); !errors.Is(err, ErrReactiveTemplateComposition) {
		t.Fatalf("typed nil Start context: %v", err)
	}
	var nilIdentity *nha044Identity
	if _, err := startReactiveTemplates(context.Background(), nilIdentity, &integrations.Manager{}, &sequencer.Sequencer{}, composition); !errors.Is(err, ErrReactiveTemplateComposition) {
		t.Fatalf("typed nil identity: %v", err)
	}
	if err := (&reactiveTemplateRuntime{}).Close(nilCtx); !errors.Is(err, ErrReactiveTemplateComposition) {
		t.Fatalf("typed nil Close context: %v", err)
	}
}

func TestNHA044RealRunUsesOneProjectionAndPublishesFinalHostRevision(t *testing.T) {
	cluster := testpg.Start(t)
	data := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n", shortPrivateDataDir(t), cluster.Port))
	cfg, err := config.ParseWithEnv(data, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha044-calculated")
	key, _ := integrations.NewSourceKey("calculated", "power")
	sourceInstance, _ := domain.ParseIntegrationInstanceID("nha044-input")
	sourceKey, _ := integrations.NewSourceKey("fixture", "power")
	output := &nha044Output{instance: instance, key: key, published: make(chan error, 2)}
	var identity *nha044Identity
	var target domain.EntityID
	configuredHost := make(chan *integrations.Manager, 1)
	factory := func(ctx context.Context, store *storage.Store) (RuntimePlan, error) {
		manifest, err := registry.ParseManifest(scope.ManifestV1())
		if err != nil {
			return RuntimePlan{}, err
		}
		repo, err := registry.NewRepository(store, manifest)
		if err != nil {
			return RuntimePlan{}, err
		}
		resolved, err := repo.ConfigureBinding(ctx, instance, key, "sensor.hvac_power_consumption")
		if err != nil {
			return RuntimePlan{}, err
		}
		target = resolved.Entity
		if _, err := repo.ConfigureBinding(ctx, sourceInstance, sourceKey, "sensor.washing_machine_power"); err != nil {
			return RuntimePlan{}, err
		}
		program, err := template.Compile("{{ states('sensor.washing_machine_power') }}")
		if err != nil {
			return RuntimePlan{}, err
		}
		id, err := reactivetemplate.ParseProgramID("calculated-power")
		if err != nil {
			return RuntimePlan{}, err
		}
		definition, err := reactivetemplate.NewDefinition(id, program, nil, &target)
		if err != nil {
			return RuntimePlan{}, err
		}
		reactivePlan, err := reactivetemplate.NewPlan([]reactivetemplate.Definition{definition}, nil, time.UTC)
		if err != nil {
			return RuntimePlan{}, err
		}
		composition, err := NewReactiveTemplateComposition(reactivePlan, output, reactivetemplate.RealClock{})
		if err != nil {
			return RuntimePlan{}, err
		}
		identity = &nha044Identity{repo: repo}
		return RuntimePlan{Identity: identity, ReactiveTemplates: composition, Configure: func(_ context.Context, host *integrations.Manager, _ *storage.Store) error {
			configuredHost <- host
			return nil
		}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	ready := make(chan struct{})
	var listener net.ListenConfig
	go func() { done <- RunWithFactory(ctx, cfg, listener.Listen, func(net.Addr) { close(ready) }, factory) }()
	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not become ready")
	}
	host := <-configuredHost
	sourceSession, err := host.OpenSession(sourceInstance)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := nha044MeasurementCandidate(sourceKey)
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := sourceSession.Discover(ctx, []integrations.Candidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	if identity == nil || identity.Sequence() == nil {
		t.Fatal("Repository projection did not attach to the Run sequencer")
	}
	unit, err := domain.NewUnitCatalog().Parse("W")
	if err != nil {
		t.Fatal(err)
	}
	for step, amount := range []float64{5, 6} {
		measurement, err := domain.NewMeasurement(amount, unit, 0, domain.SemanticMeasurement, domain.AggregationCurrent)
		if err != nil {
			t.Fatal(err)
		}
		value, err := domain.NewMeasurementValue(measurement)
		if err != nil {
			t.Fatal(err)
		}
		quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC).Add(time.Duration(step) * time.Second)
		observation, err := integrations.NewStateObservation("power", quality, &value, &at, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := sourceSession.Publish(ctx, bindings[0], observation); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-output.published:
			if err != nil {
				t.Fatal(err)
			}
		case err := <-done:
			t.Fatalf("Run stopped before Host publication %d: %v", step+1, err)
		case <-time.After(15 * time.Second):
			t.Fatalf("calculated Host publication %d timed out", step+1)
		}
		snapshot, err := identity.Sequence().Capture()
		if err != nil {
			t.Fatal(err)
		}
		state, ok := snapshot.States[target]
		if !ok {
			t.Fatal("published target missing from Run projection")
		}
		_, quality, _, _, revision, err := state.Components()
		if err != nil {
			t.Fatal(err)
		}
		availability, freshness, provenance, err := quality.Components()
		if err != nil || revision != uint64(step+1) || availability != domain.AvailabilityAvailable || freshness != domain.FreshnessFresh || provenance != domain.ProvenanceEstimated {
			t.Fatalf("final revision/quality: %d %v %v %v %v", revision, availability, freshness, provenance, err)
		}
		current, present := state.CurrentValue()
		measurement, ok = current.Measurement()
		if !present || !ok {
			t.Fatal("calculated current value absent")
		}
		parts, err := measurement.Components()
		if err != nil || parts.Value != amount {
			t.Fatalf("calculated current value: %+v %v", parts, err)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not join reactive service")
	}
}

type nha044BlockedOutput struct {
	entered     chan struct{}
	release     chan struct{}
	finished    chan struct{}
	cooperative bool
}

func (o *nha044BlockedOutput) PublishReactiveTemplate(ctx context.Context, _ *integrations.Manager, _ reactivetemplate.Update) error {
	close(o.entered)
	defer close(o.finished)
	if o.cooperative {
		<-ctx.Done()
		return ctx.Err()
	}
	<-o.release // Deliberately violates the port contract to prove Store retention.
	return nil
}

func TestNHA044RunListenerFailureCancelsOutputAndRetainsStoreUntilDone(t *testing.T) {
	for _, cooperative := range []bool{true, false} {
		name := "cooperative"
		if !cooperative {
			name = "delayed"
		}
		t.Run(name, func(t *testing.T) {
			cluster := testpg.Start(t)
			data := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n", shortPrivateDataDir(t), cluster.Port))
			cfg, err := config.ParseWithEnv(data, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
			if err != nil {
				t.Fatal(err)
			}
			output := &nha044BlockedOutput{entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{}), cooperative: cooperative}
			storeReady := make(chan *storage.Store, 1)
			factory := func(_ context.Context, store *storage.Store) (RuntimePlan, error) {
				manifest, err := registry.ParseManifest(scope.ManifestV1())
				if err != nil {
					return RuntimePlan{}, err
				}
				repo, err := registry.NewRepository(store, manifest)
				if err != nil {
					return RuntimePlan{}, err
				}
				program, err := template.Compile("{{ 'ready' }}")
				if err != nil {
					return RuntimePlan{}, err
				}
				id, err := reactivetemplate.ParseProgramID("blocked")
				if err != nil {
					return RuntimePlan{}, err
				}
				definition, err := reactivetemplate.NewDefinition(id, program, nil, nil)
				if err != nil {
					return RuntimePlan{}, err
				}
				plan, err := reactivetemplate.NewPlan([]reactivetemplate.Definition{definition}, nil, time.UTC)
				if err != nil {
					return RuntimePlan{}, err
				}
				composition, err := NewReactiveTemplateComposition(plan, output, reactivetemplate.RealClock{})
				if err != nil {
					return RuntimePlan{}, err
				}
				storeReady <- store
				return RuntimePlan{Identity: &nha044Identity{repo: repo}, ReactiveTemplates: composition}, nil
			}
			listenerFailure := errors.New("synthetic listener failure")
			listen := func(context.Context, string, string) (net.Listener, error) {
				<-output.entered
				return nil, listenerFailure
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- RunWithFactory(ctx, cfg, listen, func(net.Addr) { t.Error("failed listener became ready") }, factory)
			}()
			var store *storage.Store
			select {
			case store = <-storeReady:
			case <-time.After(10 * time.Second):
				t.Fatal("factory did not receive Store")
			}
			select {
			case <-output.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("reactive output did not start")
			}
			select {
			case err := <-done:
				if !errors.Is(err, listenerFailure) {
					t.Fatalf("listener failure lost: %v", err)
				}
				if cooperative {
					select {
					case <-output.finished:
					default:
						t.Fatal("Run returned before cooperative output joined")
					}
				} else {
					select {
					case <-output.finished:
						t.Fatal("delayed output finished before release")
					default:
					}
					probeCtx, probeCancel := context.WithTimeout(context.Background(), time.Second)
					probeErr := store.InTransaction(probeCtx, func(tx pgx.Tx) error { return tx.QueryRow(probeCtx, "SELECT 1").Scan(new(int)) })
					probeCancel()
					if probeErr != nil {
						t.Fatalf("Store closed before output Done: %v", probeErr)
					}
					close(output.release)
					select {
					case <-output.finished:
					case <-time.After(2 * time.Second):
						t.Fatal("delayed output did not finish")
					}
					deadline := time.Now().Add(5 * time.Second)
					for {
						probeCtx, probeCancel := context.WithTimeout(context.Background(), time.Second)
						probeErr = store.InTransaction(probeCtx, func(tx pgx.Tx) error { return tx.QueryRow(probeCtx, "SELECT 1").Scan(new(int)) })
						probeCancel()
						if probeErr != nil {
							break
						}
						if time.Now().After(deadline) {
							t.Fatal("Store stayed open after reactive Done")
						}
						time.Sleep(10 * time.Millisecond)
					}
				}
			case <-time.After(12 * time.Second):
				if !cooperative {
					close(output.release)
				}
				t.Fatal("Run did not finish bounded listener failure")
			}
		})
	}
}

func TestNHA044PreparationFailureCancelsBlockedOutput(t *testing.T) {
	cluster := testpg.Start(t)
	data := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n", shortPrivateDataDir(t), cluster.Port))
	cfg, err := config.ParseWithEnv(data, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	output := &nha044BlockedOutput{entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{}), cooperative: true}
	badPublic := &PublicPlan{}
	badPublic.mu.Lock()
	locked := true
	defer func() {
		if locked {
			badPublic.mu.Unlock()
		}
	}()
	factory := func(_ context.Context, store *storage.Store) (RuntimePlan, error) {
		manifest, err := registry.ParseManifest(scope.ManifestV1())
		if err != nil {
			return RuntimePlan{}, err
		}
		repo, err := registry.NewRepository(store, manifest)
		if err != nil {
			return RuntimePlan{}, err
		}
		program, err := template.Compile("{{ 'ready' }}")
		if err != nil {
			return RuntimePlan{}, err
		}
		id, err := reactivetemplate.ParseProgramID("preparation-failure")
		if err != nil {
			return RuntimePlan{}, err
		}
		definition, err := reactivetemplate.NewDefinition(id, program, nil, nil)
		if err != nil {
			return RuntimePlan{}, err
		}
		plan, err := reactivetemplate.NewPlan([]reactivetemplate.Definition{definition}, nil, time.UTC)
		if err != nil {
			return RuntimePlan{}, err
		}
		composition, err := NewReactiveTemplateComposition(plan, output, reactivetemplate.RealClock{})
		if err != nil {
			return RuntimePlan{}, err
		}
		return RuntimePlan{Identity: &nha044Identity{repo: repo}, ReactiveTemplates: composition, Public: badPublic}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		var listener net.ListenConfig
		done <- RunWithFactory(ctx, cfg, listener.Listen, func(net.Addr) { t.Error("invalid public plan became ready") }, factory)
	}()
	select {
	case <-output.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("reactive output did not block before public preparation")
	}
	badPublic.mu.Unlock()
	locked = false
	select {
	case err := <-done:
		if !errors.Is(err, ErrInvalidPublicPlan) {
			t.Fatalf("preparation failure lost: %v", err)
		}
		select {
		case <-output.finished:
		default:
			t.Fatal("Run returned before blocked output observed cancellation")
		}
	case <-time.After(12 * time.Second):
		t.Fatal("preparation failure did not drain reactive output")
	}
}
