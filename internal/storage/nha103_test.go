package storage

import (
	"context"
	"testing"
	"time"
)

func TestNHA103UpgradeFromHistorical18AndReopen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	port := testCluster(t)
	store, err := Open(ctx, testSettings(t, port))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if len(migrations) != 19 || migrations[18].version.Number() != 19 {
		t.Fatal("migration 19 is not append-only")
	}
	if version, err := store.migrate(ctx, migrations[:18]); err != nil || version.Number() != 18 {
		t.Fatalf("historical migration chain: %v %v", version, err)
	}
	var before *string
	if err := store.pool.QueryRow(ctx, `SELECT to_regclass('public.homeward_lists')::text`).Scan(&before); err != nil || before != nil {
		t.Fatalf("lists existed before migration 19: %v", err)
	}
	if version, err := store.Migrate(ctx); err != nil || version.Number() != 19 {
		t.Fatalf("upgrade to migration 19: %v %v", version, err)
	}
	var listTable, itemTable, ledgerTable *string
	if err := store.pool.QueryRow(ctx, `SELECT to_regclass('public.homeward_lists')::text,
		to_regclass('public.homeward_list_items')::text,
		to_regclass('public.homeward_legacy_import_ledger')::text`).Scan(&listTable, &itemTable, &ledgerTable); err != nil || listTable == nil || itemTable == nil || ledgerTable == nil {
		t.Fatalf("migration 19 tables missing: %v", err)
	}
	if version, err := store.Migrate(ctx); err != nil || version.Number() != 19 {
		t.Fatalf("migration 19 reopen: %v %v", version, err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO public.homeward_lists(list_id,origin,name,lifecycle,revision) VALUES('invalid-lifecycle','native','synthetic',3,1)`); err == nil {
		t.Fatal("SQL admitted unknown lifecycle")
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO public.homeward_lists(list_id,origin,name,lifecycle,revision) VALUES('invalid-origin','future','synthetic',1,1)`); err == nil {
		t.Fatal("SQL admitted unknown origin")
	}
}
