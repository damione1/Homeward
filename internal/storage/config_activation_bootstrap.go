package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/jackc/pgx/v5"
)

// SaveBootstrapLaunchSource stores an admitted launch-only document outside the
// operational manifest. The caller's launch parser owns the closed field set;
// storage enforces exact bytes, hash and the SQL kind/profile boundary.
func (c *ConfigTx) SaveBootstrapLaunchSource(binding ConfigDocumentBinding, raw []byte, hash [sha256.Size]byte, metadata ConfigVersionMetadata) (ref ConfigVersionRef, err error) {
	defer func() { c.remember(err) }()
	if len(raw) == 0 {
		return ConfigVersionRef{}, &Error{kind: ConfigInvalid}
	}
	return c.saveAdmittedSource(binding, configSourceBootstrapLaunch, raw, hash, metadata)
}

type bootstrapActiveExpectationKind uint8

const (
	bootstrapActiveUnknown bootstrapActiveExpectationKind = iota
	bootstrapActiveAbsent
	bootstrapActiveVersion
)

// ExpectedBootstrapActive has an explicit absent branch. A revision without
// its exact plaintext hash, or an uninitialized zero value, cannot CAS.
type ExpectedBootstrapActive struct {
	kind bootstrapActiveExpectationKind
	ref  ConfigVersionRef
	hash [sha256.Size]byte
}

func ExpectNoBootstrapActive() ExpectedBootstrapActive {
	return ExpectedBootstrapActive{kind: bootstrapActiveAbsent}
}

func ExpectBootstrapActive(ref ConfigVersionRef, hash [sha256.Size]byte) (ExpectedBootstrapActive, error) {
	if !ref.Valid() || hash == ([sha256.Size]byte{}) {
		return ExpectedBootstrapActive{}, &Error{kind: ConfigInvalid}
	}
	return ExpectedBootstrapActive{kind: bootstrapActiveVersion, ref: ref, hash: hash}, nil
}

func (c *ConfigTx) ActivateBootstrapSource(expected ExpectedBootstrapActive, target ConfigVersionRef) (err error) {
	defer func() { c.remember(err) }()
	if err := c.valid(); err != nil {
		return err
	}
	if !target.Valid() ||
		(expected.kind == bootstrapActiveAbsent && (expected.ref.Valid() || expected.hash != ([sha256.Size]byte{}))) ||
		(expected.kind != bootstrapActiveAbsent &&
			(expected.kind != bootstrapActiveVersion || !expected.ref.Valid() || expected.hash == ([sha256.Size]byte{}))) {
		return &Error{kind: ConfigInvalid}
	}
	var document *string
	var revision *int64
	var currentHash []byte
	if err := c.tx.QueryRow(c.ctx, `SELECT document_id,version_revision,source_hash
		FROM public.config_bootstrap_active WHERE singleton FOR UPDATE`).Scan(&document, &revision, &currentHash); err != nil {
		return err
	}
	if (expected.kind == bootstrapActiveAbsent && revision != nil) ||
		(expected.kind == bootstrapActiveVersion && (document == nil || revision == nil ||
			*document != expected.ref.document.document || *revision != expected.ref.revision.version ||
			!bytes.Equal(currentHash, expected.hash[:]))) {
		return &Error{kind: ConfigConflict}
	}
	if revision != nil && (target.document != expected.ref.document || target.revision.version <= *revision) {
		return &Error{kind: ConfigConflict}
	}
	var kind, schema, profile, representation string
	var hash, plain []byte
	err = c.tx.QueryRow(c.ctx, `SELECT d.kind::text,v.schema_version::text,v.syntax_profile::text,
		v.source_representation::text,v.source_hash,v.yaml_source
		FROM public.config_versions v JOIN public.config_documents d ON d.id=v.document_id
		WHERE v.document_id=$1 AND v.revision=$2`, target.document.document, target.revision.version).
		Scan(&kind, &schema, &profile, &representation, &hash, &plain)
	if errors.Is(err, pgx.ErrNoRows) {
		return &Error{kind: ConfigIncomplete}
	}
	if err != nil {
		return err
	}
	if kind != ConfigDocumentKindBootstrapLaunch.String() || schema != configBootstrapSchema ||
		profile != configBootstrapProfile || representation != configPlainSQL || len(hash) != sha256.Size ||
		len(plain) == 0 || len(plain) > 1<<20 || !bytes.Equal(hash, sha256Bytes(plain)) {
		return &Error{kind: ConfigIntegrity}
	}
	tag, err := c.tx.Exec(c.ctx, `UPDATE public.config_bootstrap_active SET document_id=$1,version_revision=$2,
		source_hash=$3,activated_at=clock_timestamp() WHERE singleton`, target.document.document, target.revision.version, hash)
	if err != nil {
		return configWriteError(err)
	}
	if tag.RowsAffected() != 1 {
		return &Error{kind: ConfigIntegrity}
	}
	return nil
}

func sha256Bytes(raw []byte) []byte {
	hash := sha256.Sum256(raw)
	return hash[:]
}

// ConfigBootstrapSnapshot is a launch-only SQL pointer and exact source. It
// never selects a database address before the caller has reached this Store.
type ConfigBootstrapSnapshot struct {
	ref  ConfigVersionRef
	path string
	hash [sha256.Size]byte
	raw  []byte
}

