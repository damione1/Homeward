package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	configVersionLimit     = 20
	configSourceLimitBytes = 4 * 1024 * 1024
	configSourceLimitCount = 256
)

// The SQL enum in migration v4 is authoritative; this conversion is checked
// against pg_enum by TestNHA151SchemaEnumDrift.
type ConfigDocumentKind uint8

const (
	ConfigDocumentKindInvalid ConfigDocumentKind = iota
	ConfigDocumentKindYAMLSource
	ConfigDocumentKindAuthorizationPolicy
)

func (k ConfigDocumentKind) String() string {
	switch k {
	case ConfigDocumentKindYAMLSource:
		return "yaml_source"
	case ConfigDocumentKindAuthorizationPolicy:
		return "authorization_policy"
	default:
		return ""
	}
}

func parseConfigDocumentKind(text string) (ConfigDocumentKind, error) {
	switch text {
	case ConfigDocumentKindYAMLSource.String():
		return ConfigDocumentKindYAMLSource, nil
	case ConfigDocumentKindAuthorizationPolicy.String():
		return ConfigDocumentKindAuthorizationPolicy, nil
	default:
		return ConfigDocumentKindInvalid, &Error{kind: ConfigIntegrity}
	}
}

// These IDs are open sets. Their distinct private fields prevent an implicit
// conversion between equally represented document, version and generation IDs.
type ConfigDocumentID struct{ document string }
type ConfigVersionRevision struct{ version int64 }
type ConfigGenerationRevision struct{ generation int64 }
type ConfigAuthor struct{ author string }
type ConfigSource struct{ source string }

func NewConfigDocumentID(text string) (ConfigDocumentID, error) {
	if !configLabel(text, 255) {
		return ConfigDocumentID{}, &Error{kind: ConfigInvalid}
	}
	return ConfigDocumentID{document: text}, nil
}
func NewConfigAuthor(text string) (ConfigAuthor, error) {
	if !configLabel(text, 255) {
		return ConfigAuthor{}, &Error{kind: ConfigInvalid}
	}
	return ConfigAuthor{author: text}, nil
}
func NewConfigSource(text string) (ConfigSource, error) {
	if !configLabel(text, 255) {
		return ConfigSource{}, &Error{kind: ConfigInvalid}
	}
	return ConfigSource{source: text}, nil
}
func NewConfigVersionRevision(number int64) (ConfigVersionRevision, error) {
	if number <= 0 {
		return ConfigVersionRevision{}, &Error{kind: ConfigInvalid}
	}
	return ConfigVersionRevision{version: number}, nil
}
func NewConfigGenerationRevision(number int64) (ConfigGenerationRevision, error) {
	if number <= 0 {
		return ConfigGenerationRevision{}, &Error{kind: ConfigInvalid}
	}
	return ConfigGenerationRevision{generation: number}, nil
}
func configLabel(text string, limit int) bool {
	return strings.TrimSpace(text) != "" && len(text) <= limit && utf8.ValidString(text) &&
		!strings.ContainsAny(text, "\x00\r\n")
}
func (id ConfigDocumentID) Valid() bool          { return configLabel(id.document, 255) }
func (id ConfigDocumentID) String() string       { return id.document }
func (r ConfigVersionRevision) Valid() bool      { return r.version > 0 }
func (r ConfigVersionRevision) Number() int64    { return r.version }
func (r ConfigGenerationRevision) Valid() bool   { return r.generation > 0 }
func (r ConfigGenerationRevision) Number() int64 { return r.generation }
func (a ConfigAuthor) Valid() bool               { return configLabel(a.author, 255) }
func (s ConfigSource) Valid() bool               { return configLabel(s.source, 255) }
func (a ConfigAuthor) Value() (string, bool)     { return a.author, a.Valid() }
func (s ConfigSource) Value() (string, bool)     { return s.source, s.Valid() }

// ConfigDocumentBinding gives a source path a stable, caller-owned identity.
// A candidate must supply exactly one binding for each admitted source.
type ConfigDocumentBinding struct {
	id   ConfigDocumentID
	path string
}

func NewConfigDocumentBinding(id ConfigDocumentID, path string) (ConfigDocumentBinding, error) {
	if !id.Valid() {
		return ConfigDocumentBinding{}, &Error{kind: ConfigInvalid}
	}
	if _, err := parser.NewSourceFile(path, nil); err != nil {
		return ConfigDocumentBinding{}, &Error{kind: ConfigInvalid, cause: err}
	}
	return ConfigDocumentBinding{id: id, path: path}, nil
}
func (b ConfigDocumentBinding) ID() ConfigDocumentID { return b.id }
func (b ConfigDocumentBinding) Path() string         { return b.path }

