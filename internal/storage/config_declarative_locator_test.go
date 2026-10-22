package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/damione1/Homeward/scope"
	"github.com/jackc/pgx/v5"
)

func TestNHA055DeclarativeLocatorZeroAbsenceAndPresenceAreDistinct(t *testing.T) {
	if (DeclarativeWorkspaceLocator{}).Valid() {
		t.Fatal("zero locator was admitted")
	}
	absent := NoDeclarativeWorkspaceLocator()
	if _, present := absent.Group(); !absent.Valid() || present {
		t.Fatal("explicit old unobserved state is not distinct")
	}
	if _, err := NewDeclarativeWorkspaceLocator(ConfigDocumentID{}); err == nil {
		t.Fatal("zero group became a physical namespace")
	}
	group, _ := NewConfigDocumentID("workspace.not-a-document-id")
	locator, err := NewDeclarativeWorkspaceLocator(group)
	got, present := locator.Group()
	if err != nil || !locator.Valid() || !present || got != group {
		t.Fatal("typed namespace did not round trip", err)
	}
}

func TestNHA055DeclarativeLocatorRequiresOneEntryAndOneSchema(t *testing.T) {
	group, _ := NewConfigDocumentID("workspace.namespace")
	locator, _ := NewDeclarativeWorkspaceLocator(group)
	version := ConfigVersionSnapshot{kind: ConfigDocumentKindYAMLSource, path: "entry.yaml", present: true, schema: bundle.SchemaDeclarative1, profile: parser.SyntaxProfileDeclarative}
	valid := ConfigGenerationSnapshot{entry: "entry.yaml", manifestFormat: ConfigManifestCompiledV2, declarativeWorkspace: locator, versions: []ConfigVersionSnapshot{version}}
	if err := validateDeclarativeWorkspace(valid); err != nil {
		t.Fatal(err)
	}
	for _, test := range []string{"no entry", "no bundle", "mixed schema", "duplicate entry", "wrong profile", "legacy manifest"} {
		t.Run(test, func(t *testing.T) {
			candidate := valid
			candidate.versions = append([]ConfigVersionSnapshot(nil), valid.versions...)
			switch test {
			case "no entry":
				candidate.entry = "missing.yaml"
			case "no bundle":
				candidate.versions = nil
			case "mixed schema":
				other := version
				other.path = "part.yaml"
				other.schema = bundle.SchemaDeclarative2
				other.profile = parser.SyntaxProfileDeclarative2
				candidate.versions = append(candidate.versions, other)
			case "duplicate entry":
				candidate.versions = append(candidate.versions, version)
			case "wrong profile":
				candidate.versions[0].profile = parser.SyntaxProfileDeclarative2
			case "legacy manifest":
				candidate.manifestFormat = ConfigManifestLegacyV1
			}
			nha151Kind(t, validateDeclarativeWorkspace(candidate), ConfigIntegrity)
		})
	}
	incomplete := valid
	incomplete.versions = append([]ConfigVersionSnapshot(nil), valid.versions...)
	incomplete.versions[0].present = false
	incomplete.versions[0].schema = bundle.SchemaInvalid
	incomplete.versions[0].profile = parser.SyntaxProfileInvalid
	if err := validateDeclarativeWorkspace(incomplete); err != nil {
		t.Fatal("collection was confused with malformed membership", err)
	}
}

// This existing-generation fixture uses only the old registration/read ports.
// It is suitable for a real SQL24 parent as well as the eventual SQL26 chain.
func TestNHA055DeclarativeLocatorOldSQLGenerationRemainsUnobserved(t *testing.T) {
	f := nha029HistoryFixture(t, 256, nil)
	generation, err := f.store.ReadConfigGeneration(f.ctx, f.active.generation)
	if err != nil || !generation.DeclarativeWorkspace().Valid() {
		t.Fatal("legacy read broke on an absent SQL column", err)
	}
	if _, present := generation.DeclarativeWorkspace().Group(); present {
		t.Fatal("old SQL acquired an invented physical namespace")
	}
}

