package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/jackc/pgx/v5"
)

const (
	configOperationalSchema  = "homeward.operational/1"
	configOperationalProfile = "homeward.operational-yaml/1"
	configBootstrapSchema    = "homeward.bootstrap-launch/1"
	configBootstrapProfile   = "homeward.bootstrap-launch-yaml/1"
	configProtectedSQL       = "protected_operational_v1"
	configPlainSQL           = "plain_v1"
	configCompiledManifest   = "compiled_v2"
	configLegacyManifest     = "legacy_v1"
)

// ConfigSourceRepresentation is a closed SQL representation set. Zero and a
// future enum label are never treated as an ordinary public YAML source.
type ConfigSourceRepresentation uint8

const (
	ConfigSourceUnknown ConfigSourceRepresentation = iota
	ConfigSourcePlainV1
	ConfigSourceProtectedOperationalV1
)

func (r ConfigSourceRepresentation) String() string {
	switch r {
	case ConfigSourcePlainV1:
		return configPlainSQL
	case ConfigSourceProtectedOperationalV1:
		return configProtectedSQL
	default:
		return ""
	}
}

func parseConfigSourceRepresentation(raw string) (ConfigSourceRepresentation, error) {
	switch raw {
	case configPlainSQL:
		return ConfigSourcePlainV1, nil
	case configProtectedSQL:
		return ConfigSourceProtectedOperationalV1, nil
	default:
		return ConfigSourceUnknown, &Error{kind: ConfigIntegrity}
	}
}

type ConfigManifestFormat uint8

const (
	ConfigManifestUnknown ConfigManifestFormat = iota
	ConfigManifestLegacyV1
	ConfigManifestCompiledV2
)

func (f ConfigManifestFormat) String() string {
	switch f {
	case ConfigManifestLegacyV1:
		return configLegacyManifest
	case ConfigManifestCompiledV2:
		return configCompiledManifest
	default:
		return ""
	}
}

func parseConfigManifestFormat(raw string) (ConfigManifestFormat, error) {
	switch raw {
	case configLegacyManifest:
		return ConfigManifestLegacyV1, nil
	case configCompiledManifest:
		return ConfigManifestCompiledV2, nil
	default:
		return ConfigManifestUnknown, &Error{kind: ConfigIntegrity}
	}
}

// ConfigReplayRepository is a deliberately separate reader for protected
// operational sources. It can only be constructed with the key bootstrapped
// for this exact Store; generic configuration snapshots never hold plaintext.
type ConfigReplayRepository struct {
	store   *Store
	secrets *SecretRepository
}

func NewConfigReplayRepository(store *Store, secrets *SecretRepository) (*ConfigReplayRepository, error) {
	if store == nil || store.pool == nil || !secrets.Valid() || secrets.store != store {
		return nil, &Error{kind: ConfigInvalid}
	}
	return &ConfigReplayRepository{store: store, secrets: secrets}, nil
}

// ConfigProtectedSource limits plaintext access to callers holding the replay
// repository. Its default formatting and JSON representation are redacted.
type ConfigProtectedSource struct {
	ref  ConfigVersionRef
	path string
	hash [sha256.Size]byte
	raw  []byte
}

