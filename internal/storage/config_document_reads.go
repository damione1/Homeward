package storage

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"time"

	"github.com/jackc/pgx/v5"
)

const configPublicPageLimit = 100

// ConfigDocumentMetadata never contains source bytes, author text, credentials,
// launch locators, comments, executable paths or worker arguments. Schema/profile
// values come from the SQL enums and are admitted as a pair below.
type ConfigDocumentMetadata struct {
	ref              ConfigVersionRef
	kind             ConfigDocumentKind
	schema, profile  string
	hash             [sha256.Size]byte
	updated          time.Time
	retained, active bool
}

func (d ConfigDocumentMetadata) Ref() ConfigVersionRef           { return d.ref }
func (d ConfigDocumentMetadata) Kind() ConfigDocumentKind        { return d.kind }
func (d ConfigDocumentMetadata) SchemaVersion() string           { return d.schema }
func (d ConfigDocumentMetadata) SyntaxProfile() string           { return d.profile }
func (d ConfigDocumentMetadata) SourceSHA256() [sha256.Size]byte { return d.hash }
func (d ConfigDocumentMetadata) UpdatedAt() (time.Time, bool)    { return d.updated, d.retained }
func (d ConfigDocumentMetadata) Retained() bool                  { return d.retained }
func (d ConfigDocumentMetadata) Active() bool                    { return d.active }

// ConfigDocumentCut binds page continuations to a metadata cut, including
// retained-row counts. Purging a version invalidates the continuation explicitly.
type ConfigDocumentCut struct{ digest [sha256.Size]byte }

func (c ConfigDocumentCut) SHA256() [sha256.Size]byte { return c.digest }
func (c ConfigDocumentCut) Valid() bool               { return c.digest != ([sha256.Size]byte{}) }

type ConfigDocumentPage struct {
	documents []ConfigDocumentMetadata
	cut       ConfigDocumentCut
	more      bool
}

func (p ConfigDocumentPage) Documents() []ConfigDocumentMetadata {
	return append([]ConfigDocumentMetadata(nil), p.documents...)
}
func (p ConfigDocumentPage) Cut() ConfigDocumentCut { return p.cut }
func (p ConfigDocumentPage) More() bool             { return p.more }

func configPublicSchema(kind ConfigDocumentKind, schema, profile string) bool {
	entry, ok := configPublicSchemaMetadata(schema)
	return ok && entry.kind == kind && entry.profile == profile || editableSourceFormat(kind.String(), schema, profile)
}
func configPublicSchemaMetadata(schema string) (ConfigSchemaMetadata, bool) {
	entry := ConfigSchemaMetadata{schema: schema}
	switch schema {
	case "ha-declarative-1":
		entry.kind, entry.profile = ConfigDocumentKindYAMLSource, "ha-2026.10/declarative-1"
	case "ha-declarative-2":
		entry.kind, entry.profile = ConfigDocumentKindYAMLSource, "ha-2026.10/declarative-2"
	case "homeward.authorization/1":
		entry.kind, entry.profile = ConfigDocumentKindAuthorizationPolicy, "homeward.authorization-yaml/1"
	case "homeward.mfa-policy/1":
		entry.kind, entry.profile = ConfigDocumentKindMFAPolicy, "homeward.mfa-policy-yaml/1"
	case configOperationalSchema:
		entry.kind, entry.profile = ConfigDocumentKindOperationalYAML, configOperationalProfile
	case configBootstrapSchema:
		entry.kind, entry.profile = ConfigDocumentKindBootstrapLaunch, configBootstrapProfile
	case "homeward-dashboard-1":
		entry.kind, entry.profile = ConfigDocumentKindDashboardSource, "ha-2026.10/lovelace-1"
	case "ha-logic-1":
		entry.kind, entry.profile = ConfigDocumentKindAutomationSource, "ha-2026.10/logic-1"
	case "ha-scene-1":
		entry.kind, entry.profile = ConfigDocumentKindSceneSource, "ha-2026.10/scene-1"
	case "homeward.heating-ownership/1":
		entry.kind, entry.profile = ConfigDocumentKindOwnershipSource, "homeward.heating-ownership-json/1"
	default:
		return ConfigSchemaMetadata{}, false
	}
	return entry, true
}