type ConfigVersionMetadata struct {
	author ConfigAuthor
	source ConfigSource
}

func NewConfigVersionMetadata(author ConfigAuthor, source ConfigSource) (ConfigVersionMetadata, error) {
	if !author.Valid() || !source.Valid() {
		return ConfigVersionMetadata{}, &Error{kind: ConfigInvalid}
	}
	return ConfigVersionMetadata{author: author, source: source}, nil
}

// ConfigVersionRef is an exact document revision, independent of its content
// hash. A purged revision is never reused.
type ConfigVersionRef struct {
	document ConfigDocumentID
	revision ConfigVersionRevision
}

func NewConfigVersionRef(document ConfigDocumentID, revision ConfigVersionRevision) (ConfigVersionRef, error) {
	if !document.Valid() || !revision.Valid() {
		return ConfigVersionRef{}, &Error{kind: ConfigInvalid}
	}
	return ConfigVersionRef{document: document, revision: revision}, nil
}

func (r ConfigVersionRef) Document() ConfigDocumentID      { return r.document }
func (r ConfigVersionRef) Revision() ConfigVersionRevision { return r.revision }
func (r ConfigVersionRef) Valid() bool                     { return r.document.Valid() && r.revision.Valid() }

// ConfigTx is owned by one callback and one goroutine. It has no Commit or
// Rollback method; a retained handle fails after the callback returns.
type ConfigTx struct {
	ctx     context.Context
	tx      pgx.Tx
	live    bool
	failure error
	touched map[string]ConfigDocumentID
}

func (c *ConfigTx) valid() error {
	if c == nil || !c.live || c.tx == nil || c.ctx == nil {
		return &Error{kind: ConfigTransactionClosed}
	}
	if c.failure != nil {
		return c.failure
	}
	return nil
}

func (c *ConfigTx) remember(err error) {
	if c != nil && err != nil && c.failure == nil {
		c.failure = err
	}
}

// InConfigTransactionOutcome serializes all repository writes with SQL-side
// active/GC guards. The caller must reconcile an indeterminate commit before
// trying another write; the callback must not run methods concurrently.
func (s *Store) InConfigTransactionOutcome(ctx context.Context, fn func(*ConfigTx) error) (TransactionOutcome, error) {
	if fn == nil {
		return TransactionNotCommitted, &Error{kind: ConfigInvalid}
	}
	return s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		return withConfigTx(ctx, tx, fn)
	})
}

