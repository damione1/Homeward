package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestNHA069UpgradeFromV5PreservesRegistryConfigSecretsAndReopens(t *testing.T) {
	port := testCluster(t)
	settings := testSettings(t, port)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	version, err := store.migrate(ctx, migrations[:5])
	if err != nil || version.Number() != 5 {
		t.Fatalf("v5 setup: %v %v", version, err)
	}
	for _, statement := range []string{
		`INSERT INTO public.registry_devices(id) VALUES ('device-v5')`,
		`INSERT INTO public.registry_device_sources(instance_id,source_device,device_id) VALUES ('instance-v5','wire-device-v5','device-v5')`,
		`INSERT INTO public.registry_entities(id,device_id,manifest_alias) VALUES ('entity-v5','device-v5','sensor.washing_machine_power')`,
		`INSERT INTO public.registry_source_bindings(instance_id,source_device,source_entity,entity_id) VALUES ('instance-v5','wire-device-v5','wire-power-v5','entity-v5')`,
		`INSERT INTO public.registry_aliases(alias,entity_id,current) VALUES ('sensor.washing_machine_power','entity-v5',true)`,
		`INSERT INTO public.config_documents(id,logical_path,kind) VALUES ('config-v5','configuration.yaml','yaml_source')`,
	} {
		if _, err := store.pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	dataDir := filepath.Join(t.TempDir(), "data")
	secrets, err := store.BootstrapSecrets(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	handle := nha051Handle(t, "instance-v5", "mqtt-password")
	if err := secrets.RotateCredential(ctx, handle, nha051Value(t, "isolated-v5-canary")); err != nil {
		t.Fatal(err)
	}
	latest := migrations[len(migrations)-1].version.Number()
	if latest < 7 {
		t.Fatal("current migration set omits the cloud quota table")
	}
	if got, err := store.Migrate(ctx); err != nil || got.Number() != latest {
		t.Fatalf("upgrade: %v %v", got, err)
	}
	if got, err := store.Migrate(ctx); err != nil || got.Number() != latest {
		t.Fatalf("idempotent migrate: %v %v", got, err)
	}
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.registry_source_bindings WHERE entity_id='entity-v5'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("registry witness: %d %v", count, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.config_documents WHERE id='config-v5'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("config witness: %d %v", count, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.registry_mqtt_discovery_topics`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("new table: %d %v", count, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.cloud_request_reservations`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("quota table after upgrade: %d %v", count, err)
	}
	secret, err := secrets.Read(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := secret.Bytes()
	if err != nil || string(bytes) != "isolated-v5-canary" {
		t.Fatalf("secret witness: %v", err)
	}
	store.Close()
	reopened, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, err := reopened.Migrate(ctx); err != nil || got.Number() != latest {
		t.Fatalf("reopen: %v %v", got, err)
	}
	if err := reopened.pool.QueryRow(ctx, `SELECT count(*) FROM public.cloud_request_reservations`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("quota table after reopen: %d %v", count, err)
	}
	if err := reopened.pool.QueryRow(ctx, `SELECT count(*) FROM public.registry_source_bindings WHERE entity_id='entity-v5'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("reopened registry: %d %v", count, err)
	}
	if err := reopened.pool.QueryRow(ctx, `SELECT count(*) FROM public.config_documents WHERE id='config-v5'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("reopened config: %d %v", count, err)
	}
	secretRepo, err := reopened.BootstrapSecrets(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	secret, err = secretRepo.Read(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	bytes, err = secret.Bytes()
	if err != nil || string(bytes) != "isolated-v5-canary" {
		t.Fatalf("reopened secret: %v", err)
	}
}
