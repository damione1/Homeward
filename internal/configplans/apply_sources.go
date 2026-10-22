package configplans

import (
	"context"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/storage"
	"strings"
)

func editableDescriptor(candidate Candidate) (storage.SourceDescriptor, error) {
	switch candidate.family {
	case Dashboard:
		return storage.DashboardSourceDescriptor(candidate.identity, candidate.entry)
	case Automation:
		return storage.AutomationSourceDescriptor(candidate.identity, candidate.entry)
	case Script:
		return storage.ScriptSourceDescriptor(candidate.identity, candidate.entry)
	case Scene:
		return storage.SceneSourceDescriptor(candidate.identity, candidate.entry)
	case Ownership:
		return storage.OwnershipSourceDescriptor(candidate.identity, candidate.entry)
	default:
		return storage.SourceDescriptor{}, ErrInvalid
	}
}
func editableFamily(family Family) bool {
	return family == Dashboard || family == Automation || family == Script || family == Scene || family == Ownership
}

// ApplySources uses the same source-set writer as declarative definitions. The
// trusted runtime owner recompiles these bytes against its executable ports
// before any filesystem staging; a preview never supplies execution authority.
func (s *Service) ApplySources(ctx context.Context, candidate Candidate, receipt string, expectedOperational configdocuments.ExpectedDocument, expected []configdocuments.ExpectedSource, author storage.ConfigAuthor) (configactivation.Result, error) {
	if !candidate.Valid() || !editableFamily(candidate.family) || len(expected) != len(candidate.files) {
		return configactivation.Result{}, ErrInvalid
	}
	_, basis, err := s.applyBasis(ctx, candidate, receipt, author)
	if err != nil {
		return configactivation.Result{}, err
	}
	descriptor, err := editableDescriptor(candidate)
	if err != nil {
		return configactivation.Result{}, ErrInvalid
	}
	bindings := make([]storage.ConfigDocumentBinding, 0, len(expected))
	for _, version := range expected {
		bindings = append(bindings, version.Binding())
	}
	group, err := configactivation.NewSourceGroup(descriptor, candidate.files, bindings)
	if err != nil {
		return configactivation.Result{}, ErrInvalid
	}
	groupID, err := configdocuments.SourceWorkspaceGroup(descriptor)
	if err != nil {
		return configactivation.Result{}, ErrInvalid
	}
	return s.documents.WriteSourceGroup(ctx, groupID, group, expectedOperational, expected, author, basis)
}

func (s *Service) capturedEditableSources(ctx context.Context, candidate Candidate, active storage.ConfigGenerationSnapshot) ([]parser.SourceFile, int64, string, error) {
	descriptor, err := editableDescriptor(candidate)
	if err != nil {
		return nil, 0, "", ErrInvalid
	}
	sources, err := s.documents.Store().ReadEditableSourceGroup(ctx, active.Revision(), active.ManifestSHA256(), descriptor.Kind(), descriptor.Identity())
	if err != nil {
		return nil, 0, "", err
	}
	var files []parser.SourceFile
	var revision int64
	var entry string
	for _, source := range sources {
		// ReadEditableSourceGroup has already rejected mixed descriptors.
		entry = source.Descriptor().Entry()
		files = append(files, source.File())
		if source.File().Path() == entry {
			revision = source.Ref().Revision().Number()
		}
	}
	if len(files) > 0 && revision == 0 {
		return nil, 0, "", ErrUnavailable
	}
	return files, revision, entry, nil
}

// Native ownership JSON remains a separate closed snapshot format. Existing
// YAML candidates keep their grammar; no parser or proto contract is widened.
func newFamilySource(family Family, path string, raw []byte) (parser.SourceFile, error) {
	if family == Ownership && strings.HasSuffix(path, ".json") {
		return parser.NewJSONSourceFile(path, raw)
	}
	return parser.NewSourceFile(path, raw)
}