func publicConfigRead(ctx context.Context, store *Store) (context.Context, context.CancelFunc, pgx.Tx, error) {
	if err := bootstrapReadContextError(ctx); err != nil {
		return nil, nil, nil, err
	}
	if store == nil || store.pool == nil {
		return nil, nil, nil, &Error{kind: ConfigInvalid}
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	tx, err := store.pool.BeginTx(bounded, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		cancel()
		return nil, nil, nil, &Error{kind: Unavailable}
	}
	return bounded, cancel, tx, nil
}

func writePublicConfigInt(h hash.Hash, n int64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(n))
	_, _ = h.Write(b[:])
}

// Metadata cut costs <=16384 small fixed/bounded rows. Source bytea, ciphertext,
// and open historical text are never selected. Keyset pages are coherent within
// one read-only transaction and reject a changed cut instead of blending pages.
func configDocumentCutTx(ctx context.Context, tx pgx.Tx) (ConfigDocumentCut, error) {
	rows, err := tx.Query(ctx, `SELECT CASE WHEN octet_length(d.id) BETWEEN 1 AND 255 THEN d.id END,
		d.last_revision,count(v.revision),coalesce(min(v.revision),0),coalesce(max(v.revision),0),
		coalesce((SELECT b.version_revision FROM public.config_bootstrap_active b WHERE b.document_id=d.id),0),
		coalesce((SELECT p.version_revision FROM public.authorization_policy_active p WHERE p.document_id=d.id),0),
		coalesce((SELECT p.version_revision FROM public.auth_mfa_policy p WHERE p.document_id=d.id),0)
		FROM public.config_documents d LEFT JOIN public.config_versions v ON v.document_id=d.id
		GROUP BY d.id ORDER BY d.id LIMIT 16385`)
	if err != nil {
		return ConfigDocumentCut{}, &Error{kind: Unavailable}
	}
	defer rows.Close()
	h := sha256.New()
	writeHashField(h, []byte("homeward/config-document-cut/1"))
	n := 0
	for rows.Next() {
		var id *string
		var last, retained, first, latest, bootstrap, policy, mfa int64
		if err := rows.Scan(&id, &last, &retained, &first, &latest, &bootstrap, &policy, &mfa); err != nil {
			return ConfigDocumentCut{}, &Error{kind: Unavailable}
		}
		n++
		if id == nil || !configLabel(*id, 255) || last < 1 || retained < 0 || bootstrap < 0 || policy < 0 || mfa < 0 || n > 16384 {
			return ConfigDocumentCut{}, &Error{kind: ConfigIntegrity}
		}
		writeHashField(h, []byte(*id))
		for _, value := range []int64{last, retained, first, latest, bootstrap, policy, mfa} {
			writePublicConfigInt(h, value)
		}
	}
	if rows.Err() != nil {
		return ConfigDocumentCut{}, &Error{kind: Unavailable}
	}
	rows.Close()
	var active *int64
	var manifest []byte
	if err := tx.QueryRow(ctx, `SELECT a.generation_revision,g.manifest_hash FROM public.config_active a
		LEFT JOIN public.config_generations g ON g.revision=a.generation_revision WHERE a.singleton`).Scan(&active, &manifest); err != nil {
		return ConfigDocumentCut{}, &Error{kind: Unavailable}
	}
	if active != nil {
		if *active < 1 || len(manifest) != sha256.Size {
			return ConfigDocumentCut{}, &Error{kind: ConfigIntegrity}
		}
		writePublicConfigInt(h, *active)
		writeHashField(h, manifest)
	}
	var generationCount, generationHigh, generationMembers int64
	if err := tx.QueryRow(ctx, `SELECT count(*),coalesce(max(revision),0),coalesce(sum(entry_count),0)::bigint
		FROM public.config_generations WHERE sealed`).Scan(&generationCount, &generationHigh, &generationMembers); err != nil {
		return ConfigDocumentCut{}, &Error{kind: Unavailable}
	}
	if generationCount < 0 || generationHigh < 0 || generationMembers < 0 {
		return ConfigDocumentCut{}, &Error{kind: ConfigIntegrity}
	}
	for _, value := range []int64{generationCount, generationHigh, generationMembers} {
		writePublicConfigInt(h, value)
	}
	var cut ConfigDocumentCut
	copy(cut.digest[:], h.Sum(nil))
	return cut, nil
}