func (b ConfigBootstrapSnapshot) Ref() ConfigVersionRef        { return b.ref }
func (b ConfigBootstrapSnapshot) Path() string                 { return b.path }
func (b ConfigBootstrapSnapshot) SHA256() [sha256.Size]byte    { return b.hash }
func (b ConfigBootstrapSnapshot) Bytes() []byte                { return bytes.Clone(b.raw) }
func (b ConfigBootstrapSnapshot) String() string               { return "[REDACTED BOOTSTRAP SOURCE]" }
func (b ConfigBootstrapSnapshot) GoString() string             { return b.String() }
func (b ConfigBootstrapSnapshot) MarshalJSON() ([]byte, error) { return json.Marshal(b.String()) }

func (s *Store) ReadActiveBootstrap(ctx context.Context) (ConfigBootstrapSnapshot, bool, error) {
	if err := bootstrapReadContextError(ctx); err != nil {
		return ConfigBootstrapSnapshot{}, false, err
	}
	if s == nil || s.pool == nil {
		return ConfigBootstrapSnapshot{}, false, &Error{kind: ConfigInvalid}
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ConfigBootstrapSnapshot{}, false, &Error{kind: Unavailable}
	}
	defer rollback(tx)
	b, found, err := readActiveBootstrapTx(ctx, tx)
	if err != nil {
		return ConfigBootstrapSnapshot{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ConfigBootstrapSnapshot{}, false, &Error{kind: Unavailable, cause: err}
	}
	return b, found, nil
}

// ReadActiveBootstrapAfterWriter uses a fresh connection and waits for the
// configuration writer's SQL gate before taking a READ COMMITTED snapshot.
// This is the only bootstrap pointer read that may reconcile a lost COMMIT
// acknowledgement. A deadline while waiting is never a proven absence.
func (s *Store) ReadActiveBootstrapAfterWriter(ctx context.Context) (ConfigBootstrapSnapshot, bool, error) {
	if err := bootstrapReadContextError(ctx); err != nil {
		return ConfigBootstrapSnapshot{}, false, err
	}
	if s == nil || s.pool == nil {
		return ConfigBootstrapSnapshot{}, false, &Error{kind: ConfigInvalid}
	}
	connection, err := pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig)
	if err != nil {
		return ConfigBootstrapSnapshot{}, false, &Error{kind: Unavailable, cause: err}
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = connection.Close(closeCtx)
	}()
	tx, err := connection.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ConfigBootstrapSnapshot{}, false, &Error{kind: Unavailable, cause: err}
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(481316,151)`); err != nil {
		return ConfigBootstrapSnapshot{}, false, &Error{kind: Unavailable, cause: err}
	}
	b, found, err := readActiveBootstrapTx(ctx, tx)
	if err != nil {
		return ConfigBootstrapSnapshot{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ConfigBootstrapSnapshot{}, false, &Error{kind: Unavailable, cause: err}
	}
	return b, found, nil
}

func bootstrapReadContextError(ctx context.Context) error {
	if ctx == nil {
		return &Error{kind: ConfigInvalid}
	}
	value := reflect.ValueOf(ctx)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return &Error{kind: ConfigInvalid}
		}
	}
	return ctx.Err()
}

func readActiveBootstrapTx(ctx context.Context, tx pgx.Tx) (ConfigBootstrapSnapshot, bool, error) {
	var document *string
	var revision *int64
	var pointerHash []byte
	if err := tx.QueryRow(ctx, `SELECT document_id,version_revision,source_hash FROM public.config_bootstrap_active WHERE singleton`).
		Scan(&document, &revision, &pointerHash); err != nil {
		return ConfigBootstrapSnapshot{}, false, &Error{kind: ConfigIntegrity}
	}
	if document == nil && revision == nil && pointerHash == nil {
		return ConfigBootstrapSnapshot{}, false, nil
	}
	if document == nil || revision == nil || len(pointerHash) != sha256.Size {
		return ConfigBootstrapSnapshot{}, false, &Error{kind: ConfigIntegrity}
	}
	var b ConfigBootstrapSnapshot
	var err error
	var kind, schema, profile, representation string
	var versionHash []byte
	b.ref.document, err = NewConfigDocumentID(*document)
	if err != nil {
		return ConfigBootstrapSnapshot{}, false, &Error{kind: ConfigIntegrity}
	}
	b.ref.revision, err = NewConfigVersionRevision(*revision)
	if err != nil {
		return ConfigBootstrapSnapshot{}, false, &Error{kind: ConfigIntegrity}
	}
	err = tx.QueryRow(ctx, `SELECT d.logical_path,d.kind::text,v.schema_version::text,v.syntax_profile::text,
		v.source_representation::text,v.source_hash,v.yaml_source
		FROM public.config_versions v JOIN public.config_documents d ON d.id=v.document_id
		WHERE v.document_id=$1 AND v.revision=$2`, *document, *revision).
		Scan(&b.path, &kind, &schema, &profile, &representation, &versionHash, &b.raw)
	if err != nil || kind != ConfigDocumentKindBootstrapLaunch.String() || schema != configBootstrapSchema ||
		profile != configBootstrapProfile || representation != configPlainSQL || len(versionHash) != sha256.Size ||
		!bytes.Equal(pointerHash, versionHash) || len(b.raw) == 0 || len(b.raw) > 1<<20 ||
		!bytes.Equal(versionHash, sha256Bytes(b.raw)) {
		return ConfigBootstrapSnapshot{}, false, &Error{kind: ConfigIntegrity}
	}
	if _, err := parser.NewSourceFile(b.path, nil); err != nil {
		return ConfigBootstrapSnapshot{}, false, &Error{kind: ConfigIntegrity}
	}
	copy(b.hash[:], versionHash)
	b.raw = bytes.Clone(b.raw)
	return b, true, nil
}