func (s ConfigProtectedSource) Ref() ConfigVersionRef     { return s.ref }
func (s ConfigProtectedSource) Path() string              { return s.path }
func (s ConfigProtectedSource) SHA256() [sha256.Size]byte { return s.hash }
func (s ConfigProtectedSource) Bytes() []byte             { return bytes.Clone(s.raw) }
func (s ConfigProtectedSource) String() string            { return "[REDACTED CONFIG SOURCE]" }
func (s ConfigProtectedSource) GoString() string          { return s.String() }
func (s ConfigProtectedSource) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// SaveProtectedOperationalSource persists exact already-admitted bytes and a
// caller-supplied digest. Validation of the operational YAML belongs to the
// configuration parser; this boundary verifies identity, bounds and sealing.
func (c *ConfigTx) SaveProtectedOperationalSource(replay *ConfigReplayRepository, binding ConfigDocumentBinding, raw []byte, sourceHash [sha256.Size]byte, metadata ConfigVersionMetadata) (ref ConfigVersionRef, err error) {
	defer func() { c.remember(err) }()
	if err := c.valid(); err != nil {
		return ConfigVersionRef{}, err
	}
	if replay == nil || replay.store != c.store || replay.secrets == nil || !replay.secrets.Valid() ||
		!binding.id.Valid() || !metadata.author.Valid() || !metadata.source.Valid() ||
		len(raw) == 0 || len(raw) > 1<<20 || sha256.Sum256(raw) != sourceHash {
		return ConfigVersionRef{}, &Error{kind: ConfigInvalid}
	}
	if _, err := parser.NewSourceFile(binding.path, nil); err != nil {
		return ConfigVersionRef{}, &Error{kind: ConfigInvalid}
	}
	if _, err := c.tx.Exec(c.ctx, `INSERT INTO public.config_documents(id,logical_path,kind)
		VALUES($1,$2,$3::public.config_document_kind) ON CONFLICT(id) DO NOTHING`,
		binding.id.document, binding.path, ConfigDocumentKindOperationalYAML.String()); err != nil {
		return ConfigVersionRef{}, configWriteError(err)
	}
	var path, storedKind string
	if err := c.tx.QueryRow(c.ctx, `SELECT logical_path,kind::text FROM public.config_documents WHERE id=$1`, binding.id.document).Scan(&path, &storedKind); err != nil {
		return ConfigVersionRef{}, err
	}
	if path != binding.path || storedKind != ConfigDocumentKindOperationalYAML.String() {
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
	var salt [32]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return ConfigVersionRef{}, &Error{kind: SecretUnavailable}
	}
	if _, err := c.tx.Exec(c.ctx, `INSERT INTO public.secret_salts(salt) VALUES ($1)`, salt[:]); err != nil {
		return ConfigVersionRef{}, configWriteError(err)
	}
	aad := configSourceAAD(binding.id.document, binding.path, revision, sourceHash, replay.secrets.key.id[:], salt[:])
	ciphertext, err := sealProtectedRecord(&replay.secrets.key, protectedConfigSource, aad, salt[:], raw)
	if err != nil {
		return ConfigVersionRef{}, err
	}
	if len(ciphertext) != len(raw)+28 {
		return ConfigVersionRef{}, &Error{kind: ConfigIntegrity}
	}
	_, err = c.tx.Exec(c.ctx, `INSERT INTO public.config_versions
		(document_id,revision,parent_revision,source_hash,author,source,schema_version,syntax_profile,
		 source_representation,ciphertext,key_id,suite,salt)
		VALUES($1,$2,$3,$4,$5,$6,$7::public.config_schema_version,$8::public.config_syntax_profile,
		 $9::public.config_source_representation,$10,$11,$12::public.secret_cipher_suite,$13)`,
		binding.id.document, revision, parent, sourceHash[:], metadata.author.author, metadata.source.source,
		configOperationalSchema, configOperationalProfile, configProtectedSQL, ciphertext,
		replay.secrets.key.id[:], secretSuiteSQL, salt[:])
	if err != nil {
		return ConfigVersionRef{}, configWriteError(err)
	}
	if c.touched == nil {
		c.touched = make(map[string]ConfigDocumentID)
	}
	c.touched[binding.id.document] = binding.id
	return ConfigVersionRef{document: binding.id, revision: ConfigVersionRevision{version: revision}}, nil
}

func configSourceAAD(document, path string, revision int64, sourceHash [sha256.Size]byte, keyID, salt []byte) []byte {
	var aad []byte
	for _, part := range [][]byte{
		[]byte("homeward/config-source-aad/v1"), []byte(secretSuiteSQL), keyID, salt,
		[]byte(document), []byte(path), []byte(ConfigDocumentKindOperationalYAML.String()),
		[]byte(configOperationalSchema), []byte(configOperationalProfile), sourceHash[:],
	} {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(part)))
		aad = append(aad, size[:]...)
		aad = append(aad, part...)
	}
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], uint64(revision))
	return append(aad, number[:]...)
}

