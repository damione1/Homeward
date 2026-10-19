package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func nha151SQLState(t *testing.T, err error, want string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != want {
		t.Fatalf("wanted SQLSTATE %s, got %v", want, err)
	}
}

func nha151Store(t *testing.T) (*Store, context.Context) {
	t.Helper()
	store, ctx := nha151UninitializedStore(t)
	if _, err := store.BootstrapMFAPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	return store, ctx
}

// Initialization tests need the migrated database before runtime metadata exists.
func nha151UninitializedStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	port := testCluster(t)
	s, err := Open(ctx, testSettings(t, port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if version, err := s.Migrate(ctx); err != nil || version != CurrentVersion() {
		t.Fatalf("migration chain: %v %v", version, err)
	}
	return s, ctx
}

func nha151Candidate(t *testing.T, path, label string) (bundle.Candidate, []byte) {
	t.Helper()
	data := []byte(fmt.Sprintf("# exact source: %s\ninput_boolean:\n  flag:\n    name: %q\n", label, label))
	source, err := parser.NewSourceFile(path, data)
	if err != nil {
		t.Fatal(err)
	}
	c, err := bundle.CompileSources(path, []parser.SourceFile{source})
	if err != nil || !c.Valid() {
		t.Fatalf("compile %s: %v", label, err)
	}
	return c, data
}

func nha151TwoSource(t *testing.T, label string) bundle.Candidate {
	t.Helper()
	main, err := parser.NewSourceFile("configuration.yaml",
		[]byte(fmt.Sprintf("# main %s\ninput_boolean: !include parts/boolean.yaml\n", label)))
	if err != nil {
		t.Fatal(err)
	}
	part, err := parser.NewSourceFile("parts/boolean.yaml",
		[]byte(fmt.Sprintf("# part %s\nflag:\n  name: %q\n", label, label)))
	if err != nil {
		t.Fatal(err)
	}
	c, err := bundle.CompileSources("configuration.yaml", []parser.SourceFile{part, main})
	if err != nil || !c.Valid() {
		t.Fatalf("two-source candidate: %v", err)
	}
	return c
}

func nha151Binding(t *testing.T, id, path string) ConfigDocumentBinding {
	t.Helper()
	document, err := NewConfigDocumentID(id)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := NewConfigDocumentBinding(document, path)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func nha151Metadata(t *testing.T) ConfigVersionMetadata {
	t.Helper()
	author, err := NewConfigAuthor("Damien test")
	if err != nil {
		t.Fatal(err)
	}
	source, err := NewConfigSource("isolated fixture")
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigVersionMetadata(author, source)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func nha151Kind(t *testing.T, err error, want Kind) {
	t.Helper()
	var found *Error
	for err != nil {
		if errors.As(err, &found) && found.Kind() == want {
			return
		}
		var wrapped *Error
		if errors.As(err, &wrapped) {
			err = wrapped.Unwrap()
		} else {
			break
		}
	}
	t.Fatalf("want storage kind %v, got %v", want, err)
}

func nha151Write(t *testing.T, s *Store, ctx context.Context, c bundle.Candidate, binding ConfigDocumentBinding, active ExpectedConfigActive) (ConfigVersionRef, ConfigGenerationRevision) {
	t.Helper()
	var ref ConfigVersionRef
	var generation ConfigGenerationRevision
	outcome, err := s.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
		refs, err := tx.SaveCandidate(c, []ConfigDocumentBinding{binding}, nha151Metadata(t))
		if err != nil {
			return err
		}
		ref = refs[0]
		generation, err = tx.RegisterGeneration(c, refs)
		if err != nil {
			return err
		}
		if active.kind != configActiveExpectationUnknown {
			return tx.ActivateGeneration(active, generation)
		}
		return nil
	})
	if outcome != TransactionCommitted || err != nil {
		t.Fatalf("write outcome=%v err=%v", outcome, err)
	}
	return ref, generation
}

func TestNHA151RetentionAndHistoricalDiagnosis(t *testing.T) {
	s, ctx := nha151Store(t)
	binding := nha151Binding(t, "home.config", "configuration.yaml")
	cA, exactA := nha151Candidate(t, binding.Path(), "A")
	first, firstGen := nha151Write(t, s, ctx, cA, binding, ExpectNoConfigActive())
	secondCandidate, _ := nha151Candidate(t, binding.Path(), "B")
	second, secondGen := nha151Write(t, s, ctx, secondCandidate, binding, ExpectedConfigActive{})
	if second.Revision().Number() != 2 {
		t.Fatal("second revision did not advance")
	}
	for i := 3; i <= 25; i++ {
		c, _ := nha151Candidate(t, binding.Path(), fmt.Sprintf("version %02d", i))
		ref, _ := nha151Write(t, s, ctx, c, binding, ExpectedConfigActive{})
		if ref.Revision().Number() != int64(i) {
			t.Fatalf("revision %d = %d", i, ref.Revision().Number())
		}
	}
	var count, minimum, maximum int
	if err := s.pool.QueryRow(ctx, `SELECT count(*),min(revision),max(revision) FROM public.config_versions
		WHERE document_id=$1`, binding.ID().String()).Scan(&count, &minimum, &maximum); err != nil {
		t.Fatal(err)
	}
	if count != configVersionLimit || minimum != 1 || maximum != 25 {
		t.Fatalf("retention count=%d first=%d last=%d", count, minimum, maximum)
	}
	for revision := 2; revision <= 6; revision++ {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.config_versions WHERE document_id=$1 AND revision=$2)`,
			binding.ID().String(), revision).Scan(&exists); err != nil || exists {
			t.Fatalf("old revision %d retained: %t %v", revision, exists, err)
		}
	}
	active, ok, err := s.ReadActiveConfig(ctx)
	if err != nil || !ok || active.Revision() != firstGen || active.Availability() != ConfigGenerationComplete {
		t.Fatalf("active lost: %v %t %v", active, ok, err)
	}
	restored, err := active.Restore()
	if err != nil || restored.SHA256() != cA.SHA256() || !bytes.Equal(restored.Sources()[0].Bytes(), exactA) {
		t.Fatalf("active restoration: %v", err)
	}
	versions := active.Versions()
	if len(versions) != 1 || versions[0].Ref() != first || versions[0].SHA256() != sha256.Sum256(exactA) {
		t.Fatal("active version metadata mismatch")
	}
	if parent, present := versions[0].Parent(); present || parent.Valid() {
		t.Fatal("first version has a parent")
	}
	if author, ok := versions[0].Author(); !ok || !author.Valid() {
		t.Fatal("author omitted")
	}
	if source, ok := versions[0].Source(); !ok || !source.Valid() {
		t.Fatal("source omitted")
	}
	if schema, ok := versions[0].Schema(); !ok || schema != bundle.SchemaDeclarative1 {
		t.Fatal("schema omitted")
	}
	if at, ok := versions[0].AcceptedAt(); !ok || at.IsZero() {
		t.Fatal("server date omitted")
	}
	history, err := s.ReadConfigGeneration(ctx, secondGen)
	if err != nil || history.Availability() != ConfigGenerationIncomplete {
		t.Fatalf("historical availability: %v %v", history.Availability(), err)
	}
	if len(history.Versions()) != 1 || history.Versions()[0].Ref() != second || history.Versions()[0].Present() {
		t.Fatal("purged reference not diagnosed")
	}
	if _, err := history.Restore(); err == nil {
		t.Fatal("incomplete history restored")
	} else {
		nha151Kind(t, err, ConfigIncomplete)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE public.config_active SET generation_revision=$1,activated_at=clock_timestamp()
		WHERE singleton`, secondGen.Number()); err == nil {
		t.Fatal("SQL activated incomplete history")
	} else {
		nha151SQLState(t, err, "23514")
	}
	expectFirst, _ := ExpectConfigActive(firstGen)
	outcome, err := s.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
		return tx.ActivateGeneration(expectFirst, secondGen)
	})
	if outcome != TransactionNotCommitted || err == nil {
		t.Fatal("repository activated incomplete history")
	}
	hash := sha256.Sum256(exactA)
	if _, err := s.pool.Exec(ctx, `INSERT INTO public.config_versions
		(document_id,revision,parent_revision,source_hash,author,source,schema_version,syntax_profile,yaml_source)
		VALUES ($1,2,1,$2,'tester','fixture','ha-declarative-1','ha-2026.10/declarative-1',$3)`,
		binding.ID().String(), hash[:], exactA); err == nil {
		t.Fatal("purged revision recycled by SQL")
	} else {
		nha151SQLState(t, err, "23514")
	}
	if _, err := s.ReadConfigGeneration(ctx, ConfigGenerationRevision{generation: 99999}); err == nil {
		t.Fatal("missing generation accepted")
	} else {
		nha151Kind(t, err, ConfigNotFound)
	}
}

