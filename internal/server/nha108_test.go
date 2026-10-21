package server

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/energy"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
)

type nha108SourcePilot struct {
	ready <-chan struct{}
	done  chan error
	key   integrations.SourceKey
}

func (p *nha108SourcePilot) Start(_ context.Context, host integrations.SessionHost, tasks integrations.Tasks) error {
	return tasks.Go(func(ctx context.Context) error {
		select {
		case <-p.ready:
		case <-ctx.Done():
			return nil
		}
		description := &wireregistry.CapabilityDescription{SchemaId: contracts.MeasurementSchema, Major: 1, DisplayName: "Synthetic HVAC power", Endpoint: "power", Constraints: &wireregistry.CapabilityDescription_Measurement{Measurement: &wireregistry.MeasurementConstraints{Unit: "W", Precision: 0, Semantic: wireregistry.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT, Aggregation: wireregistry.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT}}}
		candidate, err := integrations.NewCandidate(p.key, []*wireregistry.CapabilityDescription{description})
		if err != nil {
			p.done <- err
			return err
		}
		bindings, err := host.Discover(ctx, []integrations.Candidate{candidate})
		if err != nil {
			p.done <- err
			return err
		}
		unit, _ := domain.NewUnitCatalog().Parse("W")
		measurement, err := domain.NewMeasurement(1000, unit, 0, domain.SemanticMeasurement, domain.AggregationCurrent)
		if err != nil {
			p.done <- err
			return err
		}
		value, err := domain.NewMeasurementValue(measurement)
		if err != nil {
			p.done <- err
			return err
		}
		quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		if err != nil {
			p.done <- err
			return err
		}
		first := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
		for i, at := range []time.Time{first, first.Add(time.Hour)} {
			seq := uint64(i + 1)
			observation, e := integrations.NewStateObservation("power", quality, &value, &at, time.Now().UTC(), &seq)
			if e != nil {
				p.done <- e
				return e
			}
			if e = host.Publish(ctx, bindings[0], observation); e != nil {
				p.done <- e
				return e
			}
		}
		p.done <- nil
		<-ctx.Done()
		return nil
	})
}
func (*nha108SourcePilot) Stop(context.Context) error { return nil }

func TestNHA108RunWithFactoryComposesDurableEnergyBeforeReady(t *testing.T) {
	cluster := testpg.Start(t)
	configuration := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\nenergy:\n  - source: sensor.hvac_power_consumption\n    output: sensor.hvac_energy_consumption\n    precision: 3\n    max_gap: 1h\n    time_basis: capture_required\n", shortPrivateDataDir(t), cluster.Port))
	startup := nha029RunStartupFromRaw(t, configuration)
	instance, _ := domain.ParseIntegrationInstanceID("nha108-source-run")
	key, _ := integrations.NewSourceKey("synthetic-hvac", "power")
	ready := make(chan struct{})
	published := make(chan error, 1)
	storeReady := make(chan *storage.Store, 1)
	source := &nha108SourcePilot{ready: ready, done: published, key: key}
	spec, err := integrations.NewPilotSpec(instance, func() integrations.Pilot { return source })
	if err != nil {
		t.Fatal(err)
	}
	factory := func(_ context.Context, store *storage.Store) (RuntimePlan, error) {
		manifest, e := registry.ParseManifest(scope.ManifestV1())
		if e != nil {
			return RuntimePlan{}, e
		}
		identities, e := registry.NewRepository(store, manifest)
		if e != nil {
			return RuntimePlan{}, e
		}
		storeReady <- store
		return RuntimePlan{Identity: identities, Pilots: []integrations.PilotSpec{spec}, Configure: func(ctx context.Context, _ *integrations.Manager, _ *storage.Store) error {
			_, e := identities.ConfigureBinding(ctx, instance, key, "sensor.hvac_power_consumption")
			return e
		}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var listener net.ListenConfig
	go func() {
		done <- RunWithStartupFactory(ctx, startup, listener.Listen, func(net.Addr) { close(ready) }, factory)
	}()
	var store *storage.Store
	select {
	case store = <-storeReady:
	case err := <-done:
		t.Fatalf("Run failed before factory: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("factory timeout")
	}
	select {
	case err := <-published:
		if err != nil {
			t.Fatal(err)
		}
	case err := <-done:
		t.Fatalf("Run stopped before synthetic reports: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("source report timeout")
	}
	manifest, _ := registry.ParseManifest(scope.ManifestV1())
	identities, _ := registry.NewRepository(store, manifest)
	lookupCtx, lookupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer lookupCancel()
	sourceID, err := identities.LookupAlias(lookupCtx, "sensor.hvac_power_consumption")
	if err != nil {
		t.Fatal(err)
	}
	outputID, err := identities.LookupAlias(lookupCtx, "sensor.hvac_energy_consumption")
	if err != nil {
		t.Fatal(err)
	}
	unit, _ := domain.NewUnitCatalog().Parse("kWh")
	profile, err := energy.NewProfile("mvp-trapezoidal-v1", 1, sourceID.Entity, outputID.Entity, energy.MethodTrapezoidal, energy.TimeBasisCaptureRequired, unit, 3, domain.SemanticTotal, time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	repository, _ := energy.NewRepository(store)
	deadline := time.After(10 * time.Second)
	for {
		checkpoint, e := repository.Load(lookupCtx, profile)
		if e == nil && checkpoint.Revision() > 0 {
			num, den, fractionErr := checkpoint.TotalFraction()
			if _, ok := checkpoint.Pending(); !ok && fractionErr == nil && num == "1" && den == "1" && checkpoint.Coverage() == energy.CoverageContinuous {
				break
			}
		}
		select {
		case <-deadline:
			t.Fatalf("energy did not commit and publish: checkpoint=%+v err=%v", checkpoint, e)
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Run did not stop after energy pilot")
	}
}
