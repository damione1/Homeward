package storage

import (
	"bytes"
	"context"
	"crypto/sha256"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/jackc/pgx/v5"
)

// DeclarativeWorkspaceLocator describes the physical namespace of the one
// complete declarative bundle. It grants no filesystem or runtime authority.
// The zero value is invalid; an admitted absence means no physical namespace
// has been admitted for this generation by the versioned workspace writer.
type DeclarativeWorkspaceLocator struct {
	admitted bool
	group    ConfigDocumentID
}

func NoDeclarativeWorkspaceLocator() DeclarativeWorkspaceLocator {
	return DeclarativeWorkspaceLocator{admitted: true}
}
func NewDeclarativeWorkspaceLocator(group ConfigDocumentID) (DeclarativeWorkspaceLocator, error) {
	if !group.Valid() {
		return DeclarativeWorkspaceLocator{}, &Error{kind: ConfigInvalid}
	}
	return DeclarativeWorkspaceLocator{admitted: true, group: group}, nil
}
func (l DeclarativeWorkspaceLocator) Valid() bool {
	return l.admitted && (l.group == (ConfigDocumentID{}) || l.group.Valid())
}
func (l DeclarativeWorkspaceLocator) Group() (ConfigDocumentID, bool) {
	return l.group, l.Valid() && l.group.Valid()
}
func (g ConfigGenerationSnapshot) DeclarativeWorkspace() DeclarativeWorkspaceLocator {
	return g.declarativeWorkspace
}

// RegisterCompiledGenerationWithWorkspace preserves the current compilation
// input, if any, and a PRESENT or explicitly ABSENT workspace locator in the
// same immutable generation transaction. A zero Context means absent input;
// a present input is still admitted through the existing context codec.
func (c *ConfigTx) RegisterCompiledGenerationWithWorkspace(entry string, compiled [32]byte, refs []ConfigVersionRef, locator DeclarativeWorkspaceLocator, compilation configcompilation.Context) (result ConfigGenerationRevision, err error) {
	defer func() { c.remember(err) }()
	if !locator.Valid() {
		return ConfigGenerationRevision{}, &Error{kind: ConfigInvalid}
	}
	var input *configcompilation.Context
	if compilation.Valid() {
		owned := compilation
		input = &owned
	}
	return c.registerCompiledGenerationAtWorkspace(entry, compiled, refs, input, locator)
}

// validateDeclarativeWorkspace also runs for incomplete retained history:
// membership/path metadata remain exact, while collected bytes stay incomplete.
func validateDeclarativeWorkspace(g ConfigGenerationSnapshot) error {
	if !g.declarativeWorkspace.Valid() {
		return &Error{kind: ConfigIntegrity}
	}
	if _, present := g.declarativeWorkspace.Group(); !present {
		return nil
	}
	if g.manifestFormat != ConfigManifestCompiledV2 {
		return &Error{kind: ConfigIntegrity}
	}
	members, entry := 0, 0
	var schema bundle.Schema
	for _, version := range g.versions {
		if version.kind != ConfigDocumentKindYAMLSource {
			continue
		}
		members++
		if version.path == g.entry {
			entry++
		}
		if !version.present {
			continue
		}
		if version.schema != bundle.SchemaDeclarative1 && version.schema != bundle.SchemaDeclarative2 || schema != bundle.SchemaInvalid && schema != version.schema {
			return &Error{kind: ConfigIntegrity}
		}
		schema = version.schema
		if schema == bundle.SchemaDeclarative1 && version.profile != parser.SyntaxProfileDeclarative || schema == bundle.SchemaDeclarative2 && version.profile != parser.SyntaxProfileDeclarative2 {
			return &Error{kind: ConfigIntegrity}
		}
	}
	if members == 0 || entry != 1 {
		return &Error{kind: ConfigIntegrity}
	}
	return nil
}

// readOptionalGenerationCompilation stays compatible with SQL24: checking a
// row's JSON keys does not reference columns introduced only by SQL26. The
// bounded typed column query is performed only when that input exists.
func readOptionalGenerationCompilation(ctx context.Context, tx pgx.Tx, generation ConfigGenerationSnapshot) (configcompilation.Context, error) {
	var present bool
	if err := tx.QueryRow(ctx, `SELECT (to_jsonb(g)->>'compilation_context') IS NOT NULL OR (to_jsonb(g)->>'compilation_context_hash') IS NOT NULL OR (to_jsonb(g)->>'compilation_source_hash') IS NOT NULL FROM public.config_generations g WHERE revision=$1 AND sealed`, generation.revision.Number()).Scan(&present); err != nil {
		return configcompilation.Context{}, &Error{kind: Unavailable}
	}
	if !present {
		for _, version := range generation.versions {
			if editableKindText(version.kind) != "" {
				return configcompilation.Context{}, &Error{kind: ConfigIncomplete}
			}
		}
		return configcompilation.Context{}, nil
	}
	var raw, hash, source []byte
	if err := tx.QueryRow(ctx, `SELECT CASE WHEN octet_length(compilation_context) BETWEEN 1 AND $2 THEN compilation_context END,compilation_context_hash,compilation_source_hash FROM public.config_generations WHERE revision=$1 AND sealed`, generation.revision.Number(), configcompilation.MaxContextBytes).Scan(&raw, &hash, &source); err != nil {
		return configcompilation.Context{}, &Error{kind: Unavailable}
	}
	if len(raw) == 0 || len(hash) != sha256.Size || !bytes.Equal(source, generation.candidateHash[:]) {
		return configcompilation.Context{}, &Error{kind: ConfigIncomplete}
	}
	input, err := configcompilation.ParseContext(raw)
	if err != nil {
		return configcompilation.Context{}, &Error{kind: ConfigIntegrity}
	}
	digest := input.SHA256()
	if !bytes.Equal(hash, digest[:]) {
		return configcompilation.Context{}, &Error{kind: ConfigIntegrity}
	}
	return input, nil
}
