package storage

import (
	"context"
	"crypto/sha256"
	"reflect"
	"strings"
	"testing"
	"time"

	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Historical setup is deliberately test-local: current creation owners also
// allocate SQL29 API names. Values still pass the native account/policy admission.
func nha204SeedPrior28Account(t *testing.T, ctx context.Context, s *Store, input NewAccount) {
	t.Helper()
	id, login, key, display, role, profile, salt, digest, err := accountInput(input)
	if err != nil {
		t.Fatal(err)
	}
	source, err := authorization.EmptySource(input.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := source.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := source.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	semantic, err := source.SemanticSHA256()
	if err != nil {
		t.Fatal(err)
	}
	binding, err := policyBinding(input.ID)
	if err != nil {
		t.Fatal(err)
	}
	kind, schema, syntax, err := configSourceAuthorization.values()
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO public.auth_login_claims(login_key,owner_kind,owner_id) VALUES($1,1,$2)`, key, id)
	exec(`INSERT INTO public.auth_accounts(id,login_name,login_key,display_name,role,status,password_profile,password_salt,password_digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, login, key, display, role, sqlStatusActive, profile, salt[:], digest[:])
	exec(`INSERT INTO public.config_documents(id,logical_path,kind) VALUES($1,$2,$3::public.config_document_kind)`, binding.ID().String(), binding.Path(), kind.String())
	exec(`UPDATE public.config_documents SET last_revision=1 WHERE id=$1`, binding.ID().String())
	exec(`INSERT INTO public.config_versions(document_id,revision,source_hash,author,source,schema_version,syntax_profile,yaml_source) VALUES($1,1,$2,$3,'synthetic migration fixture',$4::public.config_schema_version,$5::public.config_syntax_profile,$6)`, binding.ID().String(), hash[:], id, schema, syntax, data)
	exec(`INSERT INTO public.authorization_policy_active(account_id,document_id,version_revision,rights_revision,semantic_sha256) VALUES($1,$2,1,1,$3)`, id, binding.ID().String(), semantic[:])
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// Capture the exact columns that existed before migration, including bytea,
// nullable fields and timestamps. Later added columns cannot hide native drift.
func nha204Columns(t *testing.T, ctx context.Context, s *Store) map[string][]string {
	t.Helper()
	rows, err := s.pool.Query(ctx, `SELECT c.table_name,c.column_name FROM information_schema.columns c JOIN information_schema.tables t USING(table_catalog,table_schema,table_name) WHERE c.table_schema='public' AND t.table_type='BASE TABLE' ORDER BY c.table_name,c.ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := map[string][]string{}
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatal(err)
		}
		result[table] = append(result[table], column)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
func nha204Snapshot(t *testing.T, ctx context.Context, s *Store, columns map[string][]string) map[string]string {
	t.Helper()
	result := map[string]string{}
	for table, names := range columns {
		quoted := make([]string, len(names))
		for i, name := range names {
			quoted[i] = pgx.Identifier{name}.Sanitize()
		}
		where := ""
		if table == "homeward_schema_migrations" {
			where = " WHERE version<=28"
		}
		query := `SELECT COALESCE(jsonb_agg(to_jsonb(s) ORDER BY to_jsonb(s)::text),'[]'::jsonb)::text FROM (SELECT ` + strings.Join(quoted, ",") + ` FROM ` + pgx.Identifier{"public", table}.Sanitize() + where + `) s`
		var value string
		if err := s.pool.QueryRow(ctx, query).Scan(&value); err != nil {
			t.Fatal(table, err)
		}
		result[table] = value
	}
	return result
}
func nha204AssertSnapshot(t *testing.T, want, got map[string]string) {
	t.Helper()
	for table, value := range want {
		if got[table] != value {
			t.Fatalf("native table changed: %s", table)
		}
	}
}
func nha204PriorWindow(t *testing.T, ctx context.Context, s *Store) prior28SchemaWindow {
	t.Helper()
	var w prior28SchemaWindow
	if err := s.pool.QueryRow(ctx, `SELECT singleton,format,schema_version,release_id,minimum_reader,minimum_writer,maximum_release,baseline_schema,baseline_plan,migration_plan FROM public.homeward_schema_window`).Scan(&w.singleton, &w.format, &w.schema, &w.release, &w.reader, &w.writer, &w.maximum, &w.baseline, &w.baselinePlan, &w.plan); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestNHA204Migration29AtomicRollbackAndResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	settings := testSettings(t, testCluster(t))
	s, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	if _, err := s.migrate(ctx, migrations[:28]); err != nil {
		t.Fatal(err)
	}
	baseline := schemaPlanSHA256(migrations[:28])
	if _, err := s.pool.Exec(ctx, `INSERT INTO public.homeward_schema_window(singleton,format,schema_version,release_id,minimum_reader,minimum_writer,maximum_release,baseline_schema,baseline_plan,migration_plan) VALUES(true,1,28,$1,$1,$1,$1,28,$2,$2)`, prior28SchemaRelease, baseline[:]); err != nil {
		t.Fatal(err)
	}
	admin := nha049AccountInput(t, "Synthetic Migration Administrator", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	nha204SeedPrior28Account(t, ctx, s, admin)
	account, _ := admin.ID.Value()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := s.pool.Exec(ctx, `INSERT INTO public.auth_sessions(id,account_id,account_revision,created_at,expires_at,revoked_at,current_generation,last_used_at) VALUES('historical-session:opaque',$1,1,$2,$3,$4,2,$4)`, account, now.Add(-time.Hour), now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	for generation := 1; generation <= 2; generation++ {
		digest := sha256.Sum256([]byte{byte(generation)})
		var consumed any
		state := 1
		if generation == 1 {
			state = 2
			consumed = now
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO public.auth_refresh_generations(session_id,generation,token_digest,state,issued_at,consumed_at) VALUES('historical-session:opaque',$1,$2,$3,$4,$5)`, generation, digest[:], state, now.Add(-time.Hour), consumed); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO public.registry_devices(id) VALUES('dev_synthetic'); INSERT INTO public.registry_entities(id,device_id,manifest_alias) VALUES('ent_synthetic','dev_synthetic','light.synthetic'); INSERT INTO public.registry_aliases(alias,entity_id,current) VALUES('light.synthetic','ent_synthetic',true)`); err != nil {
		t.Fatal(err)
	}
	actor, err := domain.ParseActorID("nha204-actor")
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]domain.CommandID, 2)
	for i, label := range []string{"nha204-accepted", "nha204-indeterminate"} {
		ids[i], err = domain.ParseCommandID(label)
		if err != nil {
			t.Fatal(err)
		}
		if _, created, outcome, err := s.AcceptCommandIntent(ctx, ids[i], actor, "synthetic-epoch", []byte(label)); err != nil || !created || outcome != TransactionCommitted {
			t.Fatal("seed intent", err)
		}
	}
	if moved, _, err := s.MoveCommandIntent(ctx, ids[1], IntentAccepted, IntentDispatching, nil, IntentNoError); err != nil || !moved {
		t.Fatal(err)
	}
	if moved, _, err := s.MoveCommandIntent(ctx, ids[1], IntentDispatching, IntentIndeterminate, []byte("synthetic unknown outcome"), IntentOutcomeIndeterminate); err != nil || !moved {
		t.Fatal(err)
	}
	columns := nha204Columns(t, ctx, s)
	before := nha204Snapshot(t, ctx, s, columns)
	if !nha204PriorWindow(t, ctx, s).validFor(baseline) {
		t.Fatal("fixture is not the delivered SQL28 window")
	}
	// Failure after all of SQL29 and its history insertion must roll back both.
	failed := append([]migration(nil), migrations...)
	failed = append(failed, migration{version: Version{CurrentVersion().Number() + 1}, sql: `INSERT INTO public.nha204_missing_relation VALUES(1)`})
	if _, err := s.migrate(ctx, failed); err == nil {
		t.Fatal("injected failure accepted")
	}
	nha204AssertSnapshot(t, before, nha204Snapshot(t, ctx, s, columns))
	if got := nha204Columns(t, ctx, s); !reflect.DeepEqual(got, columns) {
		t.Fatal("failed migration retained DDL")
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM public.homeward_schema_migrations WHERE version>28`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed migration retained history", err)
	}
	if version, err := s.Migrate(ctx); err != nil || version != CurrentVersion() {
		t.Fatal("resume migration", err)
	}
	delete(columns, "homeward_schema_window")
	delete(before, "homeward_schema_window")
	nha204AssertSnapshot(t, before, nha204Snapshot(t, ctx, s, columns))
	if nha204PriorWindow(t, ctx, s).validFor(baseline) {
		t.Fatal("prior28 reader window admitted SQL29")
	}
	names := map[string][]string{"homeward_registry_home": {"singleton", "resource_id"}, "homeward_api_resource_names": {"resource_type", "native_key", "resource_id"}}
	// Use actual schema columns rather than assuming API mapping storage layout.
	all := nha204Columns(t, ctx, s)
	for table := range names {
		names[table] = all[table]
		if len(names[table]) == 0 {
			t.Fatal("missing identity table", table)
		}
	}
	stable := nha204Snapshot(t, ctx, s, names)
	for i, label := range []string{"nha204-accepted", "nha204-indeterminate"} {
		record, created, _, err := s.AcceptCommandIntent(ctx, ids[i], actor, "synthetic-epoch", []byte(label))
		if err != nil || created {
			t.Fatal("replayed intent", err)
		}
		want := IntentAccepted
		if i == 1 {
			want = IntentIndeterminate
		}
		if record.State() != want {
			t.Fatal("intent state lost")
		}
	}
	s.Close()
	s, err = Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	if version, err := s.Migrate(ctx); err != nil || version != CurrentVersion() {
		t.Fatal("reopen migration", err)
	}
	nha204AssertSnapshot(t, before, nha204Snapshot(t, ctx, s, columns))
	nha204AssertSnapshot(t, stable, nha204Snapshot(t, ctx, s, names))
}
