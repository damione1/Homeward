package storage

import (
	"context"
	"crypto/sha256"
	"time"
)

type ConfigGenerationMetadata struct {
	revision     ConfigGenerationRevision
	manifest     [32]byte
	created      time.Time
	availability ConfigGenerationAvailability
}

func (g ConfigGenerationMetadata) Revision() ConfigGenerationRevision         { return g.revision }
func (g ConfigGenerationMetadata) ManifestSHA256() [32]byte                   { return g.manifest }
func (g ConfigGenerationMetadata) CreatedAt() time.Time                       { return g.created }
func (g ConfigGenerationMetadata) Availability() ConfigGenerationAvailability { return g.availability }

type ConfigGenerationPage struct {
	generations []ConfigGenerationMetadata
	cut         ConfigDocumentCut
	more        bool
}

func (p ConfigGenerationPage) Generations() []ConfigGenerationMetadata {
	return append([]ConfigGenerationMetadata(nil), p.generations...)
}
func (p ConfigGenerationPage) Cut() ConfigDocumentCut { return p.cut }
func (p ConfigGenerationPage) More() bool             { return p.more }

// ReadConfigGenerationPage reads metadata only. A missing source version is an
// explicitly partial generation, not a missing page or a restorable empty YAML.
func (s *Store) ReadConfigGenerationPage(ctx context.Context, before int64, size int, expected [32]byte) (ConfigGenerationPage, error) {
	if before < 0 || size < 1 || size > configPublicPageLimit {
		return ConfigGenerationPage{}, &Error{kind: ConfigInvalid}
	}
	ctx, cancel, tx, err := publicConfigRead(ctx, s)
	if err != nil {
		return ConfigGenerationPage{}, err
	}
	defer cancel()
	defer rollback(tx)
	cut, err := configDocumentCutTx(ctx, tx)
	if err != nil {
		return ConfigGenerationPage{}, err
	}
	if expected != ([32]byte{}) && expected != cut.digest {
		return ConfigGenerationPage{}, &Error{kind: ConfigConflict}
	}
	rows, err := tx.Query(ctx, `SELECT g.revision,CASE WHEN octet_length(g.manifest_hash)=32 THEN g.manifest_hash END,
		g.created_at,g.entry_count,
		(SELECT count(*) FROM public.config_generation_entries e WHERE e.generation_revision=g.revision),
		EXISTS(SELECT 1 FROM public.config_generation_entries e LEFT JOIN public.config_versions v
		ON v.document_id=e.document_id AND v.revision=e.version_revision WHERE e.generation_revision=g.revision AND v.revision IS NULL)
		FROM public.config_generations g WHERE g.sealed AND ($1::bigint=0 OR g.revision<$1)
		ORDER BY g.revision DESC LIMIT $2`, before, size+1)
	if err != nil {
		return ConfigGenerationPage{}, &Error{kind: Unavailable}
	}
	defer rows.Close()
	page := ConfigGenerationPage{cut: cut}
	for rows.Next() {
		var g ConfigGenerationMetadata
		var revision int64
		var digest []byte
		var expectedEntries, entries int
		var partial bool
		if err := rows.Scan(&revision, &digest, &g.created, &expectedEntries, &entries, &partial); err != nil {
			return ConfigGenerationPage{}, &Error{kind: Unavailable}
		}
		if revision < 1 || len(digest) != sha256.Size || g.created.IsZero() || expectedEntries < 1 || expectedEntries > configSourceLimitCount || entries != expectedEntries {
			return ConfigGenerationPage{}, &Error{kind: ConfigIntegrity}
		}
		g.revision = ConfigGenerationRevision{generation: revision}
		copy(g.manifest[:], digest)
		g.availability = ConfigGenerationComplete
		if partial {
			g.availability = ConfigGenerationIncomplete
		}
		if len(page.generations) == size {
			page.more = true
			break
		}
		page.generations = append(page.generations, g)
	}
	if rows.Err() != nil {
		return ConfigGenerationPage{}, &Error{kind: Unavailable}
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return ConfigGenerationPage{}, &Error{kind: Unavailable}
	}
	return page, nil
}

type ConfigSchemaMetadata struct {
	kind            ConfigDocumentKind
	schema, profile string
}

func (s ConfigSchemaMetadata) Kind() ConfigDocumentKind { return s.kind }
func (s ConfigSchemaMetadata) SchemaVersion() string    { return s.schema }
func (s ConfigSchemaMetadata) SyntaxProfile() string    { return s.profile }

// ReadConfigSchemas reads the authoritative SQL enum catalogue. Every SQL
// schema must map to an admitted kind/profile pair; unknown labels fail closed.
// The tiny bounded catalogue is paginated by the transport, without source data.
func (s *Store) ReadConfigSchemas(ctx context.Context) ([]ConfigSchemaMetadata, error) {
	ctx, cancel, tx, err := publicConfigRead(ctx, s)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer rollback(tx)
	rows, err := tx.Query(ctx, `SELECT value::text FROM unnest(enum_range(NULL::public.config_schema_version)) value ORDER BY value::text LIMIT 17`)
	if err != nil {
		return nil, &Error{kind: Unavailable}
	}
	defer rows.Close()
	result := make([]ConfigSchemaMetadata, 0, 7)
	for rows.Next() {
		var schema string
		if err := rows.Scan(&schema); err != nil {
			return nil, &Error{kind: Unavailable}
		}
		entry, known := configPublicSchemaMetadata(schema)
		if len(result) >= 16 || !known {
			return nil, &Error{kind: ConfigIntegrity}
		}
		result = append(result, entry)
	}
	if rows.Err() != nil {
		return nil, &Error{kind: Unavailable}
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, &Error{kind: Unavailable}
	}
	return result, nil
}
