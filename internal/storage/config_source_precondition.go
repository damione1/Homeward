package storage

import (
	"bytes"
	"context"

	"github.com/jackc/pgx/v5"
)

type sourceBindingQuery interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func confirmSourceBinding(ctx context.Context, query sourceBindingQuery, binding ConfigDocumentBinding, kind ConfigDocumentKind, revision ConfigVersionRevision, hash [32]byte, absent bool) error {
	if !binding.ID().Valid() || binding.Path() == "" || kind.String() == "" || absent && (revision.Valid() || hash != ([32]byte{})) || !absent && (!revision.Valid() || hash == ([32]byte{})) {
		return &Error{kind: ConfigInvalid}
	}
	rows, err := query.Query(ctx, `SELECT d.id,d.logical_path,d.kind::text,d.last_revision,v.source_hash
		FROM public.config_documents d LEFT JOIN public.config_versions v ON v.document_id=d.id AND v.revision=d.last_revision
		WHERE d.id=$1 OR (d.logical_path=$2 AND d.kind::text=$3) LIMIT 2`, binding.ID().String(), binding.Path(), kind.String())
	if err != nil {
		return &Error{kind: Unavailable}
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id, path, storedKind string
		var current int64
		var source []byte
		if err := rows.Scan(&id, &path, &storedKind, &current, &source); err != nil {
			return &Error{kind: ConfigIntegrity}
		}
		count++
		if absent || count > 1 || id != binding.ID().String() || path != binding.Path() || storedKind != kind.String() || current != revision.Number() || !bytes.Equal(source, hash[:]) {
			return &Error{kind: ConfigConflict}
		}
	}
	if rows.Err() != nil {
		return &Error{kind: Unavailable}
	}
	if absent && count == 0 || !absent && count == 1 {
		return nil
	}
	return &Error{kind: ConfigConflict}
}

// CheckSourceBinding rejects an identity/path/kind collision before filesystem
// staging. An absent document ID does not grant an already-owned logical path.
func (s *Store) CheckSourceBinding(ctx context.Context, binding ConfigDocumentBinding, kind ConfigDocumentKind, revision ConfigVersionRevision, hash [32]byte, absent bool) error {
	if err := bootstrapReadContextError(ctx); err != nil {
		return err
	}
	if s == nil || s.pool == nil {
		return &Error{kind: ConfigInvalid}
	}
	return confirmSourceBinding(ctx, s.pool, binding, kind, revision, hash, absent)
}

// ConfirmSourceVersion repeats the complete binding CAS under the existing
// ConfigTx advisory lock before creating any version. Absence stays explicit.
func (c *ConfigTx) ConfirmSourceVersion(binding ConfigDocumentBinding, kind ConfigDocumentKind, revision ConfigVersionRevision, hash [32]byte, absent bool) error {
	if err := c.valid(); err != nil {
		return err
	}
	return confirmSourceBinding(c.ctx, c.tx, binding, kind, revision, hash, absent)
}
