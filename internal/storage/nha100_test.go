package storage

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Limit the plan at its historical boundary even after future migrations are
// appended. Calendar integration fixtures separately migrate CurrentVersion.
func TestNHA100OfficialMigration14To15PreservesPriorLedger(t *testing.T) {
	port := testCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	settings := testSettings(t, port)
	store, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	index := -1
	for i, migration := range migrations {
		if migration.version.Number() == 15 {
			index = i
			break
		}
	}
	if index <= 0 || migrations[index-1].version.Number() != 14 || migrations[index].sql != calendarSchemaSQL {
		t.Fatal("official append-only calendar migration15 missing after14")
	}
	if version, err := store.migrate(ctx, migrations[:index]); err != nil || version.Number() != 14 {
		t.Fatalf("historical14: version=%d err=%v", version.Number(), err)
	}
	var priorApplied time.Time
	var calendarAbsent bool
	if err := store.InTransaction(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT applied_at,to_regclass('public.calendar_events') IS NULL FROM public.homeward_schema_migrations WHERE version=14`).Scan(&priorApplied, &calendarAbsent)
	}); err != nil || !calendarAbsent {
		t.Fatal("historical boundary was not14", err)
	}
	id, _ := domain.ParseCommandID("nha100-existing-prior-intent")
	actor, _ := domain.ParseActorID("nha100-migration-actor")
	fingerprint := []byte("nha100-synthetic-prior-fingerprint")
	if _, created, outcome, err := store.AcceptCommandIntent(ctx, id, actor, "nha100-migration-epoch", fingerprint); err != nil || !created || outcome != TransactionCommitted {
		t.Fatalf("prior ledger created=%t outcome=%d err=%v", created, outcome, err)
	}
	if version, err := store.migrate(ctx, migrations[:index+1]); err != nil || version.Number() != 15 {
		t.Fatalf("official14->15 version=%d err=%v", version.Number(), err)
	}
	store.Close()
	reopened, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if version, err := reopened.migrate(ctx, migrations[:index+1]); err != nil || version.Number() != 15 {
		t.Fatalf("historical15 reopen/idempotence version=%d err=%v", version.Number(), err)
	}
	intent, found, err := reopened.CommandIntent(ctx, id)
	if err != nil || !found || intent.State() != IntentAccepted || !bytes.Equal(intent.Fingerprint(), fingerprint) {
		t.Fatalf("15 changed prior ledger found=%t state=%d err=%v", found, intent.State(), err)
	}
	var applied time.Time
	var history, tables int
	if err := reopened.InTransaction(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT applied_at FROM public.homeward_schema_migrations WHERE version=14`).Scan(&applied); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM public.homeward_schema_migrations`).Scan(&history); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('scheduled_jobs','scheduled_job_definitions','helper_values','state_history','local_calendars','calendar_events','calendar_dates','calendar_exceptions')`).Scan(&tables)
	}); err != nil || !applied.Equal(priorApplied) || history != 15 || tables != 8 {
		t.Fatalf("historical15 integrity history=%d tables=%d priorTimestampPreserved=%t err=%v", history, tables, applied.Equal(priorApplied), err)
	}
	t.Log("real upgrade14->15; prior command ledger and migration14 timestamp preserved; reopened15 idempotent;4 prior+4 calendar tables")
}
