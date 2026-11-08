package storage

import (
	"context"
	"testing"
)

// These helpers also support the local cross-binary SQL31→32 qualification.
// The predecessor binary builds its own schema using its unchanged migrator.
func nha213SeedLedger(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	for _, query := range []string{
		`INSERT INTO public.config_generations(entry_path,candidate_hash,manifest_hash,entry_count) VALUES('synthetic.yaml',decode(repeat('11',32),'hex'),decode(repeat('22',32),'hex'),1)`,
		`INSERT INTO public.config_activation_attempts(attempt_id,target_generation,target_manifest_hash,author,source,api_request_id,api_request_method,api_request_create_time) VALUES(decode(repeat('11',16),'hex'),1,decode(repeat('22',32),'hex'),'synthetic-author','synthetic-source','018e363a-8e16-44a4-9aa6-f623ecb0b504',1,clock_timestamp())`,
		`INSERT INTO public.config_generations(entry_path,candidate_hash,manifest_hash,entry_count) VALUES('native.yaml',decode(repeat('11',32),'hex'),decode(repeat('22',32),'hex'),1)`,
		`INSERT INTO public.config_activation_attempts(attempt_id,target_generation,target_manifest_hash,author,source) VALUES(decode(repeat('77',16),'hex'),2,decode(repeat('22',32),'hex'),'synthetic-author','synthetic-source')`,
		`INSERT INTO public.config_api_requests(account_id,method,request_id,fingerprint,attempt_id,execution_digest) VALUES('synthetic-author',1,'018e363a-8e16-44a4-9aa6-f623ecb0b505',decode(repeat('33',32),'hex'),decode(repeat('44',16),'hex'),decode(repeat('55',32),'hex'))`,
	} {
		if _, err := store.pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
}

func nha213LedgerSnapshot(t *testing.T, ctx context.Context, store *Store) string {
	t.Helper()
	var data string
	if err := store.pool.QueryRow(ctx, `SELECT jsonb_build_object('generations',(SELECT jsonb_agg(to_jsonb(g) ORDER BY revision) FROM public.config_generations g),'attempts',(SELECT jsonb_agg(to_jsonb(a) ORDER BY attempt_id) FROM public.config_activation_attempts a),'requests',(SELECT jsonb_agg(to_jsonb(r) ORDER BY attempt_id) FROM public.config_api_requests r))::text`).Scan(&data); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestNHA213Migration32PreservesLedgerAndStrictMethodShape(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testSettings(t, testCluster(t)))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.migrate(ctx, migrations[:25]); err != nil {
		t.Fatal(err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	for _, m := range migrations[25:31] {
		if _, err = tx.Exec(ctx, m.sql); err != nil {
			t.Fatal(err)
		}
		if m.version.number == 31 {
			if err = migrateFanObservations31(ctx, tx); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = tx.Exec(ctx, `INSERT INTO public.homeward_schema_migrations(version) VALUES($1)`, m.version.number); err != nil {
			t.Fatal(err)
		}
	}
	base, plan := schemaPlanSHA256(migrations[:30]), schemaPlanSHA256(migrations[:31])
	const window = `INSERT INTO public.homeward_schema_window(singleton,format,schema_version,release_id,minimum_reader,minimum_writer,maximum_release,baseline_schema,baseline_plan,migration_plan) VALUES(true,1,31,$1,$1,$1,$1,30,$2,$3) ON CONFLICT(singleton) DO UPDATE SET format=1,schema_version=31,release_id=EXCLUDED.release_id,minimum_reader=EXCLUDED.minimum_reader,minimum_writer=EXCLUDED.minimum_writer,maximum_release=EXCLUDED.maximum_release,baseline_schema=30,baseline_plan=EXCLUDED.baseline_plan,migration_plan=EXCLUDED.migration_plan`
	if _, err = tx.Exec(ctx, window, "homeward-window-30-31-successor", base[:], plan[:]); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	nha213SeedLedger(t, ctx, store)
	before := nha213LedgerSnapshot(t, ctx, store)
	// A valid SQL ordinal is insufficient: a forged predecessor digest is refused.
	if _, err = store.pool.Exec(ctx, `UPDATE public.homeward_schema_window SET migration_plan=decode(repeat('00',32),'hex')`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Migrate(ctx); err == nil {
		t.Fatal("forged predecessor admitted")
	}
	if _, err = store.pool.Exec(ctx, `UPDATE public.homeward_schema_window SET migration_plan=$1`, plan[:]); err != nil {
		t.Fatal(err)
	}
	if v, err := store.Migrate(ctx); err != nil || v.Number() != 32 {
		t.Fatal("migration32", v, err)
	}
	if after := nha213LedgerSnapshot(t, ctx, store); before != after {
		t.Fatal("migration changed existing ledger bytes")
	}
	for _, c := range []struct {
		name, fields string
		valid        bool
	}{
		{"native", "NULL,NULL,NULL", true},
		{"publish", "NULL,1,NULL", true},
		{"rollback-document", "NULL,2,NULL", true},
		{"apply-plan", "NULL,3,NULL", true},
		{"rollback", "NULL,4,NULL", true},
		{"missing-method", "'018e363a-8e16-44a4-9aa6-f623ecb0b506',NULL,NULL", false},
		{"unknown", "NULL,8,NULL", false},
		{"missing-reservation-time", "'018e363a-8e16-44a4-9aa6-f623ecb0b506',1,NULL", false},
		{"integration-without-time", "NULL,5,NULL", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			tx, err := store.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollback(tx)
			var generation int64
			if err = tx.QueryRow(ctx, `INSERT INTO public.config_generations(entry_path,candidate_hash,manifest_hash,entry_count) VALUES('case.yaml',decode(repeat('11',32),'hex'),decode(repeat('22',32),'hex'),1) RETURNING revision`).Scan(&generation); err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(ctx, `INSERT INTO public.config_activation_attempts(attempt_id,target_generation,target_manifest_hash,author,source,api_request_id,api_request_method,api_request_create_time) VALUES(decode(repeat('66',16),'hex'),$1,decode(repeat('22',32),'hex'),'synthetic-author','synthetic-source',`+c.fields+`)`, generation)
			if (err == nil) != c.valid {
				t.Fatalf("strict method shape valid=%v error=%v", c.valid, err)
			}
		})
	}
}
