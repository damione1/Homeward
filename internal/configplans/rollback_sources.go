package configplans

import (
	"context"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/storage"
)

// RollbackSources selects one complete source group from the exact retained
// generation. Historical revisions identify the bytes to restore; expected
// still describes the CURRENT SQL document versions and workspace files.
// ApplySources performs fresh compilation/admission and creates a new Owner
// activation. No historical compilation context grants runtime authority.
func (s *Service) RollbackSources(ctx context.Context, family Family, identity string, expectedOperational configdocuments.ExpectedDocument, expected []configdocuments.ExpectedSource, generation storage.ConfigGenerationRevision, manifest [32]byte, author storage.ConfigAuthor) (configactivation.Result, error) {
	if !s.valid() || !usableContext(ctx) || !editableFamily(family) || !label(identity, 255) || !generation.Valid() || manifest == ([32]byte{}) || !author.Valid() {
		return configactivation.Result{}, ErrInvalid
	}
	values, err := s.documents.Store().ReadEditableSourceGroup(ctx, generation, manifest, rollbackSourceKind(family), identity)
	if err != nil {
		return configactivation.Result{}, err
	}
	members := make([]rollbackSourceMember, 0, len(values))
	for _, value := range values {
		members = append(members, rollbackSourceMember{descriptor: value.Descriptor(), binding: value.Binding(), ref: value.Ref(), file: value.File()})
	}
	candidate, err := rollbackSourceCandidate(family, identity, members, expected)
	if err != nil {
		return configactivation.Result{}, err
	}
	return s.ApplySources(ctx, candidate, "", expectedOperational, expected, author)
}

// This private projection keeps the exact historical ref alongside its group
// metadata until selection is complete. It is never an activation capability.
type rollbackSourceMember struct {
	descriptor storage.SourceDescriptor
	binding    storage.ConfigDocumentBinding
	ref        storage.ConfigVersionRef
	file       parser.SourceFile
}

func rollbackSourceKind(family Family) storage.ConfigDocumentKind {
	switch family {
	case Dashboard:
		return storage.ConfigDocumentKindDashboardSource
	case Automation:
		return storage.ConfigDocumentKindAutomationSource
	case Script:
		return storage.ConfigDocumentKindScriptSource
	case Scene:
		return storage.ConfigDocumentKindSceneSource
	case Ownership:
		return storage.ConfigDocumentKindOwnershipSource
	default:
		return storage.ConfigDocumentKindInvalid
	}
}

func rollbackSourceCandidate(family Family, identity string, members []rollbackSourceMember, expected []configdocuments.ExpectedSource) (Candidate, error) {
	kind := rollbackSourceKind(family)
	if kind == storage.ConfigDocumentKindInvalid || !label(identity, 255) {
		return Candidate{}, ErrInvalid
	}
	var descriptor storage.SourceDescriptor
	files := make([]parser.SourceFile, 0)
	bindings := make([]storage.ConfigDocumentBinding, 0)
	for _, member := range members {
		if !member.descriptor.Valid() {
			return Candidate{}, ErrUnavailable
		}
		if member.descriptor.Kind() != kind || member.descriptor.Identity() != identity {
			continue
		}
		if !descriptor.Valid() {
			descriptor = member.descriptor
		}
		if member.descriptor != descriptor || !member.ref.Valid() || member.ref.Document() != member.binding.ID() {
			return Candidate{}, ErrUnavailable
		}
		files = append(files, member.file)
		bindings = append(bindings, member.binding)
	}
	if len(files) == 0 {
		// An absent historical group cannot fall back to embedded defaults,
		// another group with the same entry path, or a recent SQL version.
		return Candidate{}, ErrUnavailable
	}
	if _, err := configactivation.NewSourceGroup(descriptor, files, bindings); err != nil {
		return Candidate{}, ErrUnavailable
	}
	if len(expected) != len(bindings) {
		return Candidate{}, ErrInvalid
	}
	byID := make(map[storage.ConfigDocumentID]storage.ConfigDocumentBinding, len(bindings))
	for _, binding := range bindings {
		byID[binding.ID()] = binding
	}
	for _, current := range expected {
		binding := current.Binding()
		if historical, present := byID[binding.ID()]; !present || historical != binding {
			return Candidate{}, ErrInvalid
		}
		delete(byID, binding.ID())
	}
	if len(byID) != 0 {
		return Candidate{}, ErrInvalid
	}
	return NewCandidate(family, identity, descriptor.Entry(), files)
}
