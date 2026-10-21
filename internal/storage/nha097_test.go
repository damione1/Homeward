package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestNHA097Migration16RetainsOldRowsAndClosesValueTags(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	port := testCluster(t)
	store, err := Open(ctx, testSettings(t, port))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if version, err := store.migrate(ctx, migrations[:15]); err != nil || version.Number() != 15 {
		t.Fatalf("historical v15: %v %v", version, err)
	}
	for _, row := range []struct {
		entity, alias string
		kind          int
		boolean       any
		number        any
		counter       any
	}{
		{"helper-boolean", "input_boolean.legacy", 1, false, nil, nil},
		{"helper-number", "input_number.legacy", 2, nil, 0.0, nil},
		{"helper-counter", "counter.legacy", 3, nil, nil, int64(0)},
	} {
		if _, err := store.pool.Exec(ctx, `INSERT INTO public.registry_devices(id) VALUES($1)`, row.entity+"-device"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.pool.Exec(ctx, `INSERT INTO public.registry_entities(id,device_id,manifest_alias) VALUES($1,$2,$3)`, row.entity, row.entity+"-device", row.alias); err != nil {
			t.Fatal(err)
		}
		if _, err := store.pool.Exec(ctx, `INSERT INTO public.helper_values(entity_id,kind,boolean_value,number_value,counter_value,revision,updated_at) VALUES($1,$2,$3,$4,$5,1,now())`, row.entity, row.kind, row.boolean, row.number, row.counter); err != nil {
			t.Fatal(err)
		}
	}
	if version, err := store.migrate(ctx, migrations[:16]); err != nil || version.Number() != 16 {
		t.Fatalf("additive v16: %v %v", version, err)
	}
	var retained int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.helper_values WHERE revision=1 AND
		((kind=1 AND boolean_value=false) OR (kind=2 AND number_value=0) OR (kind=3 AND counter_value=0)) AND
		button_pressed_at IS NULL AND date_value IS NULL AND time_value IS NULL AND datetime_value IS NULL AND select_value IS NULL AND text_value IS NULL`).Scan(&retained); err != nil || retained != 3 {
		t.Fatalf("v15 helper rows changed: %d %v", retained, err)
	}
	_, err = store.pool.Exec(ctx, `UPDATE public.helper_values SET kind=10 WHERE entity_id='helper-counter'`)
	var sqlError *pgconn.PgError
	if !errors.As(err, &sqlError) || sqlError.Code != "23514" {
		t.Fatalf("unknown helper kind crossed SQL check: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO public.recorder_epochs(epoch,started_at) VALUES(decode(repeat('01',16),'hex'),now())`); err != nil {
		t.Fatal(err)
	}
	insertHistory := `INSERT INTO public.state_history(epoch,sequence,entity_id,entity_digest,instance_id,admitted_at,admitted_unix_seconds,admitted_nanos,revision,received_at,last_changed,last_updated,last_reported,source_session,availability,freshness,provenance,value_kind,payload,payload_digest,record_digest)
		VALUES(decode(repeat('01',16),'hex'),$1,'helper-boolean',decode(repeat('02',32),'hex'),'native-helper',now(),0,0,1,now(),now(),now(),now(),'synthetic-session',1,1,1,$2,decode('00','hex'),decode(repeat('03',32),'hex'),decode(repeat('04',32),'hex'))`
	if _, err := store.pool.Exec(ctx, insertHistory, 1, 6); err != nil {
		t.Fatalf("helper observation tag 6 refused: %v", err)
	}
	if _, err := store.pool.Exec(ctx, insertHistory, 2, 7); !errors.As(err, &sqlError) || sqlError.Code != "23514" {
		t.Fatalf("future observation tag crossed v16 SQL check: %v", err)
	}
}

func TestNHA097ConfigV1AndV2RestoreExactSchemasAndHashes(t *testing.T) {
	store, ctx := nha151Store(t)
	binding := nha151Binding(t, "helpers.configuration", "configuration.yaml")
	v1, bytesV1 := nha151Candidate(t, "configuration.yaml", "unchanged v1")
	_, oldGeneration := nha151Write(t, store, ctx, v1, binding, ExpectNoConfigActive())
	v2, err := bundle.CompileV2(filepath.Join("..", "config", "bundle", "testdata", "nha097"), "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	expected := nha151ExpectedActive(t, store, ctx, oldGeneration)
	_, newGeneration := nha151Write(t, store, ctx, v2, binding, expected)
	for _, fixture := range []struct {
		generation ConfigGenerationRevision
		candidate  bundle.Candidate
		schema     bundle.Schema
		profile    parser.SyntaxProfile
	}{
		{oldGeneration, v1, bundle.SchemaDeclarative1, parser.SyntaxProfileDeclarative},
		{newGeneration, v2, bundle.SchemaDeclarative2, parser.SyntaxProfileDeclarative2},
	} {
		snapshot, err := store.ReadConfigGeneration(ctx, fixture.generation)
		if err != nil || snapshot.Availability() != ConfigGenerationComplete {
			t.Fatalf("generation unavailable: %v", err)
		}
		restored, err := snapshot.Restore()
		if err != nil || restored.Schema() != fixture.schema || restored.Profile() != fixture.profile || restored.SHA256() != fixture.candidate.SHA256() {
			t.Fatalf("version/hash drift: %v", err)
		}
		versions := snapshot.Versions()
		if len(versions) != 1 {
			t.Fatal("source manifest drift")
		}
		if schema, ok := versions[0].Schema(); !ok || schema != fixture.schema {
			t.Fatal("stored schema drift")
		}
	}
	if sha256.Sum256(bytesV1) != v1.Sources()[0].SHA256() {
		t.Fatal("v1 exact source hash changed")
	}
}
