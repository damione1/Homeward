package storage

import (
	"context"
	"crypto/sha256"
)

// ReadConfigVersionMetadata reads one exact retained reference. The retained
// generation reference survives source collection and still reports PURGED.
func (s *Store) ReadConfigVersionMetadata(ctx context.Context, ref ConfigVersionRef) (ConfigDocumentMetadata, error) {
	if !ref.Valid() {
		return ConfigDocumentMetadata{}, &Error{kind: ConfigInvalid}
	}
	ctx, cancel, tx, err := publicConfigRead(ctx, s)
	if err != nil {
		return ConfigDocumentMetadata{}, err
	}
	defer cancel()
	defer rollback(tx)
	rows, err := tx.Query(ctx, `WITH refs AS (
 SELECT revision,source_hash FROM public.config_versions WHERE document_id=$1 AND revision=$2
 UNION SELECT version_revision,source_hash FROM public.config_generation_entries WHERE document_id=$1 AND version_revision=$2)
 SELECT d.id,r.revision,d.kind::text,v.schema_version::text,v.syntax_profile::text,
 CASE WHEN octet_length(r.source_hash)=32 THEN r.source_hash END,v.accepted_at,false
 FROM refs r JOIN public.config_documents d ON d.id=$1 LEFT JOIN public.config_versions v ON v.document_id=d.id AND v.revision=r.revision LIMIT 2`, ref.Document().String(), ref.Revision().Number())
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
	value, err := scanConfigDocument(rows)
	if err != nil {
		return ConfigDocumentMetadata{}, err
	}
	if rows.Next() {
		return ConfigDocumentMetadata{}, &Error{kind: ConfigIntegrity}
	}
	if rows.Err() != nil {
		return ConfigDocumentMetadata{}, &Error{kind: Unavailable}
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return ConfigDocumentMetadata{}, &Error{kind: Unavailable}
	}
	return value, nil
}

// ReadConfigGenerationMetadata reads a sealed generation without fetching its
// source bytes. Missing version rows remain explicitly PARTIALLY_PURGED.
func (s *Store) ReadConfigGenerationMetadata(ctx context.Context, revision ConfigGenerationRevision) (ConfigGenerationMetadata, error) {
	if !revision.Valid() {
		return ConfigGenerationMetadata{}, &Error{kind: ConfigInvalid}
	}
	ctx, cancel, tx, err := publicConfigRead(ctx, s)
	if err != nil {
		return ConfigGenerationMetadata{}, err
	}
	defer cancel()
	defer rollback(tx)
	rows, err := tx.Query(ctx, `SELECT g.manifest_hash,g.created_at,g.entry_count,
 (SELECT count(*) FROM public.config_generation_entries e WHERE e.generation_revision=g.revision),
 EXISTS(SELECT 1 FROM public.config_generation_entries e LEFT JOIN public.config_versions v ON v.document_id=e.document_id AND v.revision=e.version_revision WHERE e.generation_revision=g.revision AND v.revision IS NULL)
 FROM public.config_generations g WHERE g.sealed AND g.revision=$1`, revision.Number())
	if err != nil {
		return ConfigGenerationMetadata{}, &Error{kind: Unavailable}
	}
	defer rows.Close()
	if !rows.Next() {
		if rows.Err() != nil {
			return ConfigGenerationMetadata{}, &Error{kind: Unavailable}
		}
		return ConfigGenerationMetadata{}, &Error{kind: ConfigNotFound}
	}
	var value ConfigGenerationMetadata
	var digest []byte
	var expected, count int
	var partial bool
	if rows.Scan(&digest, &value.created, &expected, &count, &partial) != nil {
		return ConfigGenerationMetadata{}, &Error{kind: Unavailable}
	}
	if len(digest) != sha256.Size || value.created.IsZero() || expected < 1 || expected > configSourceLimitCount || count != expected {
		return ConfigGenerationMetadata{}, &Error{kind: ConfigIntegrity}
	}
	value.revision = revision
	copy(value.manifest[:], digest)
	value.availability = ConfigGenerationComplete
	if partial {
		value.availability = ConfigGenerationIncomplete
	}
	if rows.Next() || rows.Err() != nil {
		return ConfigGenerationMetadata{}, &Error{kind: ConfigIntegrity}
	}
	rows.Close()
	if tx.Commit(ctx) != nil {
		return ConfigGenerationMetadata{}, &Error{kind: Unavailable}
	}
	return value, nil
}
