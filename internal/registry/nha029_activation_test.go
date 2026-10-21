package registry_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"testing"

	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
)

func TestNHA029RegistryUsesSameConfigTransactionAndRollsBackWithIt(t *testing.T) {
	store, ctx, settings := nha015Store(t)
	repo := nha015Repository(t, store)
	_, sink, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.AttachProjection(ctx, sink); err != nil {
		t.Fatal(err)
	}
	instance := nha015Instance(t, "mqtt-main")
	key := nha015Source(t, "nha029-transaction-device", "switch")
	alias := "switch.arc_soundbar_crossfade"
	gate, err := repo.BeginActivation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rollback := errors.New("NHA029 test rollback after registry write")
	outcome, err := store.InConfigTransactionOutcome(ctx, func(config *storage.ConfigTx) error {
		first := gate.ApplyInConfigTx(config, func(writer *registry.ConfigWriter) error {
			if _, err := writer.ConfigureBinding(instance, key, alias); err != nil {
				return err
			}
			return rollback
		})
		if err := gate.ApplyInConfigTx(config, func(*registry.ConfigWriter) error { return nil }); !errors.Is(err, registry.ErrInvalidInput) {
			t.Fatalf("pending gate accepted second Apply: %v", err)
		}
		return first
	})
	if outcome != storage.TransactionNotCommitted || !errors.Is(err, rollback) {
		t.Fatalf("registry write escaped config rollback: outcome=%v err=%v", outcome, err)
	}
	if err := gate.Release(); !errors.Is(err, registry.ErrProjectionUnavailable) {
		t.Fatalf("pending gate released before outcome resolution: %v", err)
	}
	if err := gate.ResolveNotCommitted(storage.TransactionIndeterminate); !errors.Is(err, registry.ErrInvalidInput) {
		t.Fatalf("unknown SQL outcome resolved as rollback: %v", err)
	}
	if err := gate.ResolveNotCommitted(outcome); err != nil {
		t.Fatal(err)
	}
	if err := gate.ResolveNotCommitted(outcome); !errors.Is(err, registry.ErrInvalidInput) {
		t.Fatalf("gate resolved rollback twice: %v", err)
	}
	if err := gate.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Resolve(ctx, instance, []integrations.SourceKey{key}); !errors.Is(err, registry.ErrUnconfigured) {
		t.Fatalf("rolled-back identity became visible: %v", err)
	}
	gate, err = repo.BeginActivation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var configured integrations.ResolvedIdentity
	var escapedWriter *registry.ConfigWriter
	secrets, err := store.BootstrapSecrets(ctx, filepath.Join(t.TempDir(), "private-data"))
	if err != nil {
		t.Fatal(err)
	}
	replay, err := storage.NewConfigReplayRepository(store, secrets)
	if err != nil {
		t.Fatal(err)
	}
	document, err := storage.NewConfigDocumentID("operational.main")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := storage.NewConfigDocumentBinding(document, "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	author, err := storage.NewConfigAuthor("nha029:registry-test")
	if err != nil {
		t.Fatal(err)
	}
	auditSource, err := storage.NewConfigSource("nha029:activation")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := storage.NewConfigVersionMetadata(author, auditSource)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := storage.RandomActivationAttemptID()
	if err != nil {
		t.Fatal(err)
	}
	operational := []byte("external_workers: []\n")
	var target storage.ConfigGenerationRevision
	outcome, err = store.InConfigTransactionOutcome(ctx, func(config *storage.ConfigTx) error {
		if err := gate.ApplyInConfigTx(config, func(writer *registry.ConfigWriter) error {
			escapedWriter = writer
			var writeErr error
			configured, writeErr = writer.ConfigureBinding(instance, key, alias)
			return writeErr
		}); err != nil {
			return err
		}
		ref, err := config.SaveProtectedOperationalSource(replay, binding, operational, sha256.Sum256(operational), metadata)
		if err != nil {
			return err
		}
		target, err = config.RegisterCompiledGeneration(binding.Path(), sha256.Sum256([]byte("nha029 compiled candidate")), []storage.ConfigVersionRef{ref})
		if err != nil {
			return err
		}
		if err := config.ActivateGeneration(storage.ExpectNoConfigActive(), target); err != nil {
			return err
		}
		return config.RecordActivationAttempt(attempt, metadata)
	})
	if outcome != storage.TransactionCommitted || err != nil {
		t.Fatalf("registry write did not share config commit: outcome=%v err=%v", outcome, err)
	}
	active, present, err := store.ReadActiveConfig(ctx)
	if err != nil || !present || active.Revision() != target {
		t.Fatalf("same ConfigTx did not publish active: present=%t err=%v", present, err)
	}
	if err := gate.Release(); !errors.Is(err, registry.ErrProjectionUnavailable) {
		t.Fatalf("committed gate released before projection: %v", err)
	}
	if err := gate.PublishCommitted(ctx, target, active.ManifestSHA256()); err != nil {
		t.Fatal(err)
	}
	if err := gate.PublishCommitted(ctx, target, active.ManifestSHA256()); !errors.Is(err, registry.ErrInvalidInput) {
		t.Fatalf("gate published committed projection twice: %v", err)
	}
	if err := gate.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := escapedWriter.ConfigureBinding(instance, key, alias); !errors.Is(err, registry.ErrInvalidInput) {
		t.Fatalf("retained registry writer escaped ConfigTx callback: %v", err)
	}
	reopened, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	other := nha015Repository(t, reopened)
	got, err := other.Resolve(ctx, instance, []integrations.SourceKey{key})
	if err != nil || len(got) != 1 || got[0] != configured {
		t.Fatalf("independent Store did not observe committed identity: got=%v err=%v", got, err)
	}
}

type nha029RegistryNilContext struct{ context.Context }

func TestNHA029RegistryActivationGateRejectsZeroTypedNilAndDoubleRelease(t *testing.T) {
	var zero registry.Repository
	if gate, err := zero.BeginActivation(context.Background()); gate != nil || !errors.Is(err, registry.ErrInvalidInput) {
		t.Fatalf("zero repository admitted activation gate: gate=%v err=%v", gate, err)
	}
	var zeroGate registry.ActivationGate
	if err := zeroGate.Release(); !errors.Is(err, registry.ErrInvalidInput) {
		t.Fatalf("zero activation gate released: %v", err)
	}
	if err := zeroGate.ResolveNotCommitted(storage.TransactionNotCommitted); !errors.Is(err, registry.ErrInvalidInput) {
		t.Fatalf("zero activation gate resolved: %v", err)
	}
	store, ctx, _ := nha015Store(t)
	repo := nha015Repository(t, store)
	_, sink, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.AttachProjection(ctx, sink); err != nil {
		t.Fatal(err)
	}
	var typedNil *nha029RegistryNilContext
	if gate, err := repo.BeginActivation(typedNil); gate != nil || !errors.Is(err, registry.ErrInvalidInput) {
		t.Fatalf("typed-nil context admitted activation gate: gate=%v err=%v", gate, err)
	}
	gate, err := repo.BeginActivation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.Release(); err != nil {
		t.Fatal(err)
	}
	if err := gate.Release(); !errors.Is(err, registry.ErrInvalidInput) {
		t.Fatalf("released gate accepted a second release: %v", err)
	}
}

func TestNHA029RegistryBorrowRejectsForeignStoreAndEscapedHandle(t *testing.T) {
	store, ctx, settings := nha015Store(t)
	foreign, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	var escaped *storage.RegistryBorrow
	outcome, err := store.InConfigTransactionOutcome(ctx, func(config *storage.ConfigTx) error {
		// The caller deliberately swallows the error. ConfigTx must still fail.
		_ = config.WithRegistryBorrow(foreign, func(*storage.RegistryBorrow) error { return nil })
		return nil
	})
	if outcome != storage.TransactionNotCommitted || err == nil {
		t.Fatalf("foreign Store borrow was not poisonous: outcome=%v err=%v", outcome, err)
	}
	outcome, err = store.InConfigTransactionOutcome(ctx, func(config *storage.ConfigTx) error {
		return config.WithRegistryBorrow(store, func(borrow *storage.RegistryBorrow) error {
			escaped = borrow
			return nil
		})
	})
	if outcome != storage.TransactionCommitted || err != nil || escaped == nil {
		t.Fatalf("same-Store borrow was not admitted: outcome=%v err=%v", outcome, err)
	}
	if _, err := escaped.Exec(storage.RegistryNewDevice(), "dev_escaped"); err == nil {
		t.Fatal("escaped registry borrow executed after callback")
	}
	var entity string
	if err := escaped.QueryRow(storage.RegistryManifestEntity(), "switch.arc_soundbar_crossfade").Scan(&entity); err == nil {
		t.Fatal("escaped registry row scanned after callback")
	}
}

func TestNHA029RegistryRowCreatedUnderBorrowCannotScanAfterCallback(t *testing.T) {
	store, ctx, _ := nha015Store(t)
	var escapedRow interface{ Scan(...any) error }
	forced := errors.New("rollback after live row escaped its borrow")
	outcome, err := store.InConfigTransactionOutcome(ctx, func(config *storage.ConfigTx) error {
		if err := config.WithRegistryBorrow(store, func(borrow *storage.RegistryBorrow) error {
			escapedRow = borrow.QueryRow(storage.RegistryManifestEntity(), "switch.arc_soundbar_crossfade")
			return nil
		}); err != nil {
			return err
		}
		var entity string
		var closed *storage.Error
		if err := escapedRow.Scan(&entity); !errors.As(err, &closed) || closed.Kind() != storage.ConfigTransactionClosed {
			return errors.New("registry row did not reject with ConfigTransactionClosed after its borrow callback")
		}
		return forced
	})
	if outcome != storage.TransactionNotCommitted || !errors.Is(err, forced) {
		t.Fatalf("escaped row changed owner transaction outcome: %v %v", outcome, err)
	}
}
