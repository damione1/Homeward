package storage

import (
	"context"
	"testing"
	"time"

	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/domain"
)

func TestNHA201RegistryMigrationPreservesIdentityAndRights(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, err := Open(ctx, testSettings(t, testCluster(t)))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.migrate(ctx, migrations[:28]); err != nil {
		t.Fatal(err)
	}
	baseline := schemaPlanSHA256(migrations[:28])
	if _, err := store.pool.Exec(ctx, `INSERT INTO public.homeward_schema_window(singleton,format,schema_version,release_id,minimum_reader,minimum_writer,maximum_release,baseline_schema,baseline_plan,migration_plan) VALUES(true,1,28,$1,$1,$1,$1,28,$2,$2)`, prior28SchemaRelease, baseline[:]); err != nil {
		t.Fatal(err)
	}
	admin := nha049AccountInput(t, "Synthetic Registry Administrator", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	nha204SeedPrior28Account(t, ctx, store, admin)
	snapshot, err := store.AuthorizationPolicy(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := snapshot.State()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO public.registry_devices(id) VALUES('dev_synthetic');
INSERT INTO public.registry_entities(id,device_id,manifest_alias) VALUES('ent_synthetic','dev_synthetic','light.synthetic');
INSERT INTO public.registry_aliases(alias,entity_id,current) VALUES('light.synthetic','ent_synthetic',true)`); err != nil {
		t.Fatal(err)
	}
	var historical []byte
	if err := store.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(t) ORDER BY version)::text FROM public.homeward_schema_migrations t`).Scan(&historical); err != nil {
		t.Fatal(err)
	}
	if version, err := store.Migrate(ctx); err != nil || version != CurrentVersion() {
		t.Fatalf("28 -> 29: %v %v", version, err)
	}
	var home, id, label, alias, native string
	var revision int64
	var created, updated *time.Time
	if err := store.pool.QueryRow(ctx, `SELECT resource_id FROM public.homeward_registry_home`).Scan(&home); err != nil {
		t.Fatal(err)
	}
	homeName, err := apitypes.ParseHomeName("homes/" + home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT id,api_resource_id,api_display_name,api_metadata_revision,api_create_time,api_update_time,manifest_alias FROM public.registry_entities`).Scan(&native, &id, &label, &revision, &created, &updated, &alias); err != nil {
		t.Fatal(err)
	}
	nativeID, _ := domain.ParseEntityID(native)
	expected, _ := apitypes.EntityNameForIdentity(homeName, nativeID)
	if expected.String() != homeName.String()+"/entities/"+id || native != "ent_synthetic" || alias != "light.synthetic" || label != "" || revision != 1 || created != nil || updated != nil {
		t.Fatal("migration changed identity or invented metadata")
	}
	current, err := store.AuthorizationPolicy(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	after, err := current.State()
	if err != nil {
		t.Fatal(err)
	}
	// AccountState contains private admitted values; its stable rights evidence
	// is compared through the public accessors, independent of API DTOs.
	bv, _ := before.RightsRevision()
	av, _ := after.RightsRevision()
	if bv != av {
		t.Fatal("migration changed rights revision")
	}
	var preserved []byte
	if err := store.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(t) ORDER BY version)::text FROM public.homeward_schema_migrations t WHERE version<=28`).Scan(&preserved); err != nil {
		t.Fatal(err)
	}
	if string(historical) != string(preserved) {
		t.Fatal("historical migration records changed")
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO public.registry_entities(id,device_id,manifest_alias) VALUES('ent_synthetic_new','dev_synthetic','light.synthetic_new')`); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT api_create_time FROM public.registry_entities WHERE id='ent_synthetic_new'`).Scan(&created); err != nil || created == nil {
		t.Fatal("new identities have no creation time", err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal("migration reopen", err)
	}
	var again string
	if err := store.pool.QueryRow(ctx, `SELECT resource_id FROM public.homeward_registry_home`).Scan(&again); err != nil || again != home {
		t.Fatal("home identity changed on reopen", err)
	}
}