// ReadActiveProtectedSource verifies the active generation stamp and membership
// in one SQL snapshot, then authenticates exact source bytes. A changed active
// pointer fails closed instead of returning a source from another generation.
func (r *ConfigReplayRepository) ReadActiveProtectedSource(ctx context.Context, generation ConfigGenerationRevision, manifest [sha256.Size]byte, ref ConfigVersionRef) (ConfigProtectedSource, error) {
	if ctx == nil || r == nil || r.store == nil || r.secrets == nil || !r.secrets.Valid() ||
		r.secrets.store != r.store || !generation.Valid() || !ref.Valid() || manifest == ([sha256.Size]byte{}) {
		return ConfigProtectedSource{}, &Error{kind: ConfigInvalid}
	}
	tx, err := r.store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ConfigProtectedSource{}, &Error{kind: Unavailable}
	}
	defer rollback(tx)
	var active *int64
	var storedManifest []byte
	var format string
	var sealed bool
	err = tx.QueryRow(ctx, `SELECT a.generation_revision,g.manifest_hash,g.manifest_format::text,g.sealed
		FROM public.config_active a LEFT JOIN public.config_generations g ON g.revision=a.generation_revision
		WHERE a.singleton`).Scan(&active, &storedManifest, &format, &sealed)
	if err != nil || active == nil || *active != generation.generation || !sealed ||
		len(storedManifest) != sha256.Size || !bytes.Equal(storedManifest, manifest[:]) || format != configCompiledManifest {
		return ConfigProtectedSource{}, &Error{kind: ConfigConflict}
	}
	source, err := r.readProtectedSourceTx(ctx, tx, generation, ref)
	if err != nil {
		return ConfigProtectedSource{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ConfigProtectedSource{}, &Error{kind: Unavailable}
	}
	return source, nil
}

// ReadHistoricalProtectedSource is trusted Go composition for explicitly
// selected retained history, never an active-generation or HTTP admission.
// The complete sealed manifest and protected member share one read-only SQL
// snapshot. Normal retention fails closed; it cannot become an empty source.
func (r *ConfigReplayRepository) ReadHistoricalProtectedSource(ctx context.Context, generation ConfigGenerationRevision, manifest [sha256.Size]byte, ref ConfigVersionRef) (ConfigProtectedSource, error) {
	if err := bootstrapReadContextError(ctx); err != nil {
		return ConfigProtectedSource{}, err
	}
	if r == nil || r.store == nil || r.store.pool == nil || r.secrets == nil || !r.secrets.Valid() ||
		r.secrets.store != r.store || !generation.Valid() || !ref.Valid() || manifest == ([sha256.Size]byte{}) {
		return ConfigProtectedSource{}, &Error{kind: ConfigInvalid}
	}
	tx, err := r.store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ConfigProtectedSource{}, &Error{kind: Unavailable}
	}
	defer rollback(tx)
	snapshot, err := readConfigGenerationTx(ctx, tx, generation)
	if err != nil {
		// Do not let driver/parser causes escape the protected source boundary.
		var failure *Error
		if errors.As(err, &failure) {
			return ConfigProtectedSource{}, &Error{kind: failure.Kind()}
		}
		return ConfigProtectedSource{}, &Error{kind: Unavailable}
	}
	if snapshot.manifestHash != manifest {
		return ConfigProtectedSource{}, &Error{kind: ConfigConflict}
	}
	if snapshot.availability != ConfigGenerationComplete {
		return ConfigProtectedSource{}, &Error{kind: ConfigIncomplete}
	}
	if snapshot.manifestFormat != ConfigManifestCompiledV2 {
		return ConfigProtectedSource{}, &Error{kind: ConfigProtected}
	}
	member := false
	for _, version := range snapshot.versions {
		if version.ref == ref && version.kind == ConfigDocumentKindOperationalYAML &&
			version.representation == ConfigSourceProtectedOperationalV1 {
			member = true
			break
		}
	}
	if !member {
		return ConfigProtectedSource{}, &Error{kind: ConfigNotFound}
	}
	source, err := r.readProtectedSourceTx(ctx, tx, generation, ref)
	if err != nil {
		return ConfigProtectedSource{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ConfigProtectedSource{}, &Error{kind: Unavailable}
	}
	return source, nil
}