func nha055LocatorCompilation(t *testing.T) configcompilation.Context {
	t.Helper()
	revision, err := scope.BaselineV1().Value()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := hex.DecodeString(scope.BaselineIDsSHA256)
	if err != nil || len(raw) != sha256.Size {
		t.Fatal("invalid canonical digest", err)
	}
	var digest [sha256.Size]byte
	copy(digest[:], raw)
	input, err := configcompilation.NewContext(revision, digest, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return input
}

// Future SQL26 fixture. It qualifies storage membership/immutability and backup
// preservation, not a fake published runtime. No run engine/pilot is attached.
func TestNHA055DeclarativeLocatorSQLSealAndRestorePreserveContextWithoutReplay(t *testing.T) {
	f := nha029HistoryFixture(t, 256, nil)
	candidate, _ := nha151Candidate(t, "entry.yaml", "locator")
	binding := nha151Binding(t, "document.entry.distinct-from-group", "entry.yaml")
	group, _ := NewConfigDocumentID("workspace.namespace.distinct-from-document")
	locator, _ := NewDeclarativeWorkspaceLocator(group)
	compilation := nha055LocatorCompilation(t)
	var generation ConfigGenerationRevision
	attempt, err := RandomActivationAttemptID()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := ExpectConfigActive(f.active.generation, f.active.manifest)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := f.store.InConfigTransactionOutcome(f.ctx, func(tx *ConfigTx) error {
		refs, err := tx.SaveCandidate(candidate, []ConfigDocumentBinding{binding}, nha151Metadata(t))
		if err != nil {
			return err
		}
		refs = append(refs, f.active.ref)
		generation, err = tx.RegisterCompiledGenerationWithWorkspace(candidate.Entry(), sha256.Sum256([]byte("compiled locator fixture")), refs, locator, compilation)
		if err != nil {
			return err
		}
		if err := tx.ActivateGeneration(expected, generation); err != nil {
			return err
		}
		return tx.RecordActivationAttempt(attempt, nha151Metadata(t))
	})
	if err != nil || outcome != TransactionCommitted {
		t.Fatal("locator fixture failed before its assertions", outcome, err)
	}
	active, present, err := f.store.ReadActiveConfig(f.ctx)
	if err != nil || !present {
		t.Fatal(err)
	}
	got, located := active.DeclarativeWorkspace().Group()
	if !located || got != group || active.Entry() != candidate.Entry() {
		t.Fatal("sealed locator/entry changed")
	}
	stored, err := f.store.ReadConfigCompilationContext(f.ctx, active.Revision(), active.ManifestSHA256(), active.CandidateSHA256())
	if err != nil || stored.SHA256() != compilation.SHA256() {
		t.Fatal("locator displaced the exact compilation input", err)
	}
	if _, err := f.store.pool.Exec(f.ctx, `UPDATE public.config_generations SET declarative_workspace_group='forged' WHERE revision=$1`, generation.Number()); err == nil {
		t.Fatal("sealed locator was mutable")
	} else {
		nha151SQLState(t, err, "23514")
	}
	// SQL must reject a missing entry independently of the typed Go validator.
	outcome, err = f.store.InConfigTransactionOutcome(f.ctx, func(tx *ConfigTx) error {
		var invalid int64
		if err := tx.tx.QueryRow(f.ctx, `INSERT INTO public.config_generations(entry_path,candidate_hash,manifest_hash,entry_count,manifest_format,declarative_workspace_group) SELECT 'missing.yaml',candidate_hash,manifest_hash,entry_count,manifest_format,declarative_workspace_group FROM public.config_generations WHERE revision=$1 RETURNING revision`, generation.Number()).Scan(&invalid); err != nil {
			return err
		}
		if _, err := tx.tx.Exec(f.ctx, `INSERT INTO public.config_generation_entries(generation_revision,document_id,version_revision,source_hash) SELECT $1,document_id,version_revision,source_hash FROM public.config_generation_entries WHERE generation_revision=$2`, invalid, generation.Number()); err != nil {
			return err
		}
		_, err := tx.tx.Exec(f.ctx, `UPDATE public.config_generations SET sealed=true WHERE revision=$1`, invalid)
		return err
	})
	if outcome != TransactionNotCommitted || err == nil {
		t.Fatal("SQL seal accepted a locator with no entry")
	}
	nha151SQLState(t, err, "23514")
	mixedFile, err := parser.NewSourceFile("part-v2.yaml", []byte("input_boolean: {other: {initial: true}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	mixed, err := bundle.CompileSourcesV2(mixedFile.Path(), []parser.SourceFile{mixedFile})
	if err != nil {
		t.Fatal(err)
	}
	mixedBinding := nha151Binding(t, "document.v2-part", mixedFile.Path())
	outcome, err = f.store.InConfigTransactionOutcome(f.ctx, func(tx *ConfigTx) error {
		refs, err := tx.SaveCandidate(mixed, []ConfigDocumentBinding{mixedBinding}, nha151Metadata(t))
		if err != nil {
			return err
		}
		var invalid int64
		if err := tx.tx.QueryRow(f.ctx, `INSERT INTO public.config_generations(entry_path,candidate_hash,manifest_hash,entry_count,manifest_format,declarative_workspace_group) SELECT entry_path,candidate_hash,manifest_hash,entry_count+1,manifest_format,declarative_workspace_group FROM public.config_generations WHERE revision=$1 RETURNING revision`, generation.Number()).Scan(&invalid); err != nil {
			return err
		}
		if _, err := tx.tx.Exec(f.ctx, `INSERT INTO public.config_generation_entries(generation_revision,document_id,version_revision,source_hash) SELECT $1,document_id,version_revision,source_hash FROM public.config_generation_entries WHERE generation_revision=$2`, invalid, generation.Number()); err != nil {
			return err
		}
		hash := mixedFile.SHA256()
		if _, err := tx.tx.Exec(f.ctx, `INSERT INTO public.config_generation_entries(generation_revision,document_id,version_revision,source_hash) VALUES($1,$2,$3,$4)`, invalid, refs[0].Document().String(), refs[0].Revision().Number(), hash[:]); err != nil {
			return err
		}
		_, err = tx.tx.Exec(f.ctx, `UPDATE public.config_generations SET sealed=true WHERE revision=$1`, invalid)
		return err
	})
	if outcome != TransactionNotCommitted || err == nil {
		t.Fatal("SQL seal accepted a mixed-schema bundle")
	}
	nha151SQLState(t, err, "23514")
	readEffects := func() [3]int64 {
		var counts [3]int64
		if err := f.store.pool.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM public.command_intents),(SELECT count(*) FROM public.execution_history_records),(SELECT count(*) FROM public.scheduled_jobs)`).Scan(&counts[0], &counts[1], &counts[2]); err != nil {
			t.Fatal(err)
		}
		return counts
	}
	beforeEffects := readEffects()
	tx, err := f.store.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Match the real backup witness' canonical SQL encodings.
	_, err = tx.Exec(f.ctx, `SELECT set_config('TimeZone','UTC',true),set_config('DateStyle','ISO, YMD',true),set_config('IntervalStyle','postgres',true),set_config('extra_float_digits','3',true),set_config('bytea_output','hex',true)`)
	if err != nil {
		rollback(tx)
		t.Fatal(err)
	}
	tables, err := backupTables(f.ctx, tx)
	rollback(tx)
	if err != nil {
		t.Fatal(err)
	}
	restoreExpected, err := NewBackupRestoreExpectation(active.Revision(), active.ManifestSHA256(), tables)
	if err != nil {
		t.Fatal(err)
	}
	restored, outcome, err := f.store.RestoreBackupGeneration(f.ctx, restoreExpected, func(context.Context, pgx.Tx) error { return nil })
	if err != nil || outcome != TransactionCommitted || restored.Number() <= active.Revision().Number() {
		t.Fatal("real ConfigTx backup restore failed", outcome, err)
	}
	after, present, err := f.store.ReadActiveConfig(f.ctx)
	if err != nil || !present {
		t.Fatal(err)
	}
	afterGroup, located := after.DeclarativeWorkspace().Group()
	if !located || afterGroup != group || after.Entry() != active.Entry() || after.CandidateSHA256() != active.CandidateSHA256() {
		t.Fatal("restore lost exact locator, entry or compiled source identity")
	}
	stored, err = f.store.ReadConfigCompilationContext(f.ctx, after.Revision(), after.ManifestSHA256(), after.CandidateSHA256())
	if err != nil || stored.SHA256() != compilation.SHA256() {
		t.Fatal("restore lost the exact compilation input", err)
	}
	if beforeEffects != ([3]int64{}) || readEffects() != beforeEffects {
		t.Fatal("configuration restore replayed a run, schedule or action")
	}
}
