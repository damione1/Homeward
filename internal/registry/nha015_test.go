package registry_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

func nha015Manifest(t *testing.T) registry.Manifest {
	t.Helper()
	data, err := os.ReadFile("../../scope/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := registry.ParseManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func nha015Store(t *testing.T) (*storage.Store, context.Context, storage.Settings) {
	t.Helper()
	cluster := testpg.Start(t)
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if got, err := store.Migrate(ctx); err != nil || got != storage.CurrentVersion() {
		t.Fatalf("migrate %v: %v", got, err)
	}
	return store, ctx, settings
}

func nha015Instance(t *testing.T, value string) domain.IntegrationInstanceID {
	t.Helper()
	id, err := domain.ParseIntegrationInstanceID(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func nha015Source(t *testing.T, device, entity string) integrations.SourceKey {
	t.Helper()
	key, err := integrations.NewSourceKey(device, entity)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func nha015Repository(t *testing.T, store *storage.Store) *registry.Repository {
	t.Helper()
	repo, err := registry.NewRepository(store, nha015Manifest(t))
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestNHA015ManifestAndExplicitAdmission(t *testing.T) {
	if _, err := (registry.HAAlias{}).Value(); !errors.Is(err, registry.ErrInvalidHAAlias) {
		t.Fatalf("zero HA alias: %v", err)
	}
	for _, bad := range []string{"", "light", "Light.Kitchen", "light.kitchen name", "light.kitchen\x00"} {
		if _, err := registry.ParseHAAlias(bad); !errors.Is(err, registry.ErrInvalidHAAlias) {
			t.Fatalf("invalid HA alias %q: %v", bad, err)
		}
	}
	data, err := os.ReadFile("../../scope/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ParseManifest([]byte(`{}`)); !errors.Is(err, registry.ErrInvalidManifest) {
		t.Fatal(err)
	}
	mutated := append([]byte(nil), data...)
	for i := range mutated {
		if mutated[i] == '8' {
			mutated[i] = '7'
			break
		}
	}
	if _, err := registry.ParseManifest(mutated); !errors.Is(err, registry.ErrInvalidManifest) {
		t.Fatal("tampered manifest admitted")
	}
	store, ctx, settings := nha015Store(t)
	repo := nha015Repository(t, store)
	instance := nha015Instance(t, "mqtt-main")
	key := nha015Source(t, "ieee:00124b0000000001", "water_leak")
	if _, err := repo.Resolve(ctx, instance, []integrations.SourceKey{key}); !errors.Is(err, registry.ErrUnconfigured) {
		t.Fatalf("unconfigured discovery: %v", err)
	}
	if _, err := repo.ConfigureBinding(ctx, instance, key, "switch.outside_frozen_manifest"); !errors.Is(err, registry.ErrOutsideManifest) {
		t.Fatalf("outside manifest: %v", err)
	}
	original := "binary_sensor.basement_leak_sensor_water_leak"
	first, err := repo.ConfigureBinding(ctx, instance, key, original)
	if err != nil {
		t.Fatal(err)
	}
	deviceText, _ := first.Device.Value()
	entityText, _ := first.Entity.Value()
	if deviceText == original || entityText == original || deviceText == entityText || len(deviceText) < 20 || len(entityText) < 20 {
		t.Fatalf("IDs are not opaque and distinct: %q %q", deviceText, entityText)
	}
	second, err := repo.ConfigureBinding(ctx, instance, key, original)
	if err != nil || second != first {
		t.Fatalf("idempotent mapping changed: %v %v", second, err)
	}
	if got, err := repo.Resolve(ctx, instance, []integrations.SourceKey{key}); err != nil || len(got) != 1 || got[0] != first {
		t.Fatalf("source identity changed: %v %v", got, err)
	}
	// A second stable endpoint on the same provider device shares DeviceID.
	otherKey := nha015Source(t, "ieee:00124b0000000001", "occupancy")
	other, err := repo.ConfigureBinding(ctx, instance, otherKey, "binary_sensor.bathroom_motion_occupancy")
	if err != nil || other.Device != first.Device || other.Entity == first.Entity {
		t.Fatalf("device relationship: %v %v", other, err)
	}
	if _, err := repo.Resolve(ctx, instance, []integrations.SourceKey{key, nha015Source(t, "unknown", "endpoint")}); !errors.Is(err, registry.ErrUnconfigured) {
		t.Fatalf("partial batch admitted: %v", err)
	}
	// The same readable name or source component in another provider is separate.
	hilo := nha015Instance(t, "hilo-main")
	neviweb := nha015Instance(t, "neviweb-main")
	homonym := nha015Source(t, "Chambre Eugenie", "climate")
	a, err := repo.ConfigureBinding(ctx, hilo, homonym, "climate.chambre_eugenie")
	if err != nil {
		t.Fatal(err)
	}
	b, err := repo.ConfigureBinding(ctx, neviweb, homonym, "climate.neviweb130_climate_chambre_eugenie")
	if err != nil {
		t.Fatal(err)
	}
	if a.Device == b.Device || a.Entity == b.Entity {
		t.Fatal("Hilo/Neviweb homonyms merged")
	}
	if _, err := repo.ConfigureBinding(ctx, neviweb, nha015Source(t, "other", "climate"), "climate.chambre_eugenie"); !errors.Is(err, registry.ErrBindingConflict) {
		t.Fatalf("manifest alias remapped: %v", err)
	}
	// A second Store proves IDs and relationships survive reopening the pool.
	// The existing Store stays open because it owns only a pool, not the cluster.
	reopened, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	repo2 := nha015Repository(t, reopened)
	if got, err := repo2.Resolve(ctx, instance, []integrations.SourceKey{key, otherKey}); err != nil || len(got) != 2 || got[0] != first || got[1] != other {
		t.Fatalf("restart changed identities: %v %v", got, err)
	}
	if err := store.InTransaction(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE public.registry_entities SET manifest_alias='switch.outside_frozen_manifest' WHERE id=$1`, entityText)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo2.Resolve(ctx, instance, []integrations.SourceKey{key}); !errors.Is(err, registry.ErrOutsideManifest) {
		t.Fatalf("SQL row bypassed manifest admission: %v", err)
	}
}

func TestNHA015RenameTombstoneCollisionAndRollback(t *testing.T) {
	store, ctx, settings := nha015Store(t)
	repo := nha015Repository(t, store)
	instance := nha015Instance(t, "mqtt-main")
	key := nha015Source(t, "ieee:00124b0000000002", "water_leak")
	old := "binary_sensor.basement_leak_sensor_water_leak"
	id, err := repo.ConfigureBinding(ctx, instance, key, old)
	if err != nil {
		t.Fatal(err)
	}
	newAlias := "binary_sensor.basement_leak_renamed"
	if err := repo.Rename(ctx, id.Entity, newAlias); err != nil {
		t.Fatal(err)
	}
	// Every original v1 alias is reserved, including identities not configured yet.
	reserved := "binary_sensor.bathroom_motion_occupancy"
	if err := repo.Rename(ctx, id.Entity, reserved); !errors.Is(err, registry.ErrAliasConflict) {
		t.Fatalf("unconfigured manifest alias stolen: %v", err)
	}
	for label, current := range map[string]bool{old: false, newAlias: true} {
		alias, err := repo.LookupAlias(ctx, label)
		if err != nil || alias.Entity != id.Entity || alias.Current != current || alias.Retired {
			t.Fatalf("alias %s: %v %v", label, alias, err)
		}
	}
	// A network address is not part of the source identity. Reconfiguration of
	// transport or display name leaves the stable key and Host ID intact.
	if got, err := repo.Resolve(ctx, instance, []integrations.SourceKey{key}); err != nil || got[0] != id {
		t.Fatalf("IP/name change affected identity: %v %v", got, err)
	}
	other, err := repo.ConfigureBinding(ctx, instance, nha015Source(t, "ieee:00124b0000000003", "water_leak"), reserved)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Rename(ctx, other.Entity, old); !errors.Is(err, registry.ErrAliasConflict) {
		t.Fatalf("historical collision: %v", err)
	}
	if err := repo.Rename(ctx, id.Entity, old); err != nil {
		t.Fatal(err)
	}
	if err := repo.Tombstone(ctx, id.Entity); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Resolve(ctx, instance, []integrations.SourceKey{key}); !errors.Is(err, registry.ErrTombstoned) {
		t.Fatalf("rediscovery revived tombstone: %v", err)
	}
	if _, err := repo.ConfigureBinding(ctx, instance, key, old); !errors.Is(err, registry.ErrTombstoned) {
		t.Fatalf("reconfigure revived tombstone: %v", err)
	}
	if alias, err := repo.LookupAlias(ctx, newAlias); err != nil || !alias.Retired || alias.Entity != id.Entity {
		t.Fatalf("retired historical alias lost: %v %v", alias, err)
	}
	reopened, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := nha015Repository(t, reopened)
	if alias, err := restarted.LookupAlias(ctx, newAlias); err != nil || !alias.Retired || alias.Entity != id.Entity {
		t.Fatalf("historical alias lost on restart: %v %v", alias, err)
	}
	if _, err := restarted.Resolve(ctx, instance, []integrations.SourceKey{key}); !errors.Is(err, registry.ErrTombstoned) {
		t.Fatalf("tombstone lost on restart: %v", err)
	}
	// A SQL failure during ConfigureBinding rolls back device and source rows.
	if err := store.InTransaction(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `ALTER TABLE public.registry_aliases ADD CONSTRAINT nha015_reject CHECK (alias <> 'binary_sensor.hydroqc_maison_critical')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	badDevice := "ieee:00124b00000000ff"
	if _, err := repo.ConfigureBinding(ctx, instance, nha015Source(t, badDevice, "critical"), "binary_sensor.hydroqc_maison_critical"); !errors.Is(err, registry.ErrStorage) {
		t.Fatalf("SQL failure not classified: %v", err)
	} else {
		var storageErr *storage.Error
		if !errors.As(err, &storageErr) || err.Error() != registry.ErrStorage.Error() {
			t.Fatalf("storage cause identity/redaction: %v", err)
		}
	}
	var count int
	if err := store.InTransaction(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM public.registry_device_sources WHERE source_device=$1`, badDevice).Scan(&count)
	}); err != nil || count != 0 {
		t.Fatalf("failed transaction persisted source: %d %v", count, err)
	}
}

func TestNHA015ConcurrentDiscoveryAndHostAdmission(t *testing.T) {
	store, ctx, settings := nha015Store(t)
	repo := nha015Repository(t, store)
	otherStore, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer otherStore.Close()
	otherRepo := nha015Repository(t, otherStore)
	instance := nha015Instance(t, "mqtt-main")
	key := nha015Source(t, "ieee:00124b0000000010", "water_leak")
	original := "binary_sensor.basement_leak_sensor_water_leak"
	if _, err := repo.ConfigureBinding(ctx, instance, key, original); err != nil {
		t.Fatal(err)
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	_, sink, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(repo, sink, catalog)
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := integrations.NewCandidate(key, []*wireregistry.CapabilityDescription{{SchemaId: contracts.BinarySchema, Major: 1, DisplayName: "Water leak", Endpoint: "water_leak", Constraints: &wireregistry.CapabilityDescription_Binary{Binary: &wireregistry.BinaryConstraints{}}}})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := session.Discover(ctx, []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		t.Fatalf("Host did not consume SQL adapter: %v %v", bindings, err)
	}
	if _, _, err := bindings[0].IDs(); err != nil {
		t.Fatal(err)
	}
	boundEntity := bindingsEntity(t, bindings[0])
	unconfigured := nha015Source(t, "ieee:unconfigured", "water_leak")
	unconfiguredCandidate, err := integrations.NewCandidate(unconfigured, []*wireregistry.CapabilityDescription{{SchemaId: contracts.BinarySchema, Major: 1, DisplayName: "Other", Endpoint: "water_leak", Constraints: &wireregistry.CapabilityDescription_Binary{Binary: &wireregistry.BinaryConstraints{}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Discover(ctx, []integrations.Candidate{unconfiguredCandidate}); !errors.Is(err, registry.ErrUnconfigured) {
		t.Fatalf("Host activated unconfigured source: %v", err)
	}
	// Registry operations serialize in PostgreSQL across repository instances.
	var wg sync.WaitGroup
	errCh := make(chan error, 64)
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := session.Discover(ctx, []integrations.Candidate{candidate})
			errCh <- err
		}()
		go func(i int) {
			defer wg.Done()
			errCh <- otherRepo.Rename(ctx, boundEntity, "binary_sensor.leak_revision_"+string(rune('a'+i)))
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent rename/discovery: %v", err)
		}
	}
	if _, err := repo.Resolve(ctx, instance, []integrations.SourceKey{key}); err != nil {
		t.Fatal(err)
	}
	if err := host.Revoke(session); err != nil {
		t.Fatal(err)
	}
	reconnected, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	afterReconnect, err := reconnected.Discover(ctx, []integrations.Candidate{candidate})
	if err != nil || len(afterReconnect) != 1 {
		t.Fatalf("rediscovery after rename/reconnect: %v %v", afterReconnect, err)
	}
	if again := bindingsEntity(t, afterReconnect[0]); again != boundEntity {
		t.Fatal("reconnect created a new Host entity ID")
	}
	if err := repo.Tombstone(ctx, boundEntity); err != nil {
		t.Fatal(err)
	}
	if _, err := reconnected.Discover(ctx, []integrations.Candidate{candidate}); !errors.Is(err, registry.ErrTombstoned) {
		t.Fatalf("Host rediscovery revived tombstone: %v", err)
	}
}

func bindingsEntity(t *testing.T, b integrations.Binding) domain.EntityID {
	t.Helper()
	_, id, err := b.IDs()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