func TestNHA151ABAAndImmutableSQL(t *testing.T) {
	s, ctx := nha151Store(t)
	binding := nha151Binding(t, "home.config", "configuration.yaml")
	cA, exactA := nha151Candidate(t, binding.Path(), "A")
	refA1, genA1 := nha151Write(t, s, ctx, cA, binding, ExpectNoConfigActive())
	cB, _ := nha151Candidate(t, binding.Path(), "B")
	expectA1, _ := ExpectConfigActive(genA1)
	refB, genB := nha151Write(t, s, ctx, cB, binding, expectA1)
	expectB, _ := ExpectConfigActive(genB)
	refA2, genA2 := nha151Write(t, s, ctx, cA, binding, expectB)
	if refA1.Revision().Number() != 1 || refB.Revision().Number() != 2 || refA2.Revision().Number() != 3 ||
		genA1.Number() >= genB.Number() || genB.Number() >= genA2.Number() {
		t.Fatal("A/B/A revisions not monotone")
	}
	g1, err := s.ReadConfigGeneration(ctx, genA1)
	if err != nil {
		t.Fatal(err)
	}
	active, ok, err := s.ReadActiveConfig(ctx)
	if err != nil || !ok {
		t.Fatalf("active: %v", err)
	}
	if g1.CandidateSHA256() != active.CandidateSHA256() ||
		g1.ManifestSHA256() == active.ManifestSHA256() ||
		g1.Versions()[0].SHA256() != active.Versions()[0].SHA256() ||
		!bytes.Equal(active.Versions()[0].yaml, exactA) {
		t.Fatal("candidate/source and version manifest hashes conflated")
	}
	if parent, present := active.Versions()[0].Parent(); !present || parent != refB.Revision() {
		t.Fatal("A2 parent is not B")
	}
	copied, ok := active.Versions()[0].Bytes()
	if !ok {
		t.Fatal("present source omitted")
	}
	copied[0] ^= 1
	again, _ := active.Versions()[0].Bytes()
	if bytes.Equal(copied, again) {
		t.Fatal("source bytes alias mutable snapshot")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE public.config_versions SET author='changed' WHERE document_id=$1 AND revision=3`, binding.ID().String()); err == nil {
		t.Fatal("version update accepted")
	} else {
		nha151SQLState(t, err, "23514")
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM public.config_versions WHERE document_id=$1 AND revision=3`, binding.ID().String()); err == nil {
		t.Fatal("direct version delete accepted")
	} else {
		nha151SQLState(t, err, "23514")
	}
	outcome, err := s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('homeward.config_gc','on',true)`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM public.config_versions WHERE document_id=$1 AND revision=3`, binding.ID().String())
		return err
	})
	if outcome != TransactionNotCommitted || err == nil {
		t.Fatal("active version deleted through GC flag")
	}
	for _, statement := range []string{
		`DELETE FROM public.config_active`,
		`INSERT INTO public.config_active(singleton) VALUES (true)`,
		`TRUNCATE public.config_active`,
		`TRUNCATE public.config_versions`,
		`TRUNCATE public.config_generation_entries`,
		`UPDATE public.config_documents SET logical_path='rewritten.yaml' WHERE id='home.config'`,
		`UPDATE public.config_documents SET last_revision=1 WHERE id='home.config'`,
		`DELETE FROM public.config_documents WHERE id='home.config'`,
		`UPDATE public.config_generations SET entry_path='rewritten.yaml' WHERE revision=1`,
		`DELETE FROM public.config_generation_entries WHERE generation_revision=1`,
	} {
		if _, err := s.pool.Exec(ctx, statement); err == nil {
			t.Fatalf("SQL mutation accepted: %s", statement)
		} else {
			nha151SQLState(t, err, "23514")
		}
	}
	secondBinding := nha151Binding(t, "home.second", "second.yaml")
	secondCandidate, _ := nha151Candidate(t, secondBinding.Path(), "second")
	secondRef, _ := nha151Write(t, s, ctx, secondCandidate, secondBinding, ExpectedConfigActive{})
	secondHash := secondCandidate.Sources()[0].SHA256()
	if _, err := s.pool.Exec(ctx, `INSERT INTO public.config_generation_entries(generation_revision,document_id,version_revision,source_hash)
		VALUES ($1,$2,$3,$4)`, genA1.Number(), secondRef.Document().String(), secondRef.Revision().Number(), secondHash[:]); err == nil {
		t.Fatal("late manifest insertion accepted")
	} else {
		nha151SQLState(t, err, "23514")
	}
	outcome, err = s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE public.config_documents SET last_revision=2 WHERE id=$1`, secondRef.Document().String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO public.config_versions
			(document_id,revision,parent_revision,source_hash,author,source,schema_version,syntax_profile,yaml_source)
			VALUES ($1,2,1,$2,'tester','fixture','ha-declarative-1','ha-2026.10/declarative-1',$3)`,
			secondRef.Document().String(), make([]byte, 32), []byte("wrong hash"))
		return err
	})
	if outcome != TransactionNotCommitted || err == nil {
		t.Fatalf("SQL source hash mismatch outcome=%v err=%v", outcome, err)
	}
	nha151SQLState(t, err, "23514")
	var hashConstraint *pgconn.PgError
	if !errors.As(err, &hashConstraint) || hashConstraint.ConstraintName != "config_version_source_hash_matches_bytes" {
		t.Fatalf("wanted source hash CHECK, got %v", err)
	}
	after, ok, err := s.ReadActiveConfig(ctx)
	if err != nil || !ok || after.Revision() != genA2 || after.Availability() != ConfigGenerationComplete {
		t.Fatalf("SQL probes damaged active: %v %t %v", after, ok, err)
	}
}

