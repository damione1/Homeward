package server

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/backups"
	"github.com/damione1/Homeward/internal/backups/installation"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
	"github.com/jackc/pgx/v5"
)

// This pilot has no network or CommandDriver. Its only operation is publishing
// a new local synthetic read after each real Run starts.
type nha035ReadOnlyUPS struct {
	key  integrations.SourceKey
	load float64
}

func (p *nha035ReadOnlyUPS) Start(ctx context.Context, host integrations.SessionHost, _ integrations.Tasks) error {
	description := &registrywire.CapabilityDescription{SchemaId: contracts.MeasurementSchema, Major: 1, Endpoint: "load", DisplayName: "NHA035 local UPS", Constraints: &registrywire.CapabilityDescription_Measurement{Measurement: &registrywire.MeasurementConstraints{Unit: "%", Precision: 1, Semantic: registrywire.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT, Aggregation: registrywire.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT}}}
	candidate, err := integrations.NewCandidate(p.key, []*registrywire.CapabilityDescription{description})
	if err != nil {
		return err
	}
	bindings, err := host.Discover(ctx, []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		return integrations.ErrInvalidInput
	}
	unit, err := domain.NewUnitCatalog().Parse("%")
	if err != nil {
		return err
	}
	measurement, err := domain.NewMeasurement(p.load, unit, 1, domain.SemanticMeasurement, domain.AggregationCurrent)
	if err != nil {
		return err
	}
	value, err := domain.NewMeasurementValue(measurement)
	if err != nil {
		return err
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		return err
	}
	at := time.Now()
	observation, err := integrations.NewStateObservation("load", quality, &value, &at, at, nil)
	if err != nil {
		return err
	}
	return host.Publish(ctx, bindings[0], observation)
}
func (*nha035ReadOnlyUPS) Stop(context.Context) error { return nil }

type nha035DerivedRun struct {
	store    *storage.Store
	sequence *sequencer.Sequencer
	archives BackupArchiveServices
	cancel   context.CancelFunc
	done     chan error
}

