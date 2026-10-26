package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
)

// These identify the actual delivered NHA036 parent, not its synthetic26
// successor. Its normal/race baseline source inventories are byte-identical.
const nha055Prior25Commit = "c379ecb35ff7a201789d116cdc43ee6b4911e779"
const nha055Prior25Sources = "1e960b763f8a71a2eeb4314e67e1ce924410847a6c38f548499e473569944d70"
const nha055Prior25Plan = "a6448f0921326107fecf14f6f5fa2a2a6b41053d136864bb35ba7ae7f2032863"
const nha055Prior25Release = "homeward-window-25-26-baseline"
const nha055Prior25NormalManifest = "6a44457ce7dad2da0108eea8c08f3504a57fcdf08ee0eb56402153508ae8fd98"
const nha055Prior25RaceManifest = "bc5d22ba618bb73889a85aeeaa005594cd1ab402565b1bfdefda57f542a6f8ab"

func nha055Prior25Artifact(t *testing.T) (nha036Artifact, nha036SchemaProfile) {
	t.Helper()
	path := os.Getenv("HOMEWARD_NHA055_PARENT25_ARTIFACTS")
	if !filepath.IsAbs(path) {
		t.Fatal("NHA055 requires the retained real parent25 artifact manifest; normal/race must match this test")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantManifest := nha055Prior25NormalManifest
	if nha036Race {
		wantManifest = nha055Prior25RaceManifest
	}
	manifestDigest := sha256.Sum256(raw)
	if hex.EncodeToString(manifestDigest[:]) != wantManifest {
		t.Fatal("NHA055 parent manifest differs from the retained normal/race delivery evidence")
	}
	var manifest nha036Artifacts
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	wantProfile := nha036SchemaProfile{25, 26, nha055Prior25Release, "homeward-window-25-26-successor"}
	a, present := manifest.Artifacts["baseline"]
	if manifest.Format != 1 || manifest.Race != nha036Race || !strings.HasPrefix(manifest.Go, "go version go1.27.2 ") || manifest.Profile != wantProfile ||
		!present || a.Schema != 25 || a.Release != nha055Prior25Release || a.MinimumReader != nha055Prior25Release || a.MinimumWriter != nha055Prior25Release ||
		a.PlanSHA256 != nha055Prior25Plan || a.SourcesSHA256 != nha055Prior25Sources {
		t.Fatal("NHA055 parent artifact differs from the delivered SQL25 baseline or race mode")
	}
	for path, expected := range map[string]string{a.Test: a.TestSHA256, a.Homed: a.HomedSHA256, a.SourceManifest: nha055Prior25Sources} {
		if !filepath.IsAbs(path) || len(expected) != 64 || expected == strings.Repeat("0", 64) || nha036FileDigest(t, path) != expected {
			t.Fatal("NHA055 real parent artifact or source manifest changed")
		}
	}
	raw, err = os.ReadFile(a.SourceManifest)
	if err != nil {
		t.Fatal(err)
	}
	var sources map[string]string
	if err := json.Unmarshal(raw, &sources); err != nil {
		t.Fatal(err)
	}
	if err := nha036VerifySourceInventory(filepath.Join(filepath.Dir(a.SourceManifest), "source"), sources); err != nil {
		t.Fatalf("NHA055 delivered parent source changed: %v", err)
	}
	t.Logf("NHA055 real-parent commit=%s schema=25 sources=%s plan=%s homed=%s test=%s race=%t", nha055Prior25Commit, a.SourcesSHA256, a.PlanSHA256, a.HomedSHA256, a.TestSHA256, manifest.Race)
	return a, manifest.Profile
}

// The parent reports its OWN compiled profile. The current profile must never
// be substituted for an old25 child identity just to reuse a readiness helper.
func nha055Prior25Fixture(t *testing.T, old nha036Artifact, profile nha036SchemaProfile) *nha029C5Fixture {
	t.Helper()
	f := nha029C5NewFixture(t) // Opens an empty DB; the actual old child migrates it.
	child := nha036Start(t, f, old.Test, false)
	ready := child.ready(t, nil)
	var identity nha036BuildIdentity
	want := nha036BuildIdentity{old.Release, old.Schema, old.PlanSHA256, profile, old.MinimumReader, old.MinimumWriter}
	if json.Unmarshal([]byte(ready.value), &identity) != nil || identity != want || ready.count != 25 {
		t.Fatal("NHA055 fixture was not created by the exact real parent25 executable")
	}
	nha036Operate(t, child, "seed", nil)
	child.stop(t)
	nha036ObserveBusiness(t, f, false, 1, false)
	return f
}

// Hash only: source bytes, encrypted material and human records are never
// printed. Capture the window separately because a negative fixture may remove
// its table. The DDL digest covers columns, enums, constraints and config guards.
const nha055MigrationDDL = `SELECT sha256(convert_to(jsonb_build_array(
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY x.table_name,x.ordinal_position) FROM information_schema.columns x WHERE x.table_schema='public'),
 (SELECT jsonb_agg(jsonb_build_array(t.typname,e.enumlabel,e.enumsortorder) ORDER BY t.typname,e.enumsortorder) FROM pg_enum e JOIN pg_type t ON t.oid=e.enumtypid JOIN pg_namespace n ON n.oid=t.typnamespace WHERE n.nspname='public'),
 (SELECT jsonb_agg(jsonb_build_array(c.relname,k.conname,pg_get_constraintdef(k.oid)) ORDER BY c.relname,k.conname) FROM pg_constraint k JOIN pg_class c ON c.oid=k.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'),
 (SELECT jsonb_agg(pg_get_functiondef(p.oid) ORDER BY p.proname) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public' AND p.proname LIKE 'homeward_config_%')
)::text,'UTF8'))`

const nha055MigrationData = `SELECT sha256(convert_to(jsonb_build_array(
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY version) FROM public.homeward_schema_migrations x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM public.config_documents x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY document_id,revision) FROM public.config_versions x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY revision) FROM public.config_generations x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY generation_revision,document_id) FROM public.config_generation_entries x),
 (SELECT jsonb_agg(to_jsonb(x)) FROM public.config_active x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY command_id) FROM public.command_intents x)
)::text,'UTF8'))`

type nha055MigrationFootprint struct {
	ddl, data, business, registry, humans, window [sha256.Size]byte
	windowPresent                                 bool
}

func nha055MigrationSnapshot(t *testing.T, f *nha029C5Fixture) nha055MigrationFootprint {
	t.Helper()
	s := nha055MigrationFootprint{
		ddl:      nha029C5RestoreDigest(t, f.ctx, f.observer, nha055MigrationDDL),
		data:     nha029C5RestoreDigest(t, f.ctx, f.observer, nha055MigrationData),
		business: nha029C5RestoreDigest(t, f.ctx, f.observer, nha029C5RestoreBusinessDigest),
		registry: nha029C5RestoreDigest(t, f.ctx, f.observer, nha036RegistryDigest),
		humans:   nha029C5RestoreDigest(t, f.ctx, f.observer, nha029C5RestoreHumanDigest),
	}
	if err := f.observer.QueryRow(f.ctx, `SELECT to_regclass('public.homeward_schema_window') IS NOT NULL`).Scan(&s.windowPresent); err != nil {
		t.Fatal(err)
	}
	if s.windowPresent {
		s.window = nha029C5RestoreDigest(t, f.ctx, f.observer, `SELECT sha256(convert_to(COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb)::text,'UTF8')) FROM public.homeward_schema_window x`)
	}
	return s
}

func TestNHA055Migration25ToCurrentRejectsDamagedPriorBeforeDDL(t *testing.T) {
	old, profile := nha055Prior25Artifact(t)
	for _, tc := range []struct{ name, sql string }{
		{"absent-table", `DROP TABLE public.homeward_schema_window`},
		{"absent-row", `DELETE FROM public.homeward_schema_window`},
		{"duplicate-row", `ALTER TABLE public.homeward_schema_window DROP CONSTRAINT homeward_schema_window_singleton_check; ALTER TABLE public.homeward_schema_window DROP CONSTRAINT homeward_schema_window_pkey; INSERT INTO public.homeward_schema_window SELECT * FROM public.homeward_schema_window`},
		{"false-singleton", `ALTER TABLE public.homeward_schema_window DROP CONSTRAINT homeward_schema_window_singleton_check; UPDATE public.homeward_schema_window SET singleton=false`},
		{"format", `UPDATE public.homeward_schema_window SET format=2`},
		{"schema", `UPDATE public.homeward_schema_window SET schema_version=26`},
		{"baseline", `UPDATE public.homeward_schema_window SET baseline_schema=24`},
		{"release", `UPDATE public.homeward_schema_window SET release_id='homeward-window-25-26-successor'`},
		{"reader", `UPDATE public.homeward_schema_window SET minimum_reader='!unknown'`},
		{"writer", `UPDATE public.homeward_schema_window SET minimum_writer='homeward-window-25-26-successor'`},
		{"maximum", `UPDATE public.homeward_schema_window SET maximum_release='!unknown'`},
		{"baseline-plan", `UPDATE public.homeward_schema_window SET baseline_plan=decode(repeat('ab',32),'hex')`},
		{"migration-plan", `UPDATE public.homeward_schema_window SET migration_plan=decode(repeat('cd',32),'hex')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := nha055Prior25Fixture(t, old, profile)
			if _, err := f.observer.Exec(f.ctx, tc.sql); err != nil {
				t.Fatal(err)
			}
			before := nha055MigrationSnapshot(t, f)
			var classified *storage.Error
			if _, err := f.store.Migrate(f.ctx); !errors.As(err, &classified) || classified.Kind() != storage.InconsistentSchema {
				t.Fatalf("damaged prior25 contract did not fail closed: %v", err)
			}
			if after := nha055MigrationSnapshot(t, f); after != before || len(f.effects) != 0 {
				t.Fatal("refused prior25 upgrade changed DDL, history, source, window or business state")
			}
		})
	}
}

func TestNHA055Migration25ToCurrentChecksPriorAfterMigrationLock(t *testing.T) {
	old, profile := nha055Prior25Artifact(t)
	f := nha055Prior25Fixture(t, old, profile)
	lock, err := f.observer.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(context.Background())
	if _, err := lock.Exec(f.ctx, `SELECT pg_advisory_xact_lock(481316,27)`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := f.store.Migrate(f.ctx); done <- err }()
	nha042ServerWait(t, "current migrator waiting on the existing migration lock", func() bool {
		var waiting bool
		if err := lock.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE '%pg_advisory_xact_lock(481316, 27)%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		return waiting
	})
	if _, err := lock.Exec(f.ctx, `UPDATE public.homeward_schema_window SET minimum_writer='!revoked-before-lock'`); err != nil {
		t.Fatal(err)
	}
	if err := lock.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	before := nha055MigrationSnapshot(t, f)
	var classified *storage.Error
	select {
	case err := <-done:
		if !errors.As(err, &classified) || classified.Kind() != storage.InconsistentSchema {
			t.Fatalf("migration did not re-read the contract under its acquired lock: %v", err)
		}
	case <-f.ctx.Done():
		t.Fatal("current migration did not join after the predecessor lock was released")
	}
	if after := nha055MigrationSnapshot(t, f); after != before {
		t.Fatal("locked predecessor refusal changed state")
	}
}

func TestNHA055Migration25ToCurrentPreservesDataAndRefusesActualOldHomed(t *testing.T) {
	old, profile := nha055Prior25Artifact(t)
	current := nha036LoadArtifacts(t).Artifacts["baseline"]
	if current.Schema != storage.CurrentSchemaBuild().Version().Number() || current.Release != storage.CurrentSchemaBuild().Release() || current.MinimumReader != current.Release || current.MinimumWriter != current.Release {
		t.Fatal("current baseline must match compiled release and require its own readers/writers")
	}
	f := nha055Prior25Fixture(t, old, profile)
	nha036Homed(t, f, old, true)
	a := nha029C5RestoreMarkSQL(t, f.ctx, f.store)
	history := nha029C5RestoreDigest(t, f.ctx, f.observer, nha036HistoryDigest, 25)
	before := nha055MigrationSnapshot(t, f)
	version, err := f.store.Migrate(f.ctx)
	if err != nil || version.Number() != current.Schema {
		t.Fatalf("actual delivered25 to current baseline migration failed: %v", err)
	}
	nha055AssertCurrentWindow(t, f, current)
	nha036ObserveBusiness(t, f, false, 1, false)
	after := nha055MigrationSnapshot(t, f)
	if after.business != before.business || after.registry != before.registry || after.humans != before.humans ||
		nha029C5RestoreDigest(t, f.ctx, f.observer, nha036HistoryDigest, 25) != history || len(f.effects) != 0 {
		t.Fatal("current migration changed prior data or dispatched an action")
	}
	if version, err := f.store.Migrate(f.ctx); err != nil || version.Number() != current.Schema || nha055MigrationSnapshot(t, f) != after {
		t.Fatal("second current migration changed committed DDL/history", err)
	}
	nha036Homed(t, f, current, true)
	b := nha029C5RestoreMarkSQL(t, f.ctx, f.store)
	if b.revision != a.revision || b.manifest != a.manifest || b.source != a.source || len(f.effects) != 0 {
		t.Fatal("current cold start changed active authority or replayed an action")
	}
	for _, native := range []bool{false, true} {
		if native {
			nha055RetainNative26Sources(t, f)
		}
		beforeRefusal := nha055MigrationSnapshot(t, f)
		nha036Homed(t, f, old, false)
		if nha055MigrationSnapshot(t, f) != beforeRefusal || len(f.effects) != 0 {
			t.Fatal("old25 refusal changed the current database")
		}
	}
	t.Logf("NHA055 real25-to-current schema=%d parent=%s old_homed=%s current_homed=%s current_plan=%s old25_refused_before_READY=true native_sources_retained=true effects=0", current.Schema, nha055Prior25Commit, old.HomedSHA256, current.HomedSHA256, current.PlanSHA256)
}

func nha055AssertCurrentWindow(t *testing.T, f *nha029C5Fixture, current nha036Artifact) {
	t.Helper()
	var schema, baseline, rows int
	var release, reader, writer, maximum string
	var baselinePlan, plan []byte
	if err := f.observer.QueryRow(f.ctx, `SELECT schema_version,baseline_schema,release_id,minimum_reader,minimum_writer,maximum_release,baseline_plan,migration_plan,(SELECT count(*) FROM public.homeward_schema_migrations) FROM public.homeward_schema_window WHERE singleton`).Scan(&schema, &baseline, &release, &reader, &writer, &maximum, &baselinePlan, &plan, &rows); err != nil {
		t.Fatal(err)
	}
	if schema != int(current.Schema) || baseline != int(current.Schema) || rows != int(current.Schema) || release != current.Release || reader != release || writer != release || maximum != release ||
		hex.EncodeToString(plan) != current.PlanSHA256 || !bytes.Equal(plan, baselinePlan) {
		t.Fatal("current window does not match the native compiled plan and reader/writer minima")
	}
}

// Retained storage proof only: the context/source digest below does not claim
// Owner publication or runtime replay. Those composed055 tests remain separate.
func nha055RetainNative26Sources(t *testing.T, f *nha029C5Fixture) {
	t.Helper()
	active, present, err := f.store.ReadActiveConfig(f.ctx)
	if err != nil || !present {
		t.Fatal(err)
	}
	author, err := storage.NewConfigAuthor("NHA055 real migration fixture")
	if err != nil {
		t.Fatal(err)
	}
	source, err := storage.NewConfigSource("retained native26 metadata; no runtime publication")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := storage.NewConfigVersionMetadata(author, source)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := registry.LoadManifest(scope.BaselineV1())
	if err != nil {
		t.Fatal(err)
	}
	revision, present := manifest.Revision()
	if !present {
		t.Fatal("fixture manifest has no revision")
	}
	manifestRevision, err := revision.Value()
	if err != nil {
		t.Fatal(err)
	}
	digest, present := manifest.Digest()
	if !present {
		t.Fatal("fixture manifest has no digest")
	}
	compilation, err := configcompilation.NewContext(manifestRevision, digest, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var refs []storage.ConfigVersionRef
	for _, version := range active.Versions() {
		refs = append(refs, version.Ref())
	}
	var retained storage.ConfigGenerationRevision
	compiled := sha256.Sum256([]byte("NHA055 retained native26 storage fixture; not an executable compilation"))
	outcome, err := f.store.InConfigTransactionOutcome(f.ctx, func(tx *storage.ConfigTx) error {
		for _, identity := range []string{"dashboard.nha055_migration_a", "dashboard.nha055_migration_b"} {
			descriptor, err := storage.DashboardSourceDescriptor(identity, "shared.yaml")
			if err != nil {
				return err
			}
			id, err := storage.NewConfigDocumentID("retained." + identity)
			if err != nil {
				return err
			}
			binding, err := storage.NewConfigDocumentBinding(id, "shared.yaml")
			if err != nil {
				return err
			}
			file, err := parser.NewSourceFile("shared.yaml", []byte("views: []\n"))
			if err != nil {
				return err
			}
			ref, err := tx.SaveEditableSource(descriptor, binding, file, metadata)
			if err != nil {
				return err
			}
			refs = append(refs, ref)
		}
		retained, err = tx.RegisterCompiledGenerationWithContext(active.Entry(), compiled, refs, compilation)
		return err
	})
	if err != nil || outcome != storage.TransactionCommitted {
		t.Fatal("real26 grouped source storage or context seal failed", err)
	}
	sealed, err := f.store.ReadConfigGeneration(f.ctx, retained)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := f.store.ReadConfigCompilationContext(f.ctx, retained, sealed.ManifestSHA256(), compiled)
	if err != nil || stored.SHA256() != compilation.SHA256() {
		t.Fatal("native26 context did not remain bound to the retained generation", err)
	}
	if current, present, err := f.store.ReadActiveConfig(f.ctx); err != nil || !present || current.Revision() != active.Revision() {
		t.Fatal("retained native storage fixture changed the active generation", err)
	}
	before := nha055MigrationSnapshot(t, f)
	outcome, err = f.store.InConfigTransactionOutcome(f.ctx, func(tx *storage.ConfigTx) error {
		_, err := tx.RegisterCompiledGeneration(active.Entry(), compiled, refs)
		return err
	})
	var transaction, conflict *storage.Error
	if outcome != storage.TransactionNotCommitted || !errors.As(err, &transaction) || transaction.Kind() != storage.TransactionFailed ||
		!errors.As(transaction.Unwrap(), &conflict) || conflict.Kind() != storage.ConfigConflict || nha055MigrationSnapshot(t, f) != before {
		t.Fatal("native26 generation without compilation context was not rejected atomically", outcome, err)
	}
}

// Keep this assertion local to the real bridge: malformed predecessor storage
// cannot turn a canceled operation into an integrity classification or a write.
func TestNHA055Migration25ToCurrentCanceledContextHasNoWrites(t *testing.T) {
	old, profile := nha055Prior25Artifact(t)
	f := nha055Prior25Fixture(t, old, profile)
	before := nha055MigrationSnapshot(t, f)
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := f.store.Migrate(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("migration lost the canceled context: %v", err)
	}
	if nha055MigrationSnapshot(t, f) != before {
		t.Fatal("canceled prior25 migration changed state")
	}
}
