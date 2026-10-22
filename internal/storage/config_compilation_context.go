package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/jackc/pgx/v5"
)

// RegisterCompiledGenerationWithContext adds historical compiler inputs to the
// same immutable generation and ConfigTx that owns its exact source manifest.
// The projection is never an active registry or a live capability declaration.
func (c *ConfigTx) RegisterCompiledGenerationWithContext(entry string, compiled [32]byte, refs []ConfigVersionRef, compilation configcompilation.Context) (result ConfigGenerationRevision, err error) {
	defer func() { c.remember(err) }()
	if !compilation.Valid() {
		return ConfigGenerationRevision{}, &Error{kind: ConfigInvalid}
	}
	return c.registerCompiledGeneration(entry, compiled, refs, &compilation)
}

func (s *Store) ReadConfigCompilationContext(ctx context.Context, revision ConfigGenerationRevision, manifest, sourceHash [32]byte) (configcompilation.Context, error) {
	if err := bootstrapReadContextError(ctx); err != nil {
		return configcompilation.Context{}, err
	}
	if s == nil || s.pool == nil || !revision.Valid() || manifest == ([32]byte{}) || sourceHash == ([32]byte{}) {
		return configcompilation.Context{}, &Error{kind: ConfigInvalid}
	}
	var raw, hash, storedSource []byte
	err := s.pool.QueryRow(ctx, `SELECT
 CASE WHEN octet_length(compilation_context) BETWEEN 1 AND $4 THEN compilation_context END,
 compilation_context_hash,compilation_source_hash
 FROM public.config_generations WHERE revision=$1 AND manifest_hash=$2 AND candidate_hash=$3 AND sealed`, revision.Number(), manifest[:], sourceHash[:], configcompilation.MaxContextBytes).Scan(&raw, &hash, &storedSource)
	if errors.Is(err, pgx.ErrNoRows) {
		return configcompilation.Context{}, &Error{kind: ConfigNotFound}
	}
	if err != nil {
		return configcompilation.Context{}, &Error{kind: Unavailable}
	}
	if len(raw) == 0 || len(hash) != sha256.Size || !bytes.Equal(storedSource, sourceHash[:]) {
		return configcompilation.Context{}, &Error{kind: ConfigIncomplete}
	}
	value, err := configcompilation.ParseContext(raw)
	if err != nil {
		return configcompilation.Context{}, &Error{kind: ConfigIntegrity}
	}
	digest := value.SHA256()
	if !bytes.Equal(hash, digest[:]) {
		return configcompilation.Context{}, &Error{kind: ConfigIntegrity}
	}
	return value, nil
}