// withConfigTx never opens another transaction. Account creation and policy
// replacement acquire this lock before auth_bootstrap in their same pgx.Tx.
func withConfigTx(ctx context.Context, tx pgx.Tx, fn func(*ConfigTx) error) error {
	if fn == nil {
		return &Error{kind: ConfigInvalid}
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(481316, 151)`); err != nil {
		return err
	}
	c := &ConfigTx{ctx: ctx, tx: tx, live: true}
	defer func() { c.live = false }()
	if err := fn(c); err != nil {
		return err
	}
	if c.failure != nil {
		return c.failure
	}
	ids := make([]string, 0, len(c.touched))
	for id := range c.touched {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := c.prune(c.touched[id]); err != nil {
			return err
		}
	}
	return nil
}

// SaveCandidate persists only the exact bytes held by an admitted Candidate.
// Its binding set must match the candidate sources by canonical path.
func (c *ConfigTx) SaveCandidate(candidate bundle.Candidate, bindings []ConfigDocumentBinding, metadata ConfigVersionMetadata) (refs []ConfigVersionRef, err error) {
	defer func() { c.remember(err) }()
	if err := c.valid(); err != nil {
		return nil, err
	}
	if !candidate.Valid() || !metadata.author.Valid() || !metadata.source.Valid() {
		return nil, &Error{kind: ConfigInvalid}
	}
	sources := candidate.Sources()
	if len(sources) == 0 || len(sources) != len(bindings) {
		return nil, &Error{kind: ConfigInvalid}
	}
	byPath := make(map[string]ConfigDocumentBinding, len(bindings))
	byID := make(map[string]struct{}, len(bindings))
	for _, b := range bindings {
		if !b.id.Valid() {
			return nil, &Error{kind: ConfigInvalid}
		}
		if _, err := parser.NewSourceFile(b.path, nil); err != nil {
			return nil, &Error{kind: ConfigInvalid, cause: err}
		}
		if _, duplicate := byPath[b.path]; duplicate {
			return nil, &Error{kind: ConfigInvalid}
		}
		if _, duplicate := byID[b.id.document]; duplicate {
			return nil, &Error{kind: ConfigInvalid}
		}
		byPath[b.path] = b
		byID[b.id.document] = struct{}{}
	}
	for _, source := range sources {
		if _, ok := byPath[source.Path()]; !ok {
			return nil, &Error{kind: ConfigInvalid}
		}
	}
	refs = make([]ConfigVersionRef, 0, len(sources))
	format := configSourceDeclarative
	if candidate.Schema() == bundle.SchemaDeclarative2 {
		format = configSourceDeclarative2
	}
	for _, source := range sources {
		b, ok := byPath[source.Path()]
		if !ok {
			return nil, &Error{kind: ConfigInvalid}
		}
		ref, saveErr := c.saveAdmittedSource(b, format, source.Bytes(), source.SHA256(), metadata)
		if saveErr != nil {
			return nil, saveErr
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

type configSourceFormat uint8

const (
	configSourceInvalid configSourceFormat = iota
	configSourceDeclarative
	configSourceAuthorization
	configSourceDeclarative2
)

func (f configSourceFormat) values() (ConfigDocumentKind, string, string, error) {
	switch f {
	case configSourceDeclarative:
		return ConfigDocumentKindYAMLSource, bundle.SchemaDeclarative1.String(), parser.SyntaxProfileDeclarative.String(), nil
	case configSourceDeclarative2:
		return ConfigDocumentKindYAMLSource, bundle.SchemaDeclarative2.String(), parser.SyntaxProfileDeclarative2.String(), nil
	case configSourceAuthorization:
		return ConfigDocumentKindAuthorizationPolicy, authorization.Schema, authorization.SyntaxProfile, nil
	case configSourceInvalid:
		return ConfigDocumentKindInvalid, "", "", &Error{kind: ConfigInvalid}
	default:
		return ConfigDocumentKindInvalid, "", "", &Error{kind: ConfigInvalid}
	}
}

// saveAdmittedSource is shared by closed-schema admission wrappers only. It
// does not accept an arbitrary schema/profile pair or masquerade as Candidate.
func (c *ConfigTx) saveAdmittedSource(binding ConfigDocumentBinding, format configSourceFormat, data []byte, sourceHash [sha256.Size]byte, metadata ConfigVersionMetadata) (ConfigVersionRef, error) {
	if err := c.valid(); err != nil {
		return ConfigVersionRef{}, err
	}
	kind, schema, profile, err := format.values()
	if err != nil || !binding.id.Valid() || !metadata.author.Valid() || !metadata.source.Valid() || len(data) > 1<<20 || sha256.Sum256(data) != sourceHash {
		return ConfigVersionRef{}, &Error{kind: ConfigInvalid}
	}
	if _, err := parser.NewSourceFile(binding.path, nil); err != nil {
		return ConfigVersionRef{}, &Error{kind: ConfigInvalid}
	}
	if _, err := c.tx.Exec(c.ctx, `INSERT INTO public.config_documents(id,logical_path,kind)
		VALUES($1,$2,$3::public.config_document_kind) ON CONFLICT(id) DO NOTHING`, binding.id.document, binding.path, kind.String()); err != nil {
		return ConfigVersionRef{}, configWriteError(err)
	}
	var path, storedKind string
	if err := c.tx.QueryRow(c.ctx, `SELECT logical_path,kind::text FROM public.config_documents WHERE id=$1`, binding.id.document).Scan(&path, &storedKind); err != nil {
		return ConfigVersionRef{}, err
	}
	if path != binding.path || storedKind != kind.String() {
		return ConfigVersionRef{}, &Error{kind: ConfigConflict}
	}
	var revision int64
	if err := c.tx.QueryRow(c.ctx, `UPDATE public.config_documents SET last_revision=last_revision+1 WHERE id=$1 RETURNING last_revision`, binding.id.document).Scan(&revision); err != nil {
		return ConfigVersionRef{}, err
	}
	var parent any
	if revision > 1 {
		parent = revision - 1
	}
	if _, err := c.tx.Exec(c.ctx, `INSERT INTO public.config_versions
		(document_id,revision,parent_revision,source_hash,author,source,schema_version,syntax_profile,yaml_source)
		VALUES($1,$2,$3,$4,$5,$6,$7::public.config_schema_version,$8::public.config_syntax_profile,$9)`, binding.id.document, revision, parent, sourceHash[:], metadata.author.author, metadata.source.source, schema, profile, data); err != nil {
		return ConfigVersionRef{}, configWriteError(err)
	}
	if c.touched == nil {
		c.touched = make(map[string]ConfigDocumentID)
	}
	c.touched[binding.id.document] = binding.id
	return ConfigVersionRef{document: binding.id, revision: ConfigVersionRevision{version: revision}}, nil
}

func configWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "23505" || pgErr.Code == "23514" || pgErr.Code == "23503") {
		return &Error{kind: ConfigConflict}
	}
	return err
}

func (c *ConfigTx) prune(id ConfigDocumentID) error {
	if _, err := c.tx.Exec(c.ctx, `SELECT set_config('homeward.config_gc','on',true)`); err != nil {
		return err
	}
	_, err := c.tx.Exec(c.ctx, `WITH ranked AS (
		SELECT v.document_id, v.revision,
		       row_number() OVER (ORDER BY
		         CASE WHEN EXISTS (
		           SELECT 1 FROM public.config_active a
		           JOIN public.config_generation_entries e ON e.generation_revision=a.generation_revision
		           WHERE a.singleton AND e.document_id=v.document_id AND e.version_revision=v.revision
		         ) OR EXISTS (
		           SELECT 1 FROM public.authorization_policy_active p
		           WHERE p.document_id=v.document_id AND p.version_revision=v.revision
		         ) THEN 0 ELSE 1 END, v.revision DESC) AS keep_rank
		FROM public.config_versions v WHERE v.document_id=$1
	)
	DELETE FROM public.config_versions v USING ranked r
	WHERE v.document_id=r.document_id AND v.revision=r.revision AND r.keep_rank>$2`, id.document, configVersionLimit)
	return err
}

type configManifestItem struct {
	ref  ConfigVersionRef
	path string
	hash [sha256.Size]byte
}

func writeHashField(h hash.Hash, data []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(data)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(data)
}

func configManifestHash(entry string, items []configManifestItem) [sha256.Size]byte {
	ordered := append([]configManifestItem(nil), items...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ref.document.document < ordered[j].ref.document.document })
	h := sha256.New()
	writeHashField(h, []byte("homeward/config-manifest/v1"))
	writeHashField(h, []byte(entry))
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], uint64(len(ordered)))
	_, _ = h.Write(number[:])
	for _, item := range ordered {
		writeHashField(h, []byte(item.ref.document.document))
		writeHashField(h, []byte(item.path))
		binary.BigEndian.PutUint64(number[:], uint64(item.ref.revision.version))
		_, _ = h.Write(number[:])
		_, _ = h.Write(item.hash[:])
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

// RegisterGeneration binds every admitted source to its exact persisted
// document revision. The manifest hash is separate from the candidate hash.
func (c *ConfigTx) RegisterGeneration(candidate bundle.Candidate, refs []ConfigVersionRef) (result ConfigGenerationRevision, err error) {
	defer func() { c.remember(err) }()
	if err := c.valid(); err != nil {
		return ConfigGenerationRevision{}, err
	}
	if !candidate.Valid() || len(refs) != len(candidate.Sources()) || len(refs) == 0 {
		return ConfigGenerationRevision{}, &Error{kind: ConfigInvalid}
	}
	byPath := make(map[string]configManifestItem, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if !ref.Valid() {
			return ConfigGenerationRevision{}, &Error{kind: ConfigInvalid}
		}
		if _, duplicate := seen[ref.document.document]; duplicate {
			return ConfigGenerationRevision{}, &Error{kind: ConfigInvalid}
		}
		seen[ref.document.document] = struct{}{}
		var item configManifestItem
		var digest, data []byte
		var kind, schema, profile string
		item.ref = ref
		err = c.tx.QueryRow(c.ctx, `SELECT d.logical_path,d.kind::text,v.source_hash,v.yaml_source,v.schema_version::text,v.syntax_profile::text
			FROM public.config_versions v JOIN public.config_documents d ON d.id=v.document_id
			WHERE v.document_id=$1 AND v.revision=$2`, ref.document.document, ref.revision.version).
			Scan(&item.path, &kind, &digest, &data, &schema, &profile)
		if errors.Is(err, pgx.ErrNoRows) {
			return ConfigGenerationRevision{}, &Error{kind: ConfigIncomplete}
		}
		if err != nil {
			return ConfigGenerationRevision{}, err
		}
		parsedKind, err := parseConfigDocumentKind(kind)
		if err != nil {
			return ConfigGenerationRevision{}, err
		}
		if parsedKind != ConfigDocumentKindYAMLSource || len(digest) != sha256.Size ||
			schema != candidate.Schema().String() || profile != candidate.Profile().String() {
			return ConfigGenerationRevision{}, &Error{kind: ConfigIntegrity}
		}
		copy(item.hash[:], digest)
		if sha256.Sum256(data) != item.hash {
			return ConfigGenerationRevision{}, &Error{kind: ConfigIntegrity}
		}
		if _, duplicate := byPath[item.path]; duplicate {
			return ConfigGenerationRevision{}, &Error{kind: ConfigInvalid}
		}
		byPath[item.path] = item
	}
	items := make([]configManifestItem, 0, len(refs))
	for _, source := range candidate.Sources() {
		item, ok := byPath[source.Path()]
		if !ok || item.hash != source.SHA256() {
			return ConfigGenerationRevision{}, &Error{kind: ConfigInvalid}
		}
		items = append(items, item)
	}
	manifestHash := configManifestHash(candidate.Entry(), items)
	candidateHash := candidate.SHA256()
	var revision int64
	err = c.tx.QueryRow(c.ctx, `INSERT INTO public.config_generations(entry_path,candidate_hash,manifest_hash,entry_count)
		VALUES ($1,$2,$3,$4) RETURNING revision`, candidate.Entry(), candidateHash[:], manifestHash[:], len(items)).Scan(&revision)
	if err != nil {
		return ConfigGenerationRevision{}, err
	}
	for _, item := range items {
		if _, err := c.tx.Exec(c.ctx, `INSERT INTO public.config_generation_entries
			(generation_revision,document_id,version_revision,source_hash) VALUES ($1,$2,$3,$4)`,
			revision, item.ref.document.document, item.ref.revision.version, item.hash[:]); err != nil {
			return ConfigGenerationRevision{}, err
		}
	}
	if _, err := c.tx.Exec(c.ctx, `UPDATE public.config_generations SET sealed=true WHERE revision=$1`, revision); err != nil {
		return ConfigGenerationRevision{}, err
	}
	return ConfigGenerationRevision{generation: revision}, nil
}

// ExpectedConfigActive makes absence distinct from an unknown or Go-zero
// precondition. Activation is a CAS and revisions never move backward.
type configActiveExpectationKind uint8

const (
	configActiveExpectationUnknown configActiveExpectationKind = iota
	configActiveExpectationAbsent
	configActiveExpectationRevision
)

type ExpectedConfigActive struct {
	kind     configActiveExpectationKind
	revision ConfigGenerationRevision
}

func ExpectNoConfigActive() ExpectedConfigActive {
	return ExpectedConfigActive{kind: configActiveExpectationAbsent}
}
func ExpectConfigActive(revision ConfigGenerationRevision) (ExpectedConfigActive, error) {
	if !revision.Valid() {
		return ExpectedConfigActive{}, &Error{kind: ConfigInvalid}
	}
	return ExpectedConfigActive{kind: configActiveExpectationRevision, revision: revision}, nil
}

func (c *ConfigTx) ActivateGeneration(expected ExpectedConfigActive, target ConfigGenerationRevision) (err error) {
	defer func() { c.remember(err) }()
	if err := c.valid(); err != nil {
		return err
	}
	if !target.Valid() ||
		(expected.kind == configActiveExpectationAbsent && expected.revision.Valid()) ||
		(expected.kind != configActiveExpectationAbsent &&
			(expected.kind != configActiveExpectationRevision || !expected.revision.Valid())) {
		return &Error{kind: ConfigInvalid}
	}
	var current *int64
	if err := c.tx.QueryRow(c.ctx, `SELECT generation_revision FROM public.config_active WHERE singleton FOR UPDATE`).Scan(&current); err != nil {
		return err
	}
	if (expected.kind == configActiveExpectationAbsent && current != nil) ||
		(expected.kind == configActiveExpectationRevision &&
			(current == nil || *current != expected.revision.generation)) {
		return &Error{kind: ConfigConflict}
	}
	if current != nil && target.generation <= *current {
		return &Error{kind: ConfigConflict}
	}
	tag, err := c.tx.Exec(c.ctx, `UPDATE public.config_active SET generation_revision=$1,activated_at=clock_timestamp()
		WHERE singleton`, target.generation)
	if err != nil {
		return configWriteError(err)
	}
	if tag.RowsAffected() != 1 {
		return &Error{kind: ConfigIntegrity}
	}
	return nil
}

// ConfigVersionSnapshot returns exact bytes by copy. Missing historical
// versions carry their identity and hash, but no bytes.
type ConfigVersionSnapshot struct {
	ref       ConfigVersionRef
	path      string
	hash      [sha256.Size]byte
	yaml      []byte
	present   bool
	parent    ConfigVersionRevision
	hasParent bool
	author    ConfigAuthor
	source    ConfigSource
	kind      ConfigDocumentKind
	schema    bundle.Schema
	profile   parser.SyntaxProfile
	accepted  time.Time
}

func (v ConfigVersionSnapshot) Ref() ConfigVersionRef     { return v.ref }
func (v ConfigVersionSnapshot) Path() string              { return v.path }
func (v ConfigVersionSnapshot) SHA256() [sha256.Size]byte { return v.hash }
func (v ConfigVersionSnapshot) Present() bool             { return v.present }
func (v ConfigVersionSnapshot) Bytes() ([]byte, bool) {
	if !v.present {
		return nil, false
	}
	return bytes.Clone(v.yaml), true
}
func (v ConfigVersionSnapshot) Parent() (ConfigVersionRevision, bool) { return v.parent, v.hasParent }
func (v ConfigVersionSnapshot) Author() (ConfigAuthor, bool)          { return v.author, v.present }
func (v ConfigVersionSnapshot) Source() (ConfigSource, bool)          { return v.source, v.present }
func (v ConfigVersionSnapshot) Kind() (ConfigDocumentKind, bool)      { return v.kind, v.present }
func (v ConfigVersionSnapshot) Schema() (bundle.Schema, bool)         { return v.schema, v.present }
func (v ConfigVersionSnapshot) Profile() (parser.SyntaxProfile, bool) { return v.profile, v.present }
func (v ConfigVersionSnapshot) AcceptedAt() (time.Time, bool)         { return v.accepted, v.present }

type ConfigGenerationAvailability uint8

const (
	ConfigGenerationUnknown ConfigGenerationAvailability = iota
	ConfigGenerationComplete
	ConfigGenerationIncomplete
)

type ConfigGenerationSnapshot struct {
	revision      ConfigGenerationRevision
	entry         string
	candidateHash [sha256.Size]byte
	manifestHash  [sha256.Size]byte
	created       time.Time
	versions      []ConfigVersionSnapshot
	availability  ConfigGenerationAvailability
}

func (g ConfigGenerationSnapshot) Revision() ConfigGenerationRevision         { return g.revision }
func (g ConfigGenerationSnapshot) Entry() string                              { return g.entry }
func (g ConfigGenerationSnapshot) CandidateSHA256() [sha256.Size]byte         { return g.candidateHash }
func (g ConfigGenerationSnapshot) ManifestSHA256() [sha256.Size]byte          { return g.manifestHash }
func (g ConfigGenerationSnapshot) CreatedAt() time.Time                       { return g.created }
func (g ConfigGenerationSnapshot) Availability() ConfigGenerationAvailability { return g.availability }
func (g ConfigGenerationSnapshot) Versions() []ConfigVersionSnapshot {
	return append([]ConfigVersionSnapshot(nil), g.versions...)
}

// Restore recompiles exact stored sources only at this restoration boundary.
// A missing historical source never becomes an empty candidate.
func (g ConfigGenerationSnapshot) Restore() (bundle.Candidate, error) {
	if !g.revision.Valid() || g.availability == ConfigGenerationUnknown {
		return bundle.Candidate{}, &Error{kind: ConfigInvalid}
	}
	if g.availability == ConfigGenerationIncomplete {
		return bundle.Candidate{}, &Error{kind: ConfigIncomplete}
	}
	sources := make([]parser.SourceFile, 0, len(g.versions))
	for _, v := range g.versions {
		if !v.present {
			return bundle.Candidate{}, &Error{kind: ConfigIncomplete}
		}
		source, err := parser.NewSourceFile(v.path, v.yaml)
		if err != nil || source.SHA256() != v.hash {
			return bundle.Candidate{}, &Error{kind: ConfigIntegrity, cause: err}
		}
		sources = append(sources, source)
	}
	if len(g.versions) == 0 {
		return bundle.Candidate{}, &Error{kind: ConfigIntegrity}
	}
	profile := g.versions[0].profile
	for _, v := range g.versions {
		if v.profile != profile {
			return bundle.Candidate{}, &Error{kind: ConfigIntegrity}
		}
	}
	var candidate bundle.Candidate
	var err error
	switch profile {
	case parser.SyntaxProfileDeclarative:
		candidate, err = bundle.CompileSources(g.entry, sources)
	case parser.SyntaxProfileDeclarative2:
		candidate, err = bundle.CompileSourcesV2(g.entry, sources)
	default:
		return bundle.Candidate{}, &Error{kind: ConfigIntegrity}
	}
	if err != nil || !candidate.Valid() || candidate.SHA256() != g.candidateHash {
		return bundle.Candidate{}, &Error{kind: ConfigIntegrity, cause: err}
	}
	return candidate, nil
}

func (s *Store) ReadConfigGeneration(ctx context.Context, revision ConfigGenerationRevision) (ConfigGenerationSnapshot, error) {
	if !revision.Valid() {
		return ConfigGenerationSnapshot{}, &Error{kind: ConfigInvalid}
	}
	return s.readConfig(ctx, &revision)
}

func (s *Store) ReadActiveConfig(ctx context.Context) (ConfigGenerationSnapshot, bool, error) {
	g, err := s.readConfig(ctx, nil)
	if err != nil {
		return ConfigGenerationSnapshot{}, false, err
	}
	if !g.revision.Valid() {
		return ConfigGenerationSnapshot{}, false, nil
	}
	if g.availability != ConfigGenerationComplete {
		return ConfigGenerationSnapshot{}, false, &Error{kind: ConfigIntegrity}
	}
	return g, true, nil
}

// readConfig uses one REPEATABLE READ snapshot for pointer, manifest and bytes.
// A concurrent collector cannot splice two generations into one response.
func (s *Store) readConfig(ctx context.Context, target *ConfigGenerationRevision) (ConfigGenerationSnapshot, error) {
	if ctx == nil {
		return ConfigGenerationSnapshot{}, &Error{kind: InvalidContext}
	}
	if s == nil || s.pool == nil {
		return ConfigGenerationSnapshot{}, &Error{kind: InvalidSettings}
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ConfigGenerationSnapshot{}, &Error{kind: Unavailable, cause: err}
	}
	defer rollback(tx)
	var revision int64
	if target == nil {
		var active *int64
		if err := tx.QueryRow(ctx, `SELECT generation_revision FROM public.config_active WHERE singleton`).Scan(&active); err != nil {
			return ConfigGenerationSnapshot{}, &Error{kind: ConfigIntegrity, cause: err}
		}
		if active == nil {
			return ConfigGenerationSnapshot{}, nil
		}
		revision = *active
	} else {
		revision = target.generation
	}
	g := ConfigGenerationSnapshot{revision: ConfigGenerationRevision{generation: revision}}
	var candidateHash, manifestHash []byte
	var count int
	var sealed bool
	err = tx.QueryRow(ctx, `SELECT entry_path,candidate_hash,manifest_hash,entry_count,sealed,created_at
		FROM public.config_generations WHERE revision=$1`, revision).
		Scan(&g.entry, &candidateHash, &manifestHash, &count, &sealed, &g.created)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConfigGenerationSnapshot{}, &Error{kind: ConfigNotFound}
	}
	if err != nil {
		return ConfigGenerationSnapshot{}, &Error{kind: Unavailable, cause: err}
	}
	if !sealed || count < 1 || count > configSourceLimitCount ||
		len(candidateHash) != sha256.Size || len(manifestHash) != sha256.Size {
		return ConfigGenerationSnapshot{}, &Error{kind: ConfigIntegrity}
	}
	if _, err := parser.NewSourceFile(g.entry, nil); err != nil {
		return ConfigGenerationSnapshot{}, &Error{kind: ConfigIntegrity, cause: err}
	}
	copy(g.candidateHash[:], candidateHash)
	copy(g.manifestHash[:], manifestHash)
	rows, err := tx.Query(ctx, `SELECT e.document_id,d.logical_path,d.kind::text,e.version_revision,e.source_hash,
		v.yaml_source,v.parent_revision,v.author,v.source,v.accepted_at,v.schema_version::text,v.syntax_profile::text
		FROM public.config_generation_entries e
		JOIN public.config_documents d ON d.id=e.document_id
		LEFT JOIN public.config_versions v ON v.document_id=e.document_id AND v.revision=e.version_revision
		WHERE e.generation_revision=$1 ORDER BY e.document_id LIMIT $2`, revision, count+1)
	if err != nil {
		return ConfigGenerationSnapshot{}, &Error{kind: Unavailable, cause: err}
	}
	items := make([]configManifestItem, 0, count)
	totalBytes := 0
	for rows.Next() {
		var v ConfigVersionSnapshot
		var documentText string
		var revisionNumber int64
		var hashBytes []byte
		var kind string
		var data []byte
		var parent *int64
		var author, source, schema, profile *string
		var accepted *time.Time
		err := rows.Scan(&documentText, &v.path, &kind, &revisionNumber, &hashBytes,
			&data, &parent, &author, &source, &accepted, &schema, &profile)
		if err != nil {
			rows.Close()
			return ConfigGenerationSnapshot{}, &Error{kind: ConfigIntegrity, cause: err}
		}
		if len(hashBytes) != sha256.Size {
			rows.Close()
			return ConfigGenerationSnapshot{}, &Error{kind: ConfigIntegrity}
		}
		if len(g.versions) >= count {
			rows.Close()
			return ConfigGenerationSnapshot{}, &Error{kind: ConfigIntegrity}
		}
		v.ref.document, err = NewConfigDocumentID(documentText)
		if err != nil {
			rows.Close()
			return ConfigGenerationSnapshot{}, &Error{kind: ConfigIntegrity, cause: err}
		}
		v.ref.revision, err = NewConfigVersionRevision(revisionNumber)
		if err != nil {
			rows.Close()
			return ConfigGenerationSnapshot{}, &Error{kind: ConfigIntegrity, cause: err}
		}
		if _, err := parser.NewSourceFile(v.path, nil); err != nil {
			rows.Close()
			return ConfigGenerationSnapshot{}, &Error{kind: ConfigIntegrity, cause: err}
		}
		copy(v.hash[:], hashBytes)
		v.kind, err = parseConfigDocumentKind(kind)
		if err != nil {
			rows.Close()
			return ConfigGenerationSnapshot{}, err
		}
		if schema != nil {
			totalBytes += len(data)
			if totalBytes > configSourceLimitBytes {
				rows.Close()
				return ConfigGenerationSnapshot{}, &Error{kind: ConfigIntegrity}
			}
			if profile == nil || !((*schema == bundle.SchemaDeclarative1.String() && *profile == parser.SyntaxProfileDeclarative.String()) ||
				(*schema == bundle.SchemaDeclarative2.String() && *profile == parser.SyntaxProfileDeclarative2.String())) ||
				author == nil || source == nil || accepted == nil || sha256.Sum256(data) != v.hash {
				rows.Close()
				return ConfigGenerationSnapshot{}, &Error{kind: ConfigIntegrity}
			}
			v.present = true
			v.yaml = bytes.Clone(data)
			v.author, err = NewConfigAuthor(*author)
			if err != nil {
				rows.Close()
				return ConfigGenerationSnapshot{}, &Error{kind: ConfigIntegrity, cause: err}
			}
			v.source, err = NewConfigSource(*source)
			if err != nil {
				rows.Close()
				return ConfigGenerationSnapshot{}, &Error{kind: ConfigIntegrity, cause: err}
			}
			if *schema == bundle.SchemaDeclarative2.String() {
				v.schema = bundle.SchemaDeclarative2
				v.profile = parser.SyntaxProfileDeclarative2
			} else {
				v.schema = bundle.SchemaDeclarative1
				v.profile = parser.SyntaxProfileDeclarative
			}
			v.accepted = *accepted
			if parent != nil {
				v.parent, err = NewConfigVersionRevision(*parent)
				if err != nil || v.parent.version >= revisionNumber {
					rows.Close()
					return ConfigGenerationSnapshot{}, &Error{kind: ConfigIntegrity, cause: err}
				}
				v.hasParent = true
			}
		} else {
			g.availability = ConfigGenerationIncomplete
		}
		g.versions = append(g.versions, v)
		items = append(items, configManifestItem{ref: v.ref, path: v.path, hash: v.hash})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return ConfigGenerationSnapshot{}, &Error{kind: Unavailable, cause: err}
	}
	rows.Close()
	if len(g.versions) != count || configManifestHash(g.entry, items) != g.manifestHash {
		return ConfigGenerationSnapshot{}, &Error{kind: ConfigIntegrity}
	}
	if g.availability == ConfigGenerationUnknown {
		g.availability = ConfigGenerationComplete
	}
	if err := tx.Commit(ctx); err != nil {
		return ConfigGenerationSnapshot{}, &Error{kind: Unavailable, cause: err}
	}
	return g, nil
}