func scanConfigDocument(rows pgx.Rows) (ConfigDocumentMetadata, error) {
	var d ConfigDocumentMetadata
	var id, schema, profile, kind *string
	var revision int64
	var digest []byte
	var updated *time.Time
	if err := rows.Scan(&id, &revision, &kind, &schema, &profile, &digest, &updated, &d.active); err != nil {
		return d, &Error{kind: Unavailable}
	}
	if id == nil || kind == nil || revision < 1 || len(digest) != sha256.Size {
		return d, &Error{kind: ConfigIntegrity}
	}
	var err error
	d.ref.document, err = NewConfigDocumentID(*id)
	if err != nil {
		return d, &Error{kind: ConfigIntegrity}
	}
	d.ref.revision = ConfigVersionRevision{version: revision}
	d.kind, err = parseConfigDocumentKind(*kind)
	if err != nil {
		return d, err
	}
	copy(d.hash[:], digest)
	if schema == nil || profile == nil || updated == nil {
		if schema != nil || profile != nil || updated != nil {
			return d, &Error{kind: ConfigIntegrity}
		}
		return d, nil
	}
	if !configPublicSchema(d.kind, *schema, *profile) || updated.IsZero() {
		return d, &Error{kind: ConfigIntegrity}
	}
	d.schema, d.profile, d.updated, d.retained = *schema, *profile, *updated, true
	return d, nil
}

// ReadConfigDocumentPage returns latest document metadata. A zero expected cut
// starts a page sequence; every continuation must present its exact prior cut.
func (s *Store) ReadConfigDocumentPage(ctx context.Context, after string, size int, expected [sha256.Size]byte) (ConfigDocumentPage, error) {
	if size < 1 || size > configPublicPageLimit || after != "" && !configLabel(after, 255) {
		return ConfigDocumentPage{}, &Error{kind: ConfigInvalid}
	}
	ctx, cancel, tx, err := publicConfigRead(ctx, s)
	if err != nil {
		return ConfigDocumentPage{}, err
	}
	defer cancel()
	defer rollback(tx)
	cut, err := configDocumentCutTx(ctx, tx)
	if err != nil {
		return ConfigDocumentPage{}, err
	}
	if expected != ([sha256.Size]byte{}) && expected != cut.digest {
		return ConfigDocumentPage{}, &Error{kind: ConfigConflict}
	}
	rows, err := tx.Query(ctx, `SELECT CASE WHEN octet_length(d.id) BETWEEN 1 AND 255 THEN d.id END,
		d.last_revision,d.kind::text,v.schema_version::text,v.syntax_profile::text,
		CASE WHEN octet_length(v.source_hash)=32 THEN v.source_hash END,v.accepted_at,
		EXISTS(SELECT 1 FROM public.config_active a JOIN public.config_generation_entries e ON e.generation_revision=a.generation_revision
		 WHERE e.document_id=d.id AND e.version_revision=d.last_revision)
		OR EXISTS(SELECT 1 FROM public.config_bootstrap_active b WHERE b.document_id=d.id AND b.version_revision=d.last_revision)
		OR EXISTS(SELECT 1 FROM public.authorization_policy_active p WHERE p.document_id=d.id AND p.version_revision=d.last_revision)
		OR EXISTS(SELECT 1 FROM public.auth_mfa_policy p WHERE p.document_id=d.id AND p.version_revision=d.last_revision)
		FROM public.config_documents d JOIN public.config_versions v ON v.document_id=d.id AND v.revision=d.last_revision
		WHERE d.id>$1 ORDER BY d.id LIMIT $2`, after, size+1)
	if err != nil {
		return ConfigDocumentPage{}, &Error{kind: Unavailable}
	}
	defer rows.Close()
	page := ConfigDocumentPage{cut: cut}
	for rows.Next() {
		d, err := scanConfigDocument(rows)
		if err != nil {
			return ConfigDocumentPage{}, err
		}
		if len(page.documents) == size {
			page.more = true
			break
		}
		page.documents = append(page.documents, d)
	}
	if rows.Err() != nil {
		return ConfigDocumentPage{}, &Error{kind: Unavailable}
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return ConfigDocumentPage{}, &Error{kind: Unavailable}
	}
	return page, nil
}

