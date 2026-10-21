package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type nha029HistoricalVersion struct {
	ref        ConfigVersionRef
	generation ConfigGenerationRevision
	manifest   [sha256.Size]byte
	hash       [sha256.Size]byte
}

type nha029HistoricalFixture struct {
	store         *Store
	ctx           context.Context
	replay        *ConfigReplayRepository
	binding       ConfigDocumentBinding
	plain         []ConfigDocumentBinding
	plainSizes    []int
	sourceSize    int
	dataDir       string
	first, active nha029HistoricalVersion
}

// These are storage admission fixtures, not a claim that an arbitrary compiled
// digest restores a Logic1 program. The Run/Owner restoration belongs to C5's
// separate process tests. Exact source buffers are cleared after SQL writes.
func nha029HistoryFixture(t *testing.T, sourceSize int, plainSizes []int) *nha029HistoricalFixture {
	t.Helper()
	store, ctx := nha151Store(t)
	dataDir := filepath.Join(t.TempDir(), "private-data")
	secrets, err := store.BootstrapSecrets(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := NewConfigReplayRepository(store, secrets)
	if err != nil {
		t.Fatal(err)
	}
	f := &nha029HistoricalFixture{store: store, ctx: ctx, replay: replay,
		binding:    nha151Binding(t, "history.operational", "configuration.yaml"),
		sourceSize: sourceSize, plainSizes: plainSizes, dataDir: dataDir}
	for i := range plainSizes {
		f.plain = append(f.plain, nha151Binding(t, fmt.Sprintf("history.plain.%d", i), fmt.Sprintf("parts/source-%d.yaml", i)))
	}
	f.first = f.write(t, 'A')
	f.active = f.write(t, 'B')
	return f
}

func nha029HistoryRaw(size int, label byte) []byte {
	prefix := []byte(fmt.Sprintf("# HISTORY_SQL_CANARY_%c\n", label))
	if size < len(prefix)+2 {
		panic("invalid historical fixture source size")
	}
	raw := bytes.Repeat([]byte{'#'}, size)
	copy(raw, prefix)
	raw[len(prefix)] = '#'
	raw[len(raw)-1] = '\n'
	return raw
}

func (f *nha029HistoricalFixture) write(t *testing.T, label byte) nha029HistoricalVersion {
	t.Helper()
	raw := nha029HistoryRaw(f.sourceSize, label)
	defer clear(raw)
	result := nha029HistoricalVersion{hash: sha256.Sum256(raw)}
	expected := ExpectNoConfigActive()
	if f.active.generation.Valid() {
		var err error
		expected, err = ExpectConfigActive(f.active.generation, f.active.manifest)
		if err != nil {
			t.Fatal(err)
		}
	} else if f.first.generation.Valid() {
		var err error
		expected, err = ExpectConfigActive(f.first.generation, f.first.manifest)
		if err != nil {
			t.Fatal(err)
		}
	}
	attempt, err := RandomActivationAttemptID()
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := f.store.InConfigTransactionOutcome(f.ctx, func(tx *ConfigTx) error {
		var err error
		result.ref, err = tx.SaveProtectedOperationalSource(f.replay, f.binding, raw, result.hash, nha151Metadata(t))
		if err != nil {
			return err
		}
		refs := []ConfigVersionRef{result.ref}
		for i, binding := range f.plain {
			plain := nha029HistoryRaw(f.plainSizes[i], label)
			ref, err := tx.saveAdmittedSource(binding, configSourceDeclarative, plain, sha256.Sum256(plain), nha151Metadata(t))
			clear(plain)
			if err != nil {
				return err
			}
			refs = append(refs, ref)
		}
		result.generation, err = tx.RegisterCompiledGeneration(f.binding.Path(), result.hash, refs)
		if err != nil {
			return err
		}
		if err = tx.ActivateGeneration(expected, result.generation); err != nil {
			return err
		}
		return tx.RecordActivationAttempt(attempt, nha151Metadata(t))
	})
	if err != nil || outcome != TransactionCommitted {
		t.Fatalf("historical SQL fixture: outcome=%v err=%v", outcome, err)
	}
	generation, err := f.store.ReadConfigGeneration(f.ctx, result.generation)
	if err != nil {
		t.Fatal(err)
	}
	result.manifest = generation.ManifestSHA256()
	return result
}

func (f *nha029HistoricalFixture) fingerprint(t *testing.T) [sha256.Size]byte {
	t.Helper()
	var result []byte
	err := f.store.pool.QueryRow(f.ctx, `SELECT sha256(convert_to(jsonb_build_array(
		(SELECT jsonb_agg(to_jsonb(x) ORDER BY x.id) FROM public.config_documents x),
		(SELECT jsonb_agg(to_jsonb(x) ORDER BY x.document_id,x.revision) FROM public.config_versions x),
		(SELECT jsonb_agg(to_jsonb(x) ORDER BY x.revision) FROM public.config_generations x),
		(SELECT jsonb_agg(to_jsonb(x) ORDER BY x.generation_revision,x.document_id) FROM public.config_generation_entries x),
		(SELECT jsonb_agg(to_jsonb(x)) FROM public.config_active x),
		(SELECT jsonb_agg(to_jsonb(x) ORDER BY x.attempt_id) FROM public.config_activation_attempts x)
		)::text,'UTF8'))`).Scan(&result)
	if err != nil || len(result) != sha256.Size {
		t.Fatalf("historical fingerprint: %v", err)
	}
	var digest [sha256.Size]byte
	copy(digest[:], result)
	return digest
}

func nha029HistoryRefused(t *testing.T, source ConfigProtectedSource, err error, kind Kind) {
	t.Helper()
	nha151Kind(t, err, kind)
	if source.Ref().Valid() || source.Path() != "" || source.SHA256() != ([sha256.Size]byte{}) || source.Bytes() != nil {
		t.Fatal("historical refusal exposed a partial source")
	}
	for _, diagnostic := range []string{fmt.Sprint(err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
		if strings.Contains(diagnostic, "HISTORY_SQL_CANARY") {
			t.Fatal("historical error disclosed source")
		}
	}
	if errors.Unwrap(err) != nil {
		t.Fatal("historical error exposed an unredacted cause")
	}
}

func TestNHA029HistoricalProtectedSourceRetainedSQLAndActiveBoundary(t *testing.T) {
	f := nha029HistoryFixture(t, 96, []int{80})
	before := f.fingerprint(t)
	source, err := f.replay.ReadHistoricalProtectedSource(f.ctx, f.first.generation, f.first.manifest, f.first.ref)
	if err != nil || source.Ref() != f.first.ref || source.Path() != f.binding.Path() || source.SHA256() != f.first.hash ||
		sha256.Sum256(source.Bytes()) != f.first.hash || !bytes.Contains(source.Bytes(), []byte("HISTORY_SQL_CANARY_A")) {
		t.Fatalf("retained SQL source was not A: %v", err)
	}
	copyOfBytes := source.Bytes()
	copyOfBytes[0] ^= 1
	if sha256.Sum256(source.Bytes()) != f.first.hash {
		t.Fatal("historical bytes alias caller memory")
	}
	encoded, err := json.Marshal(source)
	if err != nil || bytes.Contains(encoded, []byte("HISTORY_SQL_CANARY")) || strings.Contains(fmt.Sprintf("%+v %#v", source, source), "HISTORY_SQL_CANARY") {
		t.Fatal("historical source diagnostics leaked plaintext")
	}
	_, err = f.replay.ReadActiveProtectedSource(f.ctx, f.first.generation, f.first.manifest, f.first.ref)
	nha151Kind(t, err, ConfigConflict)
	active, err := f.replay.ReadActiveProtectedSource(f.ctx, f.active.generation, f.active.manifest, f.active.ref)
	if err != nil || active.SHA256() != f.active.hash {
		t.Fatalf("active reader changed: %v", err)
	}
	generic, err := f.store.ReadConfigGeneration(f.ctx, f.first.generation)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range generic.Versions() {
		if v.Ref() == f.first.ref {
			if raw, ok := v.Bytes(); ok || raw != nil {
				t.Fatal("generic historical snapshot disclosed protected source")
			}
		}
	}
	_, err = generic.Restore()
	nha151Kind(t, err, ConfigProtected)
	if f.fingerprint(t) != before {
		t.Fatal("historical read mutated SQL")
	}
}

func TestNHA029HistoricalProtectedSourceAdmissionAndMembership(t *testing.T) {
	f := nha029HistoryFixture(t, 96, []int{64})
	before := f.fingerprint(t)
	type nilContext struct{ context.Context }
	var typedNil *nilContext
	for _, ctx := range []context.Context{nil, typedNil} {
		source, err := f.replay.ReadHistoricalProtectedSource(ctx, f.first.generation, f.first.manifest, f.first.ref)
		nha029HistoryRefused(t, source, err, ConfigInvalid)
	}
	cancelled, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := f.replay.ReadHistoricalProtectedSource(cancelled, f.first.generation, f.first.manifest, f.first.ref); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled history read: %v", err)
	}
	for _, repository := range []*ConfigReplayRepository{nil, {}, {store: f.store}, {store: &Store{}, secrets: f.replay.secrets}} {
		source, err := repository.ReadHistoricalProtectedSource(f.ctx, f.first.generation, f.first.manifest, f.first.ref)
		nha029HistoryRefused(t, source, err, ConfigInvalid)
	}
	for _, test := range []struct {
		generation ConfigGenerationRevision
		manifest   [sha256.Size]byte
		ref        ConfigVersionRef
		kind       Kind
	}{
		{ConfigGenerationRevision{}, f.first.manifest, f.first.ref, ConfigInvalid},
		{f.first.generation, [sha256.Size]byte{}, f.first.ref, ConfigInvalid},
		{f.first.generation, f.first.manifest, ConfigVersionRef{}, ConfigInvalid},
		{ConfigGenerationRevision{generation: 1 << 60}, f.first.manifest, f.first.ref, ConfigNotFound},
		{f.first.generation, sha256.Sum256([]byte("wrong manifest")), f.first.ref, ConfigConflict},
		{f.first.generation, f.first.manifest, f.active.ref, ConfigNotFound},
		{f.first.generation, f.first.manifest, ConfigVersionRef{document: f.plain[0].id, revision: f.first.ref.revision}, ConfigNotFound},
		{f.first.generation, f.first.manifest, ConfigVersionRef{document: ConfigDocumentID{document: "foreign.document"}, revision: f.first.ref.revision}, ConfigNotFound},
		{f.first.generation, f.first.manifest, ConfigVersionRef{document: f.first.ref.document, revision: ConfigVersionRevision{version: 1 << 60}}, ConfigNotFound},
	} {
		source, err := f.replay.ReadHistoricalProtectedSource(f.ctx, test.generation, test.manifest, test.ref)
		nha029HistoryRefused(t, source, err, test.kind)
	}
	key, err := newMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	wrongKey := &ConfigReplayRepository{store: f.store, secrets: &SecretRepository{store: f.store, key: key}}
	source, err := wrongKey.ReadHistoricalProtectedSource(f.ctx, f.first.generation, f.first.manifest, f.first.ref)
	nha029HistoryRefused(t, source, err, ConfigIntegrity)
	if f.fingerprint(t) != before {
		t.Fatal("refused historical read mutated SQL")
	}
	for _, raw := range []string{"", "future_source"} {
		if _, err := parseConfigSourceRepresentation(raw); err == nil {
			t.Fatal("unknown source representation admitted")
		}
		if _, err := parseConfigManifestFormat(raw); err == nil {
			t.Fatal("unknown manifest format admitted")
		}
	}
}

// Only these existing tables/triggers can be corrupted in a disposable fixture.
// The normal write is first proved forbidden; no product migration is changed.
type nha029HistoryTable uint8

const (
	nha029HistoryVersion nha029HistoryTable = iota + 1
	nha029HistoryDocument
	nha029HistoryGeneration
	nha029HistoryEntry
)

func (f *nha029HistoricalFixture) corrupt(t *testing.T, target nha029HistoryTable, constraints []string, query string, args ...any) {
	t.Helper()
	var table, trigger string
	switch target {
	case nha029HistoryVersion:
		table, trigger = "config_versions", "config_version_immutable"
	case nha029HistoryDocument:
		table, trigger = "config_documents", "config_document_identity"
	case nha029HistoryGeneration:
		table, trigger = "config_generations", "config_generation_immutable"
	case nha029HistoryEntry:
		table, trigger = "config_generation_entries", "config_entry_immutable"
	default:
		t.Fatal("invalid corruption target")
	}
	if _, err := f.store.pool.Exec(f.ctx, query, args...); err == nil {
		t.Fatal("ordinary immutable SQL write accepted")
	} else {
		nha151SQLState(t, err, "23514")
	}
	tx, err := f.store.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	tableSQL := pgx.Identifier{"public", table}.Sanitize()
	triggerSQL := pgx.Identifier{trigger}.Sanitize()
	if _, err := tx.Exec(f.ctx, "ALTER TABLE "+tableSQL+" DISABLE TRIGGER "+triggerSQL); err != nil {
		t.Fatal(err)
	}
	for _, constraint := range constraints {
		if !((target == nha029HistoryVersion && (constraint == "config_version_source_hash_matches_bytes" || constraint == "config_versions_yaml_source_check")) ||
			(target == nha029HistoryGeneration && constraint == "config_generations_entry_count_check")) {
			t.Fatal("invalid fixture constraint")
		}
		if _, err := tx.Exec(f.ctx, "ALTER TABLE "+tableSQL+" DROP CONSTRAINT "+pgx.Identifier{constraint}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(f.ctx, query, args...); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(f.ctx, "ALTER TABLE "+tableSQL+" ENABLE TRIGGER "+triggerSQL); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	var enabled string
	if err := f.store.pool.QueryRow(f.ctx, `SELECT tgenabled::text FROM pg_trigger WHERE tgrelid=$1::regclass AND tgname=$2`, "public."+table, trigger).Scan(&enabled); err != nil || enabled != "O" {
		t.Fatalf("fixture guard not restored: %v", err)
	}
}

func TestNHA029HistoricalProtectedSourceRejectsTamperedSQL(t *testing.T) {
	for _, test := range []struct {
		name   string
		target nha029HistoryTable
		query  string
	}{
		{"manifest", nha029HistoryGeneration, `UPDATE public.config_generations SET manifest_hash=set_byte(manifest_hash,0,get_byte(manifest_hash,0)#1) WHERE revision=$1`},
		{"unsealed", nha029HistoryGeneration, `UPDATE public.config_generations SET sealed=false WHERE revision=$1`},
		{"member_hash", nha029HistoryEntry, `UPDATE public.config_generation_entries SET source_hash=set_byte(source_hash,0,get_byte(source_hash,0)#1) WHERE generation_revision=$1`},
		{"member_version", nha029HistoryEntry, `UPDATE public.config_generation_entries SET version_revision=2 WHERE generation_revision=$1`},
		{"ciphertext", nha029HistoryVersion, `UPDATE public.config_versions SET ciphertext=set_byte(ciphertext,13,get_byte(ciphertext,13)#1) WHERE revision=$1 AND document_id='history.operational'`},
		{"foreign_version_ciphertext", nha029HistoryVersion, `UPDATE public.config_versions SET ciphertext=(SELECT ciphertext FROM public.config_versions WHERE document_id='history.operational' AND revision=2) WHERE revision=$1 AND document_id='history.operational'`},
		{"AAD_salt", nha029HistoryVersion, `WITH replacement AS (INSERT INTO public.secret_salts(salt) VALUES(decode(repeat('ab',32),'hex')) RETURNING salt) UPDATE public.config_versions SET salt=(SELECT salt FROM replacement) WHERE revision=$1 AND document_id='history.operational'`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := nha029HistoryFixture(t, 96, nil)
			id := f.first.generation.Number()
			if test.target == nha029HistoryVersion {
				id = f.first.ref.Revision().Number()
			}
			f.corrupt(t, test.target, nil, test.query, id)
			before := f.fingerprint(t)
			source, err := f.replay.ReadHistoricalProtectedSource(f.ctx, f.first.generation, f.first.manifest, f.first.ref)
			nha029HistoryRefused(t, source, err, ConfigIntegrity)
			if f.fingerprint(t) != before {
				t.Fatal("integrity refusal mutated SQL")
			}
		})
	}
	t.Run("AAD_path_with_recomputed_manifest", func(t *testing.T) {
		f := nha029HistoryFixture(t, 96, nil)
		f.corrupt(t, nha029HistoryDocument, nil, `UPDATE public.config_documents SET logical_path='different.yaml' WHERE id='history.operational'`)
		// Keep the manifest structurally valid so the old ciphertext must fail
		// its original AAD instead of merely failing the manifest hash first.
		manifest := configCompiledManifestHash(f.binding.Path(), f.first.hash, []configManifestItem{{ref: f.first.ref, path: "different.yaml", hash: f.first.hash}})
		f.corrupt(t, nha029HistoryGeneration, nil, `UPDATE public.config_generations SET manifest_hash=$1 WHERE revision=$2`, manifest[:], f.first.generation.Number())
		before := f.fingerprint(t)
		source, err := f.replay.ReadHistoricalProtectedSource(f.ctx, f.first.generation, manifest, f.first.ref)
		nha029HistoryRefused(t, source, err, ConfigIntegrity)
		if f.fingerprint(t) != before {
			t.Fatal("AAD refusal mutated SQL")
		}
	})
}

// Count actual PostgreSQL response octets on a distinct owned pool. This does
// not mock SQL, COMMIT, the repository, or memory accounting. Rejection after
// scanning a hostile >1 Mio bytea would exceed the wire bound below.
type nha029HistoricalCountedConn struct {
	net.Conn
	received *atomic.Int64
}

func (c *nha029HistoricalCountedConn) Read(buffer []byte) (int, error) {
	n, err := c.Conn.Read(buffer)
	c.received.Add(int64(n))
	return n, err
}

func (f *nha029HistoricalFixture) measuredRead(t *testing.T) (ConfigProtectedSource, error, int64) {
	t.Helper()
	config := f.store.pool.Config()
	config.MaxConns, config.MinConns = 1, 0
	dial := config.ConnConfig.DialFunc
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	var received atomic.Int64
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &nha029HistoricalCountedConn{Conn: conn, received: &received}, nil
	}
	pool, err := pgxpool.NewWithConfig(f.ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{pool: pool}
	t.Cleanup(store.Close)
	secrets, err := store.BootstrapSecrets(f.ctx, f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := NewConfigReplayRepository(store, secrets)
	if err != nil {
		t.Fatal(err)
	}
	before := received.Load()
	source, err := replay.ReadHistoricalProtectedSource(f.ctx, f.first.generation, f.first.manifest, f.first.ref)
	return source, err, received.Load() - before
}

func TestNHA029HistoricalProtectedSourceBoundsBeforePayload(t *testing.T) {
	t.Run("exact_source_and_total", func(t *testing.T) {
		f := nha029HistoryFixture(t, 1<<20, []int{1 << 20, 1 << 20, 1 << 19, 1 << 19})
		source, err := f.replay.ReadHistoricalProtectedSource(f.ctx, f.first.generation, f.first.manifest, f.first.ref)
		if err != nil || len(source.Bytes()) != 1<<20 || source.SHA256() != f.first.hash {
			t.Fatalf("exact historical bounds rejected: %v", err)
		}
	})
	t.Run("exact_member_count", func(t *testing.T) {
		sizes := make([]int, configSourceLimitCount-1)
		for i := range sizes {
			sizes[i] = 64
		}
		f := nha029HistoryFixture(t, 96, sizes)
		if _, err := f.replay.ReadHistoricalProtectedSource(f.ctx, f.first.generation, f.first.manifest, f.first.ref); err != nil {
			t.Fatalf("256 members rejected: %v", err)
		}
	})
	for _, test := range []struct {
		name        string
		target      nha029HistoryTable
		constraints []string
		query       string
	}{
		{"257_count", nha029HistoryGeneration, []string{"config_generations_entry_count_check"}, `UPDATE public.config_generations SET entry_count=257 WHERE revision=$1`},
		{"ciphertext_short", nha029HistoryVersion, []string{"config_version_source_hash_matches_bytes"}, `UPDATE public.config_versions SET ciphertext=decode(repeat('00',28),'hex') WHERE revision=$1 AND document_id='history.operational'`},
		{"ciphertext_long", nha029HistoryVersion, []string{"config_version_source_hash_matches_bytes"}, `UPDATE public.config_versions SET ciphertext=decode(repeat('00',1048605),'hex') WHERE revision=$1 AND document_id='history.operational'`},
		{"salt_length", nha029HistoryVersion, []string{"config_version_source_hash_matches_bytes"}, `UPDATE public.config_versions SET salt=NULL WHERE revision=$1 AND document_id='history.operational'`},
		{"key_length", nha029HistoryVersion, []string{"config_version_source_hash_matches_bytes"}, `UPDATE public.config_versions SET key_id=NULL WHERE revision=$1 AND document_id='history.operational'`},
		{"plaintext_long", nha029HistoryVersion, []string{"config_version_source_hash_matches_bytes", "config_versions_yaml_source_check"}, `UPDATE public.config_versions SET yaml_source=decode(repeat('00',1048577),'hex') WHERE revision=$1 AND document_id='history.plain.0'`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := nha029HistoryFixture(t, 96, []int{64})
			id := f.first.generation.Number()
			if test.target == nha029HistoryVersion {
				id = f.first.ref.Revision().Number()
			}
			f.corrupt(t, test.target, test.constraints, test.query, id)
			before := f.fingerprint(t)
			source, err, received := f.measuredRead(t)
			nha029HistoryRefused(t, source, err, ConfigIntegrity)
			if received <= 0 || received >= 1<<18 {
				t.Fatalf("bounds refusal fetched payload: received=%d", received)
			}
			if f.fingerprint(t) != before {
				t.Fatal("bounds refusal mutated SQL")
			}
		})
	}
	t.Run("aggregate_before_plaintext_hash", func(t *testing.T) {
		f := nha029HistoryFixture(t, 1<<20, []int{1 << 20, 1 << 20, 1 << 19, 1 << 19})
		// Each document still fits 1 Mio; the extra byte only breaks 4 Mio.
		f.corrupt(t, nha029HistoryVersion, []string{"config_version_source_hash_matches_bytes"}, `UPDATE public.config_versions SET yaml_source=yaml_source||decode('23','hex') WHERE document_id='history.plain.3' AND revision=$1`, f.first.ref.Revision().Number())
		before := f.fingerprint(t)
		source, err, received := f.measuredRead(t)
		nha029HistoryRefused(t, source, err, ConfigIntegrity)
		if received <= 0 || received >= 1<<18 {
			t.Fatalf("aggregate refusal fetched payload: received=%d", received)
		}
		if f.fingerprint(t) != before {
			t.Fatal("aggregate refusal mutated SQL")
		}
	})
}

func TestNHA029HistoricalProtectedSourceCollectedVersionFailsClosed(t *testing.T) {
	f := nha029HistoryFixture(t, 96, nil)
	// Establish a real RR/read-only snapshot before normal GC. Both private
	// helpers must observe this same historical image after the writer leaves.
	tx, err := f.store.pool.BeginTx(f.ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	generation, err := readConfigGenerationTx(f.ctx, tx, f.first.generation)
	if err != nil || generation.Availability() != ConfigGenerationComplete {
		t.Fatalf("historical RR admission: %v", err)
	}
	for i := 3; i <= 21; i++ {
		f.active = f.write(t, byte('A'+i))
	}
	var retained bool
	if err := f.store.pool.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM public.config_versions WHERE document_id=$1 AND revision=$2)`, f.first.ref.Document().String(), f.first.ref.Revision().Number()).Scan(&retained); err != nil || retained {
		t.Fatalf("normal GC did not remove historical version: retained=%t err=%v", retained, err)
	}
	old, err := f.replay.readProtectedSourceTx(f.ctx, tx, f.first.generation, f.first.ref)
	if err != nil || old.SHA256() != f.first.hash {
		t.Fatalf("RR spliced GC into admitted source: %v", err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	before := f.fingerprint(t)
	source, err := f.replay.ReadHistoricalProtectedSource(f.ctx, f.first.generation, f.first.manifest, f.first.ref)
	nha029HistoryRefused(t, source, err, ConfigIncomplete)
	if f.fingerprint(t) != before {
		t.Fatal("collected history refusal mutated SQL")
	}
	active, err := f.replay.ReadActiveProtectedSource(f.ctx, f.active.generation, f.active.manifest, f.active.ref)
	if err != nil || active.SHA256() != f.active.hash {
		t.Fatalf("GC damaged current source: %v", err)
	}
}
