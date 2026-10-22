package registry_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
)

func TestNHA055CompilationGatePreparesBindingsWithoutReacquiringAdmission(t *testing.T) {
	store, ctx, _ := nha015Store(t)
	repo := nha015Repository(t, store)
	_, sink, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.AttachProjection(ctx, sink); err != nil {
		t.Fatal(err)
	}
	instance := nha015Instance(t, "nha055-borrowed-preparation")
	key := nha015Source(t, "nha055-borrowed-device", "relay")
	const original = "switch.arc_soundbar_crossfade"
	request, err := registry.NewBindingRequest(instance, key, original)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := repo.BeginActivation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The short context makes the original self-deadlock observable. The
	// successful result still comes from real SQL and the real held gate.
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	prepared, err := gate.PrepareBindings(bounded, []registry.BindingRequest{request})
	if err != nil {
		t.Fatalf("held gate reacquired its own admission: %v", err)
	}
	prospective, err := prepared.LookupAlias(original)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prospective.Entity.Value(); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("NHA055 rollback after borrowed prospective binding")
	outcome, err := store.InConfigTransactionOutcome(ctx, func(config *storage.ConfigTx) error {
		if err := gate.ApplyInConfigTx(config, func(writer *registry.ConfigWriter) error { return writer.ApplyPreparedBindings(prepared) }); err != nil {
			return err
		}
		return stop
	})
	if outcome != storage.TransactionNotCommitted || !errors.Is(err, stop) {
		t.Fatalf("prospective preparation escaped rollback: %v %v", outcome, err)
	}
	if err := gate.ResolveNotCommitted(outcome); err != nil {
		t.Fatal(err)
	}
	if err := gate.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.PrepareBindings(ctx, []registry.BindingRequest{request}); !errors.Is(err, registry.ErrInvalidInput) {
		t.Fatalf("released gate still prepared identities: %v", err)
	}
	if _, err := repo.Resolve(ctx, instance, []integrations.SourceKey{key}); !errors.Is(err, registry.ErrUnconfigured) {
		t.Fatalf("prospective identity became durable: %v", err)
	}
	gate, err = repo.BeginActivation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancelNow := context.WithCancel(ctx)
	cancelNow()
	if _, err := gate.PrepareBindings(canceled, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled preparation admitted: %v", err)
	}
	if err := gate.Release(); err != nil {
		t.Fatalf("failed preparation retained the gate phase: %v", err)
	}
}
