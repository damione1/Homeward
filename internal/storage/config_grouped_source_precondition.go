package storage

import (
	"bytes"
	"context"
)

// confirmEditableSourceBinding separates the native path namespace of each
// admitted configuration identity. IDs remain global; a caller cannot move an
// existing ID into another source group by presenting a different descriptor.
func confirmEditableSourceBinding(ctx context.Context, query sourceBindingQuery, descriptor SourceDescriptor, binding ConfigDocumentBinding, revision ConfigVersionRevision, hash [32]byte, absent bool) error {
	if !descriptor.Valid() || !binding.ID().Valid() || configSourcePath(descriptor.Kind().String(), binding.Path()) != nil || absent && (revision.Valid() || hash != ([32]byte{})) || !absent && (!revision.Valid() || hash == ([32]byte{})) {
		return &Error{kind: ConfigInvalid}
	}
	rows, err := query.Query(ctx, `SELECT d.id,d.logical_path,d.kind::text,d.source_identity,d.source_entry,d.last_revision,v.source_hash
 FROM public.config_documents d LEFT JOIN public.config_versions v ON v.document_id=d.id AND v.revision=d.last_revision
 WHERE d.id=$1 OR (d.kind::text=$2 AND d.source_identity=$3 AND d.logical_path=$4) LIMIT 2`, binding.ID().String(), descriptor.Kind().String(), descriptor.Identity(), binding.Path())
	if err != nil {
		return &Error{kind: Unavailable}
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id, path, kind string
		var identity, entry *string
		var current int64
		var source []byte
		if err := rows.Scan(&id, &path, &kind, &identity, &entry, &current, &source); err != nil {
			return &Error{kind: ConfigIntegrity}
		}
		count++
		if absent || count > 1 || id != binding.ID().String() || path != binding.Path() || kind != descriptor.Kind().String() || identity == nil || entry == nil || *identity != descriptor.Identity() || *entry != descriptor.Entry() || current != revision.Number() || !bytes.Equal(source, hash[:]) {
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

func (s *Store) CheckEditableSourceBinding(ctx context.Context, descriptor SourceDescriptor, binding ConfigDocumentBinding, revision ConfigVersionRevision, hash [32]byte, absent bool) error {
	if err := bootstrapReadContextError(ctx); err != nil {
		return err
	}
	if s == nil || s.pool == nil {
		return &Error{kind: ConfigInvalid}
	}
	return confirmEditableSourceBinding(ctx, s.pool, descriptor, binding, revision, hash, absent)
}

func (c *ConfigTx) ConfirmEditableSourceVersion(descriptor SourceDescriptor, binding ConfigDocumentBinding, revision ConfigVersionRevision, hash [32]byte, absent bool) error {
	if err := c.valid(); err != nil {
		return err
	}
	return confirmEditableSourceBinding(c.ctx, c.tx, descriptor, binding, revision, hash, absent)
}
