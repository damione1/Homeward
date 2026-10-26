package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNHA036WindowModelRejectsUnknownAndZero(t *testing.T) {
	profile := compiledSchemaProfile()
	if !profile.Valid() || (SchemaProfile{}).Valid() || !CurrentSchemaBuild().Valid() || (SchemaBuild{}).Valid() {
		t.Fatal("compiled profile/build admission failed")
	}
	badProfiles := []SchemaProfile{{}, {
		baseline: Version{24}, successor: Version{25},
		baselineRelease: "homeward-window-1", successorRelease: "homeward-window-2",
	}}
	for _, change := range []func(*SchemaProfile){
		func(p *SchemaProfile) { p.baseline = Version{} },
		func(p *SchemaProfile) { p.successor = p.baseline },
		func(p *SchemaProfile) { p.successor = Version{schemaWindowSuccessor + 1} },
		func(p *SchemaProfile) { p.baselineRelease = "!unknown-release" },
		func(p *SchemaProfile) { p.successorRelease = p.baselineRelease },
	} {
		bad := profile
		change(&bad)
		badProfiles = append(badProfiles, bad)
	}
	for _, bad := range badProfiles {
		if bad.Valid() {
			t.Fatal("noncompiled profile admitted")
		}
	}
	for _, raw := range []string{"", "!unknown-release", "homeward-window-1", "homeward-window-2", strings.ToUpper(profile.BaselineRelease())} {
		if parseSchemaRelease(raw) != schemaReleaseInvalid {
			t.Fatalf("admitted release %q", raw)
		}
	}
	plan := sha256.Sum256([]byte("baseline"))
	next := sha256.Sum256([]byte("successor"))
	good := schemaWindow{format: 1, schema: int64(profile.Successor().Number()), release: schemaReleaseSuccessor, reader: schemaReleaseBaseline, writer: schemaReleaseBaseline, maximum: schemaReleaseSuccessor, baseline: int64(profile.Baseline().Number()), baselinePlan: plan, plan: next}
	if !(good.validFor(profile.Successor())) {
		t.Fatal("valid pair refused")
	}
	cases := []schemaWindow{{}}
	for _, change := range []func(*schemaWindow){
		func(w *schemaWindow) { w.format = 2 },
		func(w *schemaWindow) { w.schema = int64(profile.Successor().Number()) + 1 },
		func(w *schemaWindow) { w.release = schemaRelease(255) },
		func(w *schemaWindow) { w.reader = schemaReleaseInvalid },
		func(w *schemaWindow) { w.writer = schemaRelease(255) },
		func(w *schemaWindow) { w.maximum = schemaReleaseBaseline },
		func(w *schemaWindow) { w.baseline = 0 },
		func(w *schemaWindow) { w.baselinePlan = [32]byte{} },
		func(w *schemaWindow) { w.plan = w.baselinePlan },
	} {
		bad := good
		change(&bad)
		cases = append(cases, bad)
	}
	for i, bad := range cases {
		if bad.validFor(profile.Successor()) {
			t.Fatalf("invalid case %d admitted", i)
		}
	}
	if schemaPlanSHA256([]migration{{version: profile.Baseline(), sql: "ab"}, {version: profile.Successor(), sql: "c"}}) == schemaPlanSHA256([]migration{{version: profile.Baseline(), sql: "a"}, {version: profile.Successor(), sql: "bc"}}) {
		t.Fatal("ambiguous plan digest")
	}
}

func TestNHA036BaselineAndFutureDefault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, testSettings(t, testCluster(t)))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var release string
	var version int
	var digest []byte
	if err := store.pool.QueryRow(ctx, `SELECT release_id,schema_version,migration_plan FROM public.homeward_schema_window`).Scan(&release, &version, &digest); err != nil {
		t.Fatal(err)
	}
	build := CurrentSchemaBuild()
	if release != build.Release() || version != int(build.Version().Number()) || string(digest) != string(build.plan[:]) {
		t.Fatal("window disagrees with compiled authority")
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO public.homeward_schema_migrations(version) VALUES ($1)`, build.Version().Number()+1); err != nil {
		t.Fatal(err)
	}
	var classified *Error
	if _, err := store.Migrate(ctx); !errors.As(err, &classified) || classified.Kind() != FutureSchema {
		t.Fatalf("unknown future admitted: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `DELETE FROM public.homeward_schema_window`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); !errors.As(err, &classified) || classified.Kind() != FutureSchema {
		t.Fatalf("future without declaration admitted: %v", err)
	}
}