// readProtectedSourceTx opens only an exact member of the wrapper's admitted
// generation. It never chooses whether an active or historical read is allowed.
// No source payload is materialized before all metadata and lengths are admitted.
func (r *ConfigReplayRepository) readProtectedSourceTx(ctx context.Context, tx pgx.Tx, generation ConfigGenerationRevision, ref ConfigVersionRef) (ConfigProtectedSource, error) {
	var path *string
	var kind, schema, profile, representation, suite string
	var digest, keyID, salt []byte
	var ciphertextBytes *int
	var noPlain bool
	err := tx.QueryRow(ctx, `SELECT
		CASE WHEN octet_length(d.logical_path) BETWEEN 1 AND 4096 THEN d.logical_path END,
		d.kind::text,v.schema_version::text,v.syntax_profile::text,v.source_representation::text,
		CASE WHEN octet_length(v.source_hash)=32 THEN v.source_hash END,
		CASE WHEN octet_length(v.key_id)=32 THEN v.key_id END,v.suite::text,
		CASE WHEN octet_length(v.salt)=32 THEN v.salt END,
		octet_length(v.ciphertext),v.yaml_source IS NULL
		FROM public.config_generation_entries e
		JOIN public.config_documents d ON d.id=e.document_id
		JOIN public.config_versions v ON v.document_id=e.document_id AND v.revision=e.version_revision
		WHERE e.generation_revision=$1 AND e.document_id=$2 AND e.version_revision=$3 AND e.source_hash=v.source_hash`,
		generation.generation, ref.document.document, ref.revision.version).
		Scan(&path, &kind, &schema, &profile, &representation, &digest, &keyID, &suite, &salt, &ciphertextBytes, &noPlain)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConfigProtectedSource{}, &Error{kind: ConfigNotFound}
	}
	if err != nil {
		return ConfigProtectedSource{}, &Error{kind: Unavailable}
	}
	if path == nil || kind != ConfigDocumentKindOperationalYAML.String() || schema != configOperationalSchema ||
		profile != configOperationalProfile || representation != configProtectedSQL || suite != secretSuiteSQL || !noPlain ||
		len(digest) != sha256.Size || len(keyID) != sha256.Size || !bytes.Equal(keyID, r.secrets.key.id[:]) ||
		len(salt) != 32 || ciphertextBytes == nil || *ciphertextBytes < 29 || *ciphertextBytes > (1<<20)+28 {
		return ConfigProtectedSource{}, &Error{kind: ConfigIntegrity}
	}
	if _, err := parser.NewSourceFile(*path, nil); err != nil {
		return ConfigProtectedSource{}, &Error{kind: ConfigIntegrity}
	}
	var ciphertext []byte
	// Repeat the bound in SQL as well as retaining the same snapshot. Even a
	// privileged concurrent writer cannot turn the admitted length into a copy
	// of an arbitrary payload between metadata and this SELECT.
	err = tx.QueryRow(ctx, `SELECT CASE WHEN octet_length(v.ciphertext) BETWEEN 29 AND 1048604 THEN v.ciphertext END
		FROM public.config_versions v WHERE v.document_id=$1 AND v.revision=$2`,
		ref.document.document, ref.revision.version).Scan(&ciphertext)
	if err != nil {
		return ConfigProtectedSource{}, &Error{kind: Unavailable}
	}
	if len(ciphertext) != *ciphertextBytes {
		return ConfigProtectedSource{}, &Error{kind: ConfigIntegrity}
	}
	var hash [sha256.Size]byte
	copy(hash[:], digest)
	aad := configSourceAAD(ref.document.document, *path, ref.revision.version, hash, keyID, salt)
	raw, err := openProtectedRecord(&r.secrets.key, protectedConfigSource, aad, salt, ciphertext)
	if err != nil || len(raw) != len(ciphertext)-28 || sha256.Sum256(raw) != hash {
		return ConfigProtectedSource{}, &Error{kind: ConfigIntegrity}
	}
	return ConfigProtectedSource{ref: ref, path: *path, hash: hash, raw: raw}, nil
}