// ConfigDocumentState is one metadata-only SQL cut, including all independent
// active pointers and the generation authority. It holds no source payload.
type ConfigDocumentState struct {
	document, operational ConfigDocumentMetadata
	generation            ConfigGenerationRevision
	manifest              [32]byte
	cut                   ConfigDocumentCut
}

func (s ConfigDocumentState) Document() ConfigDocumentMetadata     { return s.document }
func (s ConfigDocumentState) Operational() ConfigDocumentMetadata  { return s.operational }
func (s ConfigDocumentState) Generation() ConfigGenerationRevision { return s.generation }
func (s ConfigDocumentState) ManifestSHA256() [32]byte             { return s.manifest }
func (s ConfigDocumentState) Cut() ConfigDocumentCut               { return s.cut }

func readConfigDocumentMetadataTx(ctx context.Context, tx pgx.Tx, id ConfigDocumentID) (ConfigDocumentMetadata, error) {
	rows, err := tx.Query(ctx, `SELECT d.id,d.last_revision,d.kind::text,v.schema_version::text,v.syntax_profile::text,
		CASE WHEN octet_length(v.source_hash)=32 THEN v.source_hash END,v.accepted_at,
		EXISTS(SELECT 1 FROM public.config_active a JOIN public.config_generation_entries e ON e.generation_revision=a.generation_revision
		WHERE e.document_id=d.id AND e.version_revision=d.last_revision)
		OR EXISTS(SELECT 1 FROM public.config_bootstrap_active b WHERE b.document_id=d.id AND b.version_revision=d.last_revision)
		OR EXISTS(SELECT 1 FROM public.authorization_policy_active p WHERE p.document_id=d.id AND p.version_revision=d.last_revision)
		OR EXISTS(SELECT 1 FROM public.auth_mfa_policy p WHERE p.document_id=d.id AND p.version_revision=d.last_revision)
		FROM public.config_documents d JOIN public.config_versions v ON v.document_id=d.id AND v.revision=d.last_revision WHERE d.id=$1`, id.document)
	if err != nil {
		return ConfigDocumentMetadata{}, &Error{kind: Unavailable}
	}
	defer rows.Close()
	if !rows.Next() {
		if rows.Err() != nil {
			return ConfigDocumentMetadata{}, &Error{kind: Unavailable}
		}
		return ConfigDocumentMetadata{}, &Error{kind: ConfigNotFound}
	}
	d, err := scanConfigDocument(rows)
	if err != nil {
		return ConfigDocumentMetadata{}, err
	}
	rows.Close()
	return d, nil
}