func TestNHA151TransactionalGCAndConcurrentReads(t *testing.T) {
	s, ctx := nha151Store(t)
	binding := nha151Binding(t, "home.config", "configuration.yaml")
	c, _ := nha151Candidate(t, binding.Path(), "active")
	_, activeGen := nha151Write(t, s, ctx, c, binding, ExpectNoConfigActive())
	for i := 2; i <= 20; i++ {
		c, _ := nha151Candidate(t, binding.Path(), fmt.Sprintf("v%d", i))
		nha151Write(t, s, ctx, c, binding, ExpectedConfigActive{})
	}
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION public.nha151_fail_gc() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected GC failure' USING ERRCODE='P0001'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `CREATE TRIGGER nha151_fail_gc BEFORE DELETE ON public.config_versions
		FOR EACH ROW EXECUTE FUNCTION public.nha151_fail_gc()`); err != nil {
		t.Fatal(err)
	}
	c21, _ := nha151Candidate(t, binding.Path(), "v21")
	outcome, err := s.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
		_, err := tx.SaveCandidate(c21, []ConfigDocumentBinding{binding}, nha151Metadata(t))
		return err
	})
	if outcome != TransactionNotCommitted || err == nil {
		t.Fatalf("injected collector rollback: %v %v", outcome, err)
	}
	nha151SQLState(t, err, "P0001")
	if _, err := s.pool.Exec(ctx, `DROP TRIGGER nha151_fail_gc ON public.config_versions`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DROP FUNCTION public.nha151_fail_gc()`); err != nil {
		t.Fatal(err)
	}
	var count, last int
	if err := s.pool.QueryRow(ctx, `SELECT count(*),max(revision) FROM public.config_versions WHERE document_id=$1`,
		binding.ID().String()).Scan(&count, &last); err != nil || count != 20 || last != 20 {
		t.Fatalf("collector rollback: count=%d last=%d err=%v", count, last, err)
	}
	var allocated int
	if err := s.pool.QueryRow(ctx, `SELECT last_revision FROM public.config_documents WHERE id=$1`,
		binding.ID().String()).Scan(&allocated); err != nil || allocated != 20 {
		t.Fatalf("revision rolled forward on rollback: %d %v", allocated, err)
	}
	var wg sync.WaitGroup
	writerErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 21; i <= 28; i++ {
			c, _ := nha151Candidate(t, binding.Path(), fmt.Sprintf("v%d", i))
			outcome, err := s.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
				_, err := tx.SaveCandidate(c, []ConfigDocumentBinding{binding}, nha151Metadata(t))
				return err
			})
			if outcome != TransactionCommitted || err != nil {
				writerErr <- fmt.Errorf("write %d: %v %v", i, outcome, err)
				return
			}
		}
	}()
	for i := 0; i < 40; i++ {
		g, ok, err := s.ReadActiveConfig(ctx)
		if err != nil || !ok || g.Revision() != activeGen || g.Availability() != ConfigGenerationComplete {
			t.Fatalf("concurrent active read: %v %t %v", g, ok, err)
		}
		if _, err := g.Restore(); err != nil {
			t.Fatalf("concurrent restore: %v", err)
		}
	}
	wg.Wait()
	select {
	case err := <-writerErr:
		t.Fatal(err)
	default:
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*),max(revision) FROM public.config_versions WHERE document_id=$1`,
		binding.ID().String()).Scan(&count, &last); err != nil || count != 20 || last != 28 {
		t.Fatalf("post-concurrency retention: count=%d last=%d err=%v", count, last, err)
	}
}

func TestNHA151CollectorUsesFinalActivePointer(t *testing.T) {
	s, ctx := nha151Store(t)
	binding := nha151Binding(t, "home.config", "configuration.yaml")
	c1, _ := nha151Candidate(t, binding.Path(), "v1")
	_, oldActive := nha151Write(t, s, ctx, c1, binding, ExpectNoConfigActive())
	for i := 2; i <= 20; i++ {
		c, _ := nha151Candidate(t, binding.Path(), fmt.Sprintf("v%d", i))
		nha151Write(t, s, ctx, c, binding, ExpectedConfigActive{})
	}
	c21, _ := nha151Candidate(t, binding.Path(), "v21")
	expected, _ := ExpectConfigActive(oldActive)
	ref, newActive := nha151Write(t, s, ctx, c21, binding, expected)
	if ref.Revision().Number() != 21 {
		t.Fatal("new active revision not allocated")
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM public.config_versions WHERE document_id=$1`,
		binding.ID().String()).Scan(&count); err != nil || count != 20 {
		t.Fatalf("retention after active switch: %d %v", count, err)
	}
	for revision, want := range map[int]bool{1: false, 2: true, 21: true} {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.config_versions WHERE document_id=$1 AND revision=$2)`,
			binding.ID().String(), revision).Scan(&exists); err != nil || exists != want {
			t.Fatalf("revision %d exists=%t want=%t err=%v", revision, exists, want, err)
		}
	}
	g, ok, err := s.ReadActiveConfig(ctx)
	if err != nil || !ok || g.Revision() != newActive || g.Availability() != ConfigGenerationComplete {
		t.Fatalf("final active after collection: %v %t %v", g, ok, err)
	}
	old, err := s.ReadConfigGeneration(ctx, oldActive)
	if err != nil || old.Availability() != ConfigGenerationIncomplete {
		t.Fatalf("retired active not diagnosed: %v %v", old.Availability(), err)
	}
}

func TestNHA151MultiSourceManifestAndPrevalidation(t *testing.T) {
	s, ctx := nha151Store(t)
	mainBinding := nha151Binding(t, "home.main", "configuration.yaml")
	partBinding := nha151Binding(t, "home.part", "parts/boolean.yaml")
	compile := func(label string) bundle.Candidate {
		t.Helper()
		main, err := parser.NewSourceFile(mainBinding.Path(), []byte("input_boolean: !include parts/boolean.yaml\n"))
		if err != nil {
			t.Fatal(err)
		}
		part, err := parser.NewSourceFile(partBinding.Path(), []byte(fmt.Sprintf("flag:\n  name: %q\n", label)))
		if err != nil {
			t.Fatal(err)
		}
		c, err := bundle.CompileSources(mainBinding.Path(), []parser.SourceFile{part, main})
		if err != nil || !c.Valid() {
			t.Fatalf("two-source candidate: %v", err)
		}
		return c
	}
	cA := compile("A")
	meta := nha151Metadata(t)
	outcome, err := s.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
		_, ignored := tx.SaveCandidate(cA, []ConfigDocumentBinding{mainBinding}, meta)
		if ignored == nil {
			return errors.New("incomplete binding accepted")
		}
		// Even an ignored method error must poison the transaction.
		return nil
	})
	if outcome != TransactionNotCommitted || err == nil {
		t.Fatalf("prevalidation rollback: %v %v", outcome, err)
	}
	var docs int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM public.config_documents`).Scan(&docs); err != nil || docs != 1 {
		t.Fatalf("prevalidation left a prefix: %d %v", docs, err)
	}
	var first []ConfigVersionRef
	var genA ConfigGenerationRevision
	outcome, err = s.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
		var err error
		first, err = tx.SaveCandidate(cA, []ConfigDocumentBinding{partBinding, mainBinding}, meta)
		if err != nil {
			return err
		}
		genA, err = tx.RegisterGeneration(cA, first)
		if err != nil {
			return err
		}
		return tx.ActivateGeneration(ExpectNoConfigActive(), genA)
	})
	if outcome != TransactionCommitted || err != nil {
		t.Fatalf("initial two-source generation: %v %v", outcome, err)
	}
	gA, ok, err := s.ReadActiveConfig(ctx)
	if err != nil || !ok || len(gA.Versions()) != 2 {
		t.Fatalf("two-source read: %v %t", err, ok)
	}
	if restored, err := gA.Restore(); err != nil || restored.SHA256() != cA.SHA256() {
		t.Fatalf("two-source restore: %v", err)
	}
	cB := compile("B")
	outcome, err = s.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
		refs, err := tx.SaveCandidate(cB, []ConfigDocumentBinding{mainBinding, partBinding}, meta)
		if err != nil {
			return err
		}
		_, err = tx.RegisterGeneration(cA, refs)
		return err
	})
	if outcome != TransactionNotCommitted || err == nil {
		t.Fatalf("cross-candidate manifest accepted: %v %v", outcome, err)
	}
	var genB ConfigGenerationRevision
	outcome, err = s.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
		refs, err := tx.SaveCandidate(cB, []ConfigDocumentBinding{mainBinding, partBinding}, meta)
		if err != nil {
			return err
		}
		genB, err = tx.RegisterGeneration(cB, refs)
		if err != nil {
			return err
		}
		expected, _ := ExpectConfigActive(genA)
		return tx.ActivateGeneration(expected, genB)
	})
	if outcome != TransactionCommitted || err != nil {
		t.Fatalf("second two-source generation: %v %v", outcome, err)
	}
	gB, ok, err := s.ReadActiveConfig(ctx)
	if err != nil || !ok || gB.Revision() != genB || gB.ManifestSHA256() == gA.ManifestSHA256() {
		t.Fatalf("two-source manifest mix: %v %t", err, ok)
	}
	if restored, err := gB.Restore(); err != nil || restored.SHA256() != cB.SHA256() {
		t.Fatalf("second two-source restore: %v", err)
	}
}

