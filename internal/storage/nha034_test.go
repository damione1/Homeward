package storage

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/jackc/pgx/v5"
)

func TestNHA034OfficialMigrationPreservesPriorCommandLedger(t *testing.T) {
	port := testCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, testSettings(t, port))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	jobMigration := -1
	for index, migration := range migrations {
		if migration.version.Number() == 14 {
			jobMigration = index
			break
		}
	}
	if jobMigration <= 0 || migrations[jobMigration-1].version.Number() != 13 || migrations[jobMigration].sql != scheduledJobsSchema {
		t.Fatal("official append-only v14 job migration missing")
	}
	if version, err := store.migrate(ctx, migrations[:jobMigration]); err != nil || version.Number() != 13 {
		t.Fatalf("real historical v13: version=%d err=%v", version.Number(), err)
	}
	id, _ := domain.ParseCommandID("nha034-existing-intent")
	actor, _ := domain.ParseActorID("nha034-migration-actor")
	fingerprint := []byte("nha034-existing-immutable-fingerprint")
	if _, created, outcome, err := store.AcceptCommandIntent(ctx, id, actor, "nha034-migration-epoch", fingerprint); err != nil || !created || outcome != TransactionCommitted {
		t.Fatalf("prior intent: created=%v outcome=%d err=%v", created, outcome, err)
	}
	for range 2 {
		if version, err := store.migrate(ctx, migrations[:jobMigration+1]); err != nil || version.Number() != 14 {
			t.Fatalf("official migration/reopen: version=%d err=%v", version.Number(), err)
		}
	}
	intent, found, err := store.CommandIntent(ctx, id)
	if err != nil || !found || intent.State() != IntentAccepted || !bytes.Equal(intent.Fingerprint(), fingerprint) {
		t.Fatalf("v14 changed existing ledger: found=%v state=%d err=%v", found, intent.State(), err)
	}
	var tables int
	if err := store.InTransaction(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('scheduled_jobs','scheduled_job_definitions','helper_values','state_history')`).Scan(&tables)
	}); err != nil || tables != 4 {
		t.Fatalf("existing/new tables: count=%d err=%v", tables, err)
	}
}
