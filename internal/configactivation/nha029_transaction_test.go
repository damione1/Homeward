package configactivation

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
)

func TestNHA029PreparedSourceRegistryAndAttemptShareOneCAS(t *testing.T) {
	store, ctx, port := nha029Store(t)
	root := t.TempDir()
	path := filepath.Join(root, "configuration.yaml")
	raw := append(nha029Launch(port, filepath.Join(root, "private-data")), []byte("external_workers: []\n")...)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029Lookup)
	if err != nil {
		t.Fatal(err)
	}
	old, _, err := ReconcileStartup(ctx, store, startup)
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := old.Secrets()
	if err != nil {
		t.Fatal(err)
	}
	replay, err := storage.NewConfigReplayRepository(store, secrets)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := registry.ParseManifest(scope.ManifestV1())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := registry.NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	_, sink, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.AttachProjection(ctx, sink); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, []byte("# exact next source\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	author, err := storage.NewConfigAuthor("nha029:test-operator")
	if err != nil {
		t.Fatal(err)
	}
	auditSource, err := storage.NewConfigSource("nha029:activation")
	if err != nil {
		t.Fatal(err)
	}
	first, err := prepareWorkspaceActivation(ctx, store, startup, old, RetainDefinitions(), author, auditSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Both candidates capture the same old active pair before either commits.
	second, err := prepareWorkspaceActivation(ctx, store, startup, old, RetainDefinitions(), author, auditSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	firstInstance, err := integrations.NewSourceKey("nha029-first-device", "switch")
	if err != nil {
		t.Fatal(err)
	}
	secondInstance, err := integrations.NewSourceKey("nha029-second-device", "occupancy")
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha029-test-pilot")
	if err != nil {
		t.Fatal(err)
	}
	gate, err := repo.BeginActivation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	target, outcome, err := writePreparedActivation(ctx, store, replay, gate, first, func(writer *registry.ConfigWriter) error {
		_, err := writer.ConfigureBinding(instance, firstInstance, "switch.arc_soundbar_crossfade")
		return err
	})
	if outcome != storage.TransactionCommitted || err != nil || !target.Valid() || target.Number() <= old.Revision().Number() {
		t.Fatalf("first coherent commit: target=%v outcome=%v err=%v", target, outcome, err)
	}
	committed, present, err := store.ReadActiveConfig(ctx)
	if err != nil || !present || committed.Revision() != target {
		t.Fatalf("committed active missing before publication: present=%t err=%v", present, err)
	}
	if err := gate.PublishCommitted(ctx, target, committed.ManifestSHA256()); err != nil {
		t.Fatal(err)
	}
	if err := gate.Release(); err != nil {
		t.Fatal(err)
	}
	active, present, err := store.ReadActiveConfig(ctx)
	if err != nil || !present || active.Revision() != target || active.ManifestSHA256() == old.ManifestSHA256() {
		t.Fatalf("new SQL authority missing: present=%t revision=%v err=%v", present, active.Revision(), err)
	}
	if _, found, err := store.ReadActivationAttempt(ctx, first.attempt); err != nil || !found {
		t.Fatalf("committed attempt not durable: found=%t err=%v", found, err)
	}
	if _, err := repo.Resolve(ctx, instance, []integrations.SourceKey{firstInstance}); err != nil {
		t.Fatalf("committed registry binding missing: %v", err)
	}
	gate, err = repo.BeginActivation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, outcome, err = writePreparedActivation(ctx, store, replay, gate, second, func(writer *registry.ConfigWriter) error {
		_, err := writer.ConfigureBinding(instance, secondInstance, "binary_sensor.bathroom_motion_occupancy")
		return err
	})
	if outcome != storage.TransactionNotCommitted || err == nil {
		t.Fatalf("stale revision+manifest CAS accepted: outcome=%v err=%v", outcome, err)
	}
	if err := gate.ResolveNotCommitted(outcome); err != nil {
		t.Fatal(err)
	}
	if err := gate.Release(); err != nil {
		t.Fatal(err)
	}
	active, present, err = store.ReadActiveConfig(ctx)
	if err != nil || !present || active.Revision() != target {
		t.Fatalf("conflicting candidate changed active: present=%t revision=%v err=%v", present, active.Revision(), err)
	}
	if _, found, err := store.ReadActivationAttempt(ctx, second.attempt); err != nil || found {
		t.Fatalf("conflicting attempt persisted: found=%t err=%v", found, err)
	}
	if _, err := repo.Resolve(ctx, instance, []integrations.SourceKey{secondInstance}); !errors.Is(err, registry.ErrUnconfigured) {
		t.Fatalf("conflicting registry write escaped rollback: %v", err)
	}
}
