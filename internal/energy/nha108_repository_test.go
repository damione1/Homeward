package energy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

type nha108UnknownStore struct {
	base   *storage.Store
	commit bool
}

func (s nha108UnknownStore) InTransaction(ctx context.Context, fn func(pgx.Tx) error) error {
	return s.base.InTransaction(ctx, fn)
}
func (s nha108UnknownStore) InTransactionOutcome(ctx context.Context, fn func(pgx.Tx) error) (storage.TransactionOutcome, error) {
	if s.commit {
		outcome, err := s.base.InTransactionOutcome(ctx, fn)
		if outcome != storage.TransactionCommitted || err != nil {
			return outcome, err
		}
	}
	return storage.TransactionIndeterminate, errors.New("synthetic lost commit acknowledgment")
}

type nha108LostAckHost struct{ integrations.SessionHost }

func (h nha108LostAckHost) Publish(ctx context.Context, b integrations.Binding, o integrations.StateObservation) error {
	if err := h.SessionHost.Publish(ctx, b, o); err != nil {
		return err
	}
	return errors.New("synthetic lost Host acknowledgment")
}
func nha108Store(t *testing.T, port int) *storage.Store {
	t.Helper()
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		store.Close()
		t.Fatal(err)
	}
	return store
}
func nha108Output(t *testing.T, r *nha108Rig) (*integrations.Session, integrations.Binding, domain.EntityID) {
	t.Helper()
	instance, _ := domain.ParseIntegrationInstanceID("nha108-derived")
	session, err := r.host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := integrations.NewSourceKey("computed-energy", "counter")
	description := &wireregistry.CapabilityDescription{SchemaId: contracts.MeasurementSchema, Major: 1, DisplayName: "Estimated energy", Endpoint: "energy", Constraints: &wireregistry.CapabilityDescription_Measurement{Measurement: &wireregistry.MeasurementConstraints{Unit: "kWh", Precision: 3, Semantic: wireregistry.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL, Aggregation: wireregistry.MeasurementAggregation_MEASUREMENT_AGGREGATION_SUM}}}
	candidate, err := integrations.NewCandidate(key, []*wireregistry.CapabilityDescription{description})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	_, output, err := bindings[0].IDs()
	if err != nil {
		t.Fatal(err)
	}
	return session, bindings[0], output
}
func TestNHA108CheckpointCASUnknownOutcomeAndPendingHostReplay(t *testing.T) {
	cluster := testpg.Start(t)
	store := nha108Store(t, cluster.Port)
	r := nha108NewRig(t, "W")
	outputSession, outputBinding, outputID := nha108Output(t, r)
	r.output = outputID
	profile := nha108Profile(t, r, MethodTrapezoidal, TimeBasisCaptureRequired, time.Hour, 0, 3)
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	initial, err := repository.Load(ctx, profile)
	if err != nil {
		t.Fatal(err)
	}
	at := r.clock.Now()
	first := r.publish(t, 1000, &at, domain.AvailabilityAvailable, domain.ProvenanceObserved, 1)
	one := nha108Advance(t, profile, initial, r, first)
	savedOne, err := repository.Save(ctx, initial, one)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Save(ctx, initial, one); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale CAS: %v", err)
	}
	unit, _ := domain.NewUnitCatalog().Parse("kWh")
	changedProfile, err := NewProfile("nha108-fixture-v1", 2, r.source, r.output, MethodTrapezoidal, TimeBasisCaptureRequired, unit, 3, domain.SemanticTotal, time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Load(ctx, changedProfile); !errors.Is(err, ErrProfileChanged) {
		t.Fatalf("profile change silently reinterpreted counter: %v", err)
	}
	r.clock.set(at.Add(time.Hour), time.Hour)
	later := at.Add(time.Hour)
	second := r.publish(t, 1000, &later, domain.AvailabilityAvailable, domain.ProvenanceObserved, 2)
	two := nha108Advance(t, profile, savedOne, r, second)
	unknownCommitted := &Repository{store: nha108UnknownStore{base: store, commit: true}}
	savedTwo, err := unknownCommitted.Save(ctx, savedOne, two)
	if err != nil || !savedTwo.hasPending {
		t.Fatalf("known by reread: %v %+v", err, savedTwo)
	}
	gap, err := BreakContinuity(savedTwo, DiagnosticDeliveryLoss, at.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	unknownRolledBack := &Repository{store: nha108UnknownStore{base: store, commit: false}}
	if _, err := unknownRolledBack.Save(ctx, savedTwo, gap); !errors.Is(err, ErrConflict) {
		t.Fatalf("unknown without commit accepted: %v", err)
	}
	pilot := &Pilot{repository: repository}
	item := &counter{binding: outputBinding, state: savedTwo}
	if err := pilot.publishPending(ctx, nha108LostAckHost{outputSession}, item); err == nil {
		t.Fatal("lost acknowledgment not signaled")
	}
	if !item.state.hasPending {
		t.Fatal("unacknowledged publication was erased")
	}
	state, ok := r.fixture.State(outputID)
	if !ok {
		t.Fatal("Host did not receive committed energy")
	}
	_, quality, _, _, _, err := state.Components()
	if err != nil {
		t.Fatal(err)
	}
	_, freshness, provenance, err := quality.Components()
	if err != nil || freshness != domain.FreshnessFresh || provenance != domain.ProvenanceEstimated {
		t.Fatalf("derived quality: %v %v %v", freshness, provenance, err)
	}
	store.Close()
	reopened := nha108Store(t, cluster.Port)
	defer reopened.Close()
	replayRepo, _ := NewRepository(reopened)
	loaded, err := replayRepo.Load(ctx, profile)
	if err != nil || !loaded.hasPending || loaded.pending.quantized != 1000 {
		t.Fatalf("pending reopen: %+v %v", loaded, err)
	}
	pilot.repository = replayRepo
	item.state = loaded
	if err := pilot.publishPending(ctx, outputSession, item); err != nil {
		t.Fatal(err)
	}
	final, err := replayRepo.Load(ctx, profile)
	if err != nil || final.hasPending || final.totalNum != loaded.totalNum || final.totalDen != loaded.totalDen {
		t.Fatalf("replay reintegrated energy: %+v %v", final, err)
	}
	if _, changed, err := AdvanceReport(profile, final, second, r.epoch); err != nil || changed {
		t.Fatalf("durable duplicate advanced: %t %v", changed, err)
	}
}

func TestNHA108RepositoryConcurrentCASKeepsSingleRevision(t *testing.T) {
	cluster := testpg.Start(t)
	store := nha108Store(t, cluster.Port)
	defer store.Close()
	r := nha108NewRig(t, "W")
	profile := nha108Profile(t, r, MethodTrapezoidal, TimeBasisCaptureRequired, time.Hour, 0, 3)
	repository, _ := NewRepository(store)
	ctx := context.Background()
	initial, err := repository.Load(ctx, profile)
	if err != nil {
		t.Fatal(err)
	}
	at := r.clock.Now()
	entry := r.publish(t, 1000, &at, domain.AvailabilityAvailable, domain.ProvenanceObserved, 1)
	one := nha108Advance(t, profile, initial, r, entry)
	one, err = repository.Save(ctx, initial, one)
	if err != nil {
		t.Fatal(err)
	}
	left, err := BreakContinuity(one, DiagnosticDeliveryLoss, at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	right, err := BreakContinuity(one, DiagnosticUnavailable, at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	go func() { _, e := repository.Save(ctx, one, left); results <- e }()
	go func() { _, e := repository.Save(ctx, one, right); results <- e }()
	committed, conflicted := 0, 0
	for i := 0; i < 2; i++ {
		e := <-results
		if e == nil {
			committed++
		} else if errors.Is(e, ErrConflict) {
			conflicted++
		} else {
			t.Fatalf("CAS result: %v", e)
		}
	}
	if committed != 1 || conflicted != 1 {
		t.Fatalf("CAS winners: %d conflicts: %d", committed, conflicted)
	}
	loaded, err := repository.Load(ctx, profile)
	if err != nil || loaded.Revision() != one.Revision()+1 {
		t.Fatalf("CAS revision: %+v %v", loaded, err)
	}
}