func TestNHA151RepeatableReadAcrossActivationAndGC(t *testing.T) {
	s, ctx := nha151Store(t)
	bindings := []ConfigDocumentBinding{
		nha151Binding(t, "home.main", "configuration.yaml"),
		nha151Binding(t, "home.part", "parts/boolean.yaml"),
	}
	meta := nha151Metadata(t)
	save := func(candidate bundle.Candidate, activate ExpectedConfigActive, register bool) ConfigGenerationRevision {
		t.Helper()
		var generation ConfigGenerationRevision
		outcome, err := s.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
			refs, err := tx.SaveCandidate(candidate, bindings, meta)
			if err != nil {
				return err
			}
			if register {
				generation, err = tx.RegisterGeneration(candidate, refs)
				if err != nil {
					return err
				}
				if activate.kind != configActiveExpectationUnknown {
					return tx.ActivateGeneration(activate, generation)
				}
			}
			return nil
		})
		if outcome != TransactionCommitted || err != nil {
			t.Fatalf("setup generation: %v %v", outcome, err)
		}
		return generation
	}
	cA := nha151TwoSource(t, "A")
	genA := save(cA, ExpectNoConfigActive(), true)
	for i := 2; i <= 20; i++ {
		save(nha151TwoSource(t, fmt.Sprintf("v%d", i)), ExpectedConfigActive{}, false)
	}
	cB := nha151TwoSource(t, "B")
	genB := save(cB, ExpectedConfigActive{}, true)
	cC := nha151TwoSource(t, "C")
	expectedA, _ := ExpectConfigActive(genA)
	locked := make(chan int, 1)
	proceed := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(proceed) }) })
	writerDone := make(chan error, 1)
	go func() {
		outcome, err := s.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
			if _, err := tx.tx.Exec(ctx, `LOCK TABLE public.config_generations IN ACCESS EXCLUSIVE MODE`); err != nil {
				return err
			}
			var pid int
			if err := tx.tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			locked <- pid
			<-proceed
			if err := tx.ActivateGeneration(expectedA, genB); err != nil {
				return err
			}
			_, err := tx.SaveCandidate(cC, bindings, meta)
			return err
		})
		if outcome != TransactionCommitted || err != nil {
			writerDone <- fmt.Errorf("writer outcome=%v err=%w", outcome, err)
			return
		}
		writerDone <- nil
	}()
	var writerPID int
	select {
	case writerPID = <-locked:
	case err := <-writerDone:
		t.Fatalf("writer did not lock table: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("writer lock timeout")
	}
	type readResult struct {
		g   ConfigGenerationSnapshot
		ok  bool
		err error
	}
	readerDone := make(chan readResult, 1)
	go func() {
		g, ok, err := s.ReadActiveConfig(ctx)
		readerDone <- readResult{g: g, ok: ok, err: err}
	}()
	blocked := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM pg_stat_activity a
			WHERE a.wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(a.pid))
		)`, writerPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case result := <-readerDone:
			t.Fatalf("reader finished before barrier: %v", result.err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("reader did not reach generation lock after pointer read")
	}
	release.Do(func() { close(proceed) })
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	result := <-readerDone
	if result.err != nil || !result.ok || result.g.Revision() != genA ||
		result.g.Availability() != ConfigGenerationComplete || len(result.g.Versions()) != 2 {
		t.Fatalf("repeatable-read snapshot mixed generations: %v %t %v", result.g, result.ok, result.err)
	}
	if restored, err := result.g.Restore(); err != nil || restored.SHA256() != cA.SHA256() {
		t.Fatalf("old snapshot lost after GC: %v", err)
	}
	fresh, ok, err := s.ReadActiveConfig(ctx)
	if err != nil || !ok || fresh.Revision() != genB || len(fresh.Versions()) != 2 {
		t.Fatalf("fresh active generation: %v %t %v", fresh, ok, err)
	}
	if restored, err := fresh.Restore(); err != nil || restored.SHA256() != cB.SHA256() {
		t.Fatalf("new snapshot is mixed: %v", err)
	}
	history, err := s.ReadConfigGeneration(ctx, genA)
	if err != nil || history.Availability() != ConfigGenerationIncomplete {
		t.Fatalf("post-GC old generation diagnosis: %v %v", history.Availability(), err)
	}
}

func TestNHA151DirectDeleteRacesActivation(t *testing.T) {
	s, ctx := nha151Store(t)
	binding := nha151Binding(t, "home.config", "configuration.yaml")
	cA, _ := nha151Candidate(t, binding.Path(), "A")
	_, genA := nha151Write(t, s, ctx, cA, binding, ExpectNoConfigActive())
	cB, _ := nha151Candidate(t, binding.Path(), "B")
	refB, genB := nha151Write(t, s, ctx, cB, binding, ExpectedConfigActive{})
	expectedA, _ := ExpectConfigActive(genA)
	locked := make(chan int, 1)
	proceed := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(proceed) }) })
	writerDone := make(chan error, 1)
	go func() {
		outcome, err := s.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
			var pid int
			if err := tx.tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			locked <- pid
			<-proceed
			return tx.ActivateGeneration(expectedA, genB)
		})
		if outcome != TransactionCommitted || err != nil {
			writerDone <- fmt.Errorf("activation outcome=%v err=%v", outcome, err)
			return
		}
		writerDone <- nil
	}()
	var writerPID int
	select {
	case writerPID = <-locked:
	case err := <-writerDone:
		t.Fatalf("writer lock failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("writer lock timeout")
	}
	deleterDone := make(chan error, 1)
	go func() {
		outcome, err := s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('homeward.config_gc','on',true)`); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `DELETE FROM public.config_versions
				WHERE document_id=$1 AND revision=$2`, refB.Document().String(), refB.Revision().Number())
			return err
		})
		if outcome != TransactionNotCommitted || err == nil {
			deleterDone <- fmt.Errorf("direct deletion outcome=%v err=%v", outcome, err)
			return
		}
		deleterDone <- err
	}()
	blocked := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM pg_stat_activity a
			WHERE a.wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(a.pid))
		)`, writerPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-deleterDone:
			t.Fatalf("deleter finished before advisory barrier: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("deleter did not block on shared advisory lock")
	}
	release.Do(func() { close(proceed) })
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	nha151SQLState(t, <-deleterDone, "23514")
	active, ok, err := s.ReadActiveConfig(ctx)
	if err != nil || !ok || active.Revision() != genB || active.Availability() != ConfigGenerationComplete {
		t.Fatalf("direct concurrent deletion damaged active: %v %t %v", active, ok, err)
	}
	if restored, err := active.Restore(); err != nil || restored.SHA256() != cB.SHA256() {
		t.Fatalf("active source deleted during activation: %v", err)
	}
}

func TestNHA151BoundariesAndCommitClassification(t *testing.T) {
	if _, err := NewConfigDocumentID(""); err == nil {
		t.Fatal("zero document ID admitted")
	}
	if _, err := NewConfigAuthor(""); err == nil {
		t.Fatal("zero author admitted")
	}
	if _, err := NewConfigSource(""); err == nil {
		t.Fatal("zero source admitted")
	}
	if _, err := NewConfigDocumentID("\xff"); err == nil {
		t.Fatal("invalid UTF-8 document ID admitted")
	}
	if _, err := NewConfigAuthor("\xff"); err == nil {
		t.Fatal("invalid UTF-8 author admitted")
	}
	if _, err := NewConfigSource("\xff"); err == nil {
		t.Fatal("invalid UTF-8 source admitted")
	}
	if _, err := NewConfigVersionRevision(0); err == nil {
		t.Fatal("zero version revision admitted")
	}
	if _, err := NewConfigGenerationRevision(0); err == nil {
		t.Fatal("zero generation revision admitted")
	}
	if _, err := NewConfigVersionRef(ConfigDocumentID{}, ConfigVersionRevision{}); err == nil {
		t.Fatal("zero version ref admitted")
	}
	if _, err := NewConfigDocumentBinding(ConfigDocumentID{}, "configuration.yaml"); err == nil {
		t.Fatal("zero binding admitted")
	}
	if _, err := NewConfigVersionMetadata(ConfigAuthor{}, ConfigSource{}); err == nil {
		t.Fatal("zero metadata admitted")
	}
	if _, err := ExpectConfigActive(ConfigGenerationRevision{}); err == nil {
		t.Fatal("zero active expectation admitted")
	}
	var typedNil *ConfigTx
	if _, err := typedNil.SaveCandidate(bundle.Candidate{}, nil, ConfigVersionMetadata{}); err == nil {
		t.Fatal("typed nil transaction admitted")
	}
	s, ctx := nha151Store(t)
	if _, ok, err := s.ReadActiveConfig(ctx); err != nil || ok {
		t.Fatalf("unset active pointer: %t %v", ok, err)
	}
	c, _ := nha151Candidate(t, "configuration.yaml", "A")
	binding := nha151Binding(t, "home.config", "configuration.yaml")
	var escaped *ConfigTx
	outcome, err := s.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
		escaped = tx
		_, err := tx.SaveCandidate(bundle.Candidate{}, []ConfigDocumentBinding{binding}, nha151Metadata(t))
		if err == nil {
			return errors.New("zero candidate admitted")
		}
		return nil
	})
	if outcome != TransactionNotCommitted || err == nil {
		t.Fatalf("boundary transaction: %v %v", outcome, err)
	}
	nha151Kind(t, err, ConfigInvalid)
	if _, err := escaped.SaveCandidate(c, []ConfigDocumentBinding{binding}, nha151Metadata(t)); err == nil {
		t.Fatal("escaped transaction reused")
	} else {
		nha151Kind(t, err, ConfigTransactionClosed)
	}
	outcome, err = failedCommit(context.Background(), errors.New("simulated lost commit acknowledgement"))
	if outcome != TransactionIndeterminate || err == nil {
		t.Fatal("unknown commit outcome turned into success or rollback")
	}
}

func TestNHA151SchemaEnumDrift(t *testing.T) {
	s, ctx := nha151Store(t)
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"config_document_kind", []string{ConfigDocumentKindYAMLSource.String(), ConfigDocumentKindAuthorizationPolicy.String(), ConfigDocumentKindMFAPolicy.String()}},
		{"config_schema_version", []string{bundle.SchemaDeclarative1.String(), authorization.Schema, bundle.SchemaDeclarative2.String(), authmfa.PolicySchema}},
		{"config_syntax_profile", []string{parser.SyntaxProfileDeclarative.String(), authorization.SyntaxProfile, parser.SyntaxProfileDeclarative2.String(), authmfa.PolicySyntaxProfile}},
	} {
		rows, err := s.pool.Query(ctx, `SELECT e.enumlabel FROM pg_enum e
			JOIN pg_type t ON t.oid=e.enumtypid
			WHERE t.typname=$1 ORDER BY e.enumsortorder`, tc.name)
		if err != nil {
			t.Fatal(err)
		}
		var labels []string
		for rows.Next() {
			var label string
			if err := rows.Scan(&label); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			labels = append(labels, label)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
		if len(labels) != len(tc.want) {
			t.Fatalf("SQL enum %s drifted from Go conversion: %v want %v", tc.name, labels, tc.want)
		}
		for i := range labels {
			if labels[i] != tc.want[i] {
				t.Fatalf("SQL enum %s drifted from Go conversion: %v want %v", tc.name, labels, tc.want)
			}
		}
	}
	if kind, err := parseConfigDocumentKind(ConfigDocumentKindYAMLSource.String()); err != nil || kind != ConfigDocumentKindYAMLSource {
		t.Fatalf("document kind conversion: %v %v", kind, err)
	}
	if _, err := parseConfigDocumentKind("future_kind"); err == nil {
		t.Fatal("future SQL enum silently admitted")
	}
}