func nha035StartDerived(t *testing.T, ctx context.Context, startup config.StartupInputs, load float64, capture bool) *nha035DerivedRun {
	t.Helper()
	runCtx, cancel := context.WithCancel(ctx)
	result := &nha035DerivedRun{cancel: cancel, done: make(chan error, 1)}
	ready := make(chan struct{})
	factory := func(callback context.Context, store *storage.Store) (RuntimePlan, error) {
		result.store = store
		manifest, err := registry.ParseManifest(scope.ManifestV1())
		if err != nil {
			return RuntimePlan{}, err
		}
		identities, err := registry.NewRepository(store, manifest)
		if err != nil {
			return RuntimePlan{}, err
		}
		instance, _ := domain.ParseIntegrationInstanceID("nha035-ups")
		key, _ := integrations.NewSourceKey("fixture-ups", "load")
		if _, err = identities.ConfigureBinding(callback, instance, key, "sensor.ups_load"); err != nil {
			return RuntimePlan{}, err
		}
		pilot := &nha035ReadOnlyUPS{key, load}
		spec, err := integrations.NewPilotSpec(instance, func() integrations.Pilot { return pilot })
		if err != nil {
			return RuntimePlan{}, err
		}
		plan := RuntimePlan{Identity: identities, Pilots: []integrations.PilotSpec{spec}, Configure: func(context.Context, *integrations.Manager, *storage.Store) error { return nil }}
		if capture {
			plan.ConfigureBackupArchives = func(_ context.Context, services BackupArchiveServices) error { result.archives = services; return nil }
		}
		return plan, nil
	}
	go func() {
		result.done <- runWithDependencies(runCtx, startup, (&net.ListenConfig{}).Listen, func(net.Addr) { close(ready) }, factory, runDependencies{observe: func(_ context.Context, sequence *sequencer.Sequencer) error { result.sequence = sequence; return nil }})
	}()
	select {
	case <-ready:
	case err := <-result.done:
		cancel()
		t.Fatalf("derived Run failed before ready: %v", err)
	case <-ctx.Done():
		cancel()
		t.Fatal("derived Run readiness timeout")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-result.done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(12 * time.Second):
			t.Error("derived restored Run did not join")
		}
	})
	return result
}
func nha035WaitDerived(t *testing.T, run *nha035DerivedRun, want float64) domain.EntityID {
	t.Helper()
	var identity domain.EntityID
	nha042ServerWait(t, "fresh derived power after isolated Run", func() bool {
		snapshot, err := run.sequence.Capture()
		if err != nil {
			return false
		}
		for entity, record := range snapshot.Registry {
			original, _, _, err := record.Aliases()
			if err != nil {
				continue
			}
			alias, _ := original.Value()
			if alias != "sensor.ups_power_consumption" {
				continue
			}
			state, present := snapshot.States[entity]
			if !present {
				continue
			}
			value, present := state.CurrentValue()
			if !present {
				continue
			}
			measurement, present := value.Measurement()
			if !present {
				continue
			}
			parts, err := measurement.Components()
			if err != nil || parts.Value != want {
				continue
			}
			_, quality, _, _, _, err := state.Components()
			if err != nil {
				continue
			}
			availability, freshness, provenance, err := quality.Components()
			if err == nil && availability == domain.AvailabilityAvailable && freshness == domain.FreshnessFresh && provenance == domain.ProvenanceEstimated {
				identity = entity
				return true
			}
		}
		return false
	})
	return identity
}
func TestNHA035RunRecomputesDerivedFromNewLocalRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	source := testpg.Start(t)
	data := shortPrivateDataDir(t)
	public, private := nha035Recovery(t)
	publicText, err := public.PublicText()
	if err != nil {
		t.Fatal(err)
	}
	raw := append(nha035Bootstrap(source.Port, data), []byte("derived_power: mvp-v1\nbackup_recovery_recipient: "+publicText+"\n")...)
	startup, err := config.NewDetachedStartup(raw, "configuration.yaml", nil, nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	old := nha035StartDerived(t, ctx, startup, 75, true)
	oldEntity := nha035WaitDerived(t, old, 675)
	oldText, _ := oldEntity.Value()
	nha042ServerWait(t, "old derived result committed to actual Recorder", func() bool {
		var count int
		err := old.store.InTransaction(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM public.state_history WHERE entity_id=$1`, oldText).Scan(&count)
		})
		return err == nil && count > 0
	})
	service, err := old.archives.Installation()
	if err != nil {
		t.Fatal("035 did not borrow actual Run Store/key", err)
	}
	tools := nha035Tools(t)
	id, _ := backups.NewArtifactID()
	sealed, err := service.Create(ctx, tools, id)
	if err != nil {
		t.Fatal(err)
	}
	payload := nha035Export(t, sealed)
	// The new isolated Run reads 25%, so a restored old 675W result would be wrong.
	target := testpg.Start(t)
	parent := shortPrivateDataDir(t)
	destination := filepath.Join(parent, "restored")
	bootstrap := nha035Bootstrap(target.Port, destination)
	intervention := time.Now()
	result, err := installation.Restore(ctx, bytes.NewReader(payload), id, private, parent, "restored", tools, bootstrap, nha029TestEnv)
	if err != nil || result.State() != installation.Restored {
		t.Fatal(err)
	}
	freshStartup, err := config.NewDetachedStartup(raw, "configuration.yaml", bootstrap, nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	fresh := nha035StartDerived(t, ctx, freshStartup, 25, false)
	var historical int
	if err = fresh.store.InTransaction(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM public.state_history WHERE entity_id=$1`, oldText).Scan(&historical)
	}); err != nil || historical == 0 {
		t.Fatal("old derived history was not archived and retained", err)
	}
	freshEntity := nha035WaitDerived(t, fresh, 225)
	serverRTO := time.Since(intervention)
	if serverRTO > 30*time.Minute {
		t.Fatalf("synthetic server RTO exceeded target: %s", serverRTO)
	}
	t.Logf("MEASURED_SYNTHETIC server_RTO_after_intervention=%s archive_snapshot_age=%s bytes=%d", serverRTO, result.Measurement().DataAge, len(payload))
	if freshEntity != oldEntity {
		t.Fatal("restoration changed the derived entity identity")
	}
	state, present, err := fresh.sequence.State(freshEntity)
	if err != nil || !present {
		t.Fatal(err)
	}
	value, present := state.CurrentValue()
	if !present {
		t.Fatal("missing recomputed value")
	}
	measurement, present := value.Measurement()
	parts, err := measurement.Components()
	if !present || err != nil || parts.Value == 675 {
		t.Fatal("archived derived state was replayed")
	}
}
