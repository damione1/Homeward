package configplans

import (
	"context"
	"crypto/sha256"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/storage"
)

// RollbackDefinitions selects the exact retained declarative bundle, then
// performs a new ordinary ApplyDefinitions activation. Historical refs locate
// bytes only; expected carries CURRENT SQL versions and workspace observations.
// The historical compilation context is never read or used as runtime authority.
func (s *Service) RollbackDefinitions(ctx context.Context, family Family, identity string, expectedOperational configdocuments.ExpectedDocument, expected []configdocuments.ExpectedSource, generation storage.ConfigGenerationRevision, manifest [32]byte, author storage.ConfigAuthor) (configactivation.Result, error) {
	if !s.valid() || !usableContext(ctx) || family != Declarative1 && family != Declarative2 || !label(identity, 255) || len(expected) < 1 || len(expected) > 64 || !generation.Valid() || manifest == ([32]byte{}) || !author.Valid() {
		return configactivation.Result{}, ErrInvalid
	}
	historical, err := s.documents.Store().ReadConfigGeneration(ctx, generation)
	if err != nil {
		return configactivation.Result{}, err
	}
	if historical.Revision() != generation || historical.ManifestSHA256() != manifest {
		return configactivation.Result{}, ErrStale
	}
	candidate, err := rollbackDefinitionsFromSnapshot(family, identity, historical, expected)
	if err != nil {
		return configactivation.Result{}, err
	}
	return s.ApplyDefinitions(ctx, candidate, "", expectedOperational, expected, author)
}

type rollbackDefinitionMember struct {
	ref     storage.ConfigVersionRef
	path    string
	hash    [32]byte
	raw     []byte
	schema  bundle.Schema
	profile parser.SyntaxProfile
}

func rollbackDefinitionsFromSnapshot(family Family, identity string, historical storage.ConfigGenerationSnapshot, expected []configdocuments.ExpectedSource) (Candidate, error) {
	if historical.Availability() != storage.ConfigGenerationComplete {
		return Candidate{}, ErrUnavailable
	}
	members := make([]rollbackDefinitionMember, 0)
	for _, version := range historical.Versions() {
		kind, present := version.Kind()
		if !present {
			return Candidate{}, ErrUnavailable
		}
		switch kind {
		case storage.ConfigDocumentKindYAMLSource:
			schema, hasSchema := version.Schema()
			profile, hasProfile := version.Profile()
			raw, hasBytes := version.Bytes()
			if !hasSchema || !hasProfile || !hasBytes {
				return Candidate{}, ErrUnavailable
			}
			members = append(members, rollbackDefinitionMember{ref: version.Ref(), path: version.Path(), hash: version.SHA256(), raw: raw, schema: schema, profile: profile})
		case storage.ConfigDocumentKindOperationalYAML, storage.ConfigDocumentKindDashboardSource, storage.ConfigDocumentKindAutomationSource, storage.ConfigDocumentKindScriptSource, storage.ConfigDocumentKindSceneSource, storage.ConfigDocumentKindOwnershipSource:
			// Other admitted generation sources are retained by the current
			// writer; they never become members of the declarative rollback.
		default:
			return Candidate{}, ErrUnavailable
		}
	}
	return rollbackDefinitionCandidate(family, identity, historical.Entry(), historical.DeclarativeWorkspace(), members, expected)
}

func rollbackDefinitionCandidate(family Family, identity, entry string, locator storage.DeclarativeWorkspaceLocator, members []rollbackDefinitionMember, expected []configdocuments.ExpectedSource) (Candidate, error) {
	if family != Declarative1 && family != Declarative2 || !label(identity, 255) {
		return Candidate{}, ErrInvalid
	}
	if !locator.Valid() {
		return Candidate{}, ErrUnavailable
	}
	if group, present := locator.Group(); present && group.String() != identity {
		return Candidate{}, ErrInvalid
	}
	// For old unobserved SQL bundles, identity is the EXPLICIT request
	// namespace. No document/path/directory is interpreted as a group ID.
	if _, err := parser.NewSourceFile(entry, nil); err != nil {
		return Candidate{}, ErrUnavailable
	}
	if len(members) == 0 || len(members) > 64 {
		return Candidate{}, ErrUnavailable
	}
	if len(expected) != len(members) {
		return Candidate{}, ErrInvalid
	}
	ids := make(map[storage.ConfigDocumentID]storage.ConfigDocumentBinding, len(members))
	paths := make(map[string]bool, len(members))
	files := make([]parser.SourceFile, 0, len(members))
	total, entries := 0, 0
	for _, member := range members {
		if !member.ref.Valid() || paths[member.path] || len(member.raw) < 1 || len(member.raw) > 1<<20 || sha256.Sum256(member.raw) != member.hash {
			return Candidate{}, ErrUnavailable
		}
		if _, present := ids[member.ref.Document()]; present {
			return Candidate{}, ErrUnavailable
		}
		selected := Declarative1
		switch member.schema {
		case bundle.SchemaDeclarative1:
			if member.profile != parser.SyntaxProfileDeclarative {
				return Candidate{}, ErrUnavailable
			}
		case bundle.SchemaDeclarative2:
			selected = Declarative2
			if member.profile != parser.SyntaxProfileDeclarative2 {
				return Candidate{}, ErrUnavailable
			}
		default:
			return Candidate{}, ErrUnavailable
		}
		if selected != family {
			return Candidate{}, ErrInvalid
		}
		binding, err := storage.NewConfigDocumentBinding(member.ref.Document(), member.path)
		if err != nil {
			return Candidate{}, ErrUnavailable
		}
		file, err := parser.NewSourceFile(member.path, member.raw)
		if err != nil || file.SHA256() != member.hash {
			return Candidate{}, ErrUnavailable
		}
		total += len(member.raw)
		if total > 4<<20 {
			return Candidate{}, ErrUnavailable
		}
		if member.path == entry {
			entries++
		}
		ids[binding.ID()] = binding
		paths[member.path] = true
		files = append(files, file)
	}
	if entries != 1 {
		return Candidate{}, ErrUnavailable
	}
	for _, current := range expected {
		binding := current.Binding()
		if historical, present := ids[binding.ID()]; !present || historical != binding {
			return Candidate{}, ErrInvalid
		}
		delete(ids, binding.ID())
	}
	if len(ids) != 0 {
		return Candidate{}, ErrInvalid
	}
	return NewCandidate(family, identity, entry, files)
}