func (s *Store) ReadConfigDocument(ctx context.Context, id ConfigDocumentID) (ConfigDocumentMetadata, error) {
	if !id.Valid() {
		return ConfigDocumentMetadata{}, &Error{kind: ConfigInvalid}
	}
	ctx, cancel, tx, err := publicConfigRead(ctx, s)
	if err != nil {
		return ConfigDocumentMetadata{}, err
	}
	defer cancel()
	defer rollback(tx)
	document, err := readConfigDocumentMetadataTx(ctx, tx, id)
	if err != nil {
		return ConfigDocumentMetadata{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ConfigDocumentMetadata{}, &Error{kind: Unavailable}
	}
	return document, nil
}

// ReadConfigDocumentState admits every delivered versioned document kind. The
// operational ID is trusted composition used for this Run's workspace binding;
// it is not a caller-supplied physical locator. Both documents, active membership,
// generation and catalogue continuation cut share one repeatable-read snapshot.
func (s *Store) ReadConfigDocumentState(ctx context.Context, id, operational ConfigDocumentID) (ConfigDocumentState, error) {
	if !id.Valid() || !operational.Valid() {
		return ConfigDocumentState{}, &Error{kind: ConfigInvalid}
	}
	ctx, cancel, tx, err := publicConfigRead(ctx, s)
	if err != nil {
		return ConfigDocumentState{}, err
	}
	defer cancel()
	defer rollback(tx)
	var state ConfigDocumentState
	state.document, err = readConfigDocumentMetadataTx(ctx, tx, id)
	if err != nil {
		return ConfigDocumentState{}, err
	}
	state.operational = state.document
	if id != operational {
		state.operational, err = readConfigDocumentMetadataTx(ctx, tx, operational)
		if err != nil {
			return ConfigDocumentState{}, err
		}
	}
	if state.operational.kind != ConfigDocumentKindOperationalYAML {
		return ConfigDocumentState{}, &Error{kind: ConfigIntegrity}
	}
	var revision *int64
	var digest []byte
	var sealed *bool
	if err := tx.QueryRow(ctx, `SELECT a.generation_revision,
 CASE WHEN octet_length(g.manifest_hash)=32 THEN g.manifest_hash END,g.sealed
 FROM public.config_active a LEFT JOIN public.config_generations g ON g.revision=a.generation_revision
 WHERE a.singleton`).Scan(&revision, &digest, &sealed); err != nil {
		return ConfigDocumentState{}, &Error{kind: Unavailable}
	}
	if revision == nil || *revision < 1 || len(digest) != 32 || sealed == nil || !*sealed {
		return ConfigDocumentState{}, &Error{kind: ConfigIntegrity}
	}
	state.generation = ConfigGenerationRevision{generation: *revision}
	copy(state.manifest[:], digest)
	state.cut, err = configDocumentCutTx(ctx, tx)
	if err != nil {
		return ConfigDocumentState{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ConfigDocumentState{}, &Error{kind: Unavailable}
	}
	return state, nil
}

// ReadConfigVersionPage includes references whose version was purged. Missing
// metadata stays absent; a generation timestamp is never invented as a version's
// updated_at. Retained and purged entries share the same repeatable-read cut.
func (s *Store) ReadConfigVersionPage(ctx context.Context, id ConfigDocumentID, before int64, size int, expected [sha256.Size]byte) (ConfigDocumentPage, error) {
	if !id.Valid() || before < 0 || size < 1 || size > configPublicPageLimit {
		return ConfigDocumentPage{}, &Error{kind: ConfigInvalid}
	}
	ctx, cancel, tx, err := publicConfigRead(ctx, s)
	if err != nil {
		return ConfigDocumentPage{}, err
	}
	defer cancel()
	defer rollback(tx)
	cut, err := configDocumentCutTx(ctx, tx)
	if err != nil {
		return ConfigDocumentPage{}, err
	}
	if expected != ([sha256.Size]byte{}) && expected != cut.digest {
		return ConfigDocumentPage{}, &Error{kind: ConfigConflict}
	}
	rows, err := tx.Query(ctx, `WITH refs AS (
		SELECT revision,source_hash FROM public.config_versions WHERE document_id=$1
		UNION SELECT version_revision,source_hash FROM public.config_generation_entries WHERE document_id=$1)
		SELECT d.id,r.revision,d.kind::text,v.schema_version::text,v.syntax_profile::text,
		CASE WHEN octet_length(r.source_hash)=32 THEN r.source_hash END,v.accepted_at,
		EXISTS(SELECT 1 FROM public.config_active a JOIN public.config_generation_entries e ON e.generation_revision=a.generation_revision
		WHERE e.document_id=d.id AND e.version_revision=r.revision)
		OR EXISTS(SELECT 1 FROM public.config_bootstrap_active b WHERE b.document_id=d.id AND b.version_revision=r.revision)
		OR EXISTS(SELECT 1 FROM public.authorization_policy_active p WHERE p.document_id=d.id AND p.version_revision=r.revision)
		OR EXISTS(SELECT 1 FROM public.auth_mfa_policy p WHERE p.document_id=d.id AND p.version_revision=r.revision)
		FROM refs r JOIN public.config_documents d ON d.id=$1 LEFT JOIN public.config_versions v ON v.document_id=d.id AND v.revision=r.revision
		WHERE ($2::bigint=0 OR r.revision<$2) ORDER BY r.revision DESC LIMIT $3`, id.document, before, size+1)
	if err != nil {
		return ConfigDocumentPage{}, &Error{kind: Unavailable}
	}
	defer rows.Close()
	page := ConfigDocumentPage{cut: cut}
	for rows.Next() {
		d, err := scanConfigDocument(rows)
		if err != nil {
			return ConfigDocumentPage{}, err
		}
		if len(page.documents) == size {
			page.more = true
			break
		}
		page.documents = append(page.documents, d)
	}
	if rows.Err() != nil {
		return ConfigDocumentPage{}, &Error{kind: Unavailable}
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return ConfigDocumentPage{}, &Error{kind: Unavailable}
	}
	return page, nil
}
