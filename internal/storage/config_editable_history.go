package storage

import (
	"context"

	"github.com/damione1/Homeward/compat/ha/parser"
)

// ReadEditableSourceGroup resolves a group only inside the exact sealed,
// retained generation. An absent group remains absent; neither the latest
// document version nor a matching entry in another group can supply it.
func (s *Store) ReadEditableSourceGroup(ctx context.Context, generation ConfigGenerationRevision, manifest [32]byte, kind ConfigDocumentKind, identity string) ([]EditableSourceSnapshot, error) {
	if editableKindText(kind) == "" || !configLabel(identity, 255) {
		return nil, &Error{kind: ConfigInvalid}
	}
	values, err := s.ReadEditableSources(ctx, generation, manifest)
	if err != nil {
		return nil, err
	}
	return selectEditableSourceGroup(values, kind, identity)
}

func selectEditableSourceGroup(values []EditableSourceSnapshot, kind ConfigDocumentKind, identity string) ([]EditableSourceSnapshot, error) {
	if editableKindText(kind) == "" || !configLabel(identity, 255) {
		return nil, &Error{kind: ConfigInvalid}
	}
	var selected []EditableSourceSnapshot
	var descriptor SourceDescriptor
	ids := make(map[ConfigDocumentID]bool)
	paths := make(map[string]bool)
	entry, total := false, 0
	for _, value := range values {
		if !value.descriptor.Valid() {
			return nil, &Error{kind: ConfigIntegrity}
		}
		if value.descriptor.Kind() != kind || value.descriptor.Identity() != identity {
			continue
		}
		if !descriptor.Valid() {
			descriptor = value.descriptor
		}
		owned, err := parser.CloneSourceFile(value.file)
		total += len(value.file.Bytes())
		if value.descriptor != descriptor || !value.ref.Valid() || value.ref.Document() != value.binding.ID() || value.binding.Path() != value.file.Path() || configSourcePath(kind.String(), value.binding.Path()) != nil || ids[value.binding.ID()] || paths[value.binding.Path()] || len(selected) >= 64 || len(value.file.Bytes()) < 1 || len(value.file.Bytes()) > 1<<20 || total > 4<<20 || err != nil || owned.SHA256() != value.file.SHA256() {
			return nil, &Error{kind: ConfigIntegrity}
		}
		ids[value.binding.ID()], paths[value.binding.Path()] = true, true
		entry = entry || value.file.Path() == descriptor.Entry()
		value.file = owned
		selected = append(selected, value)
	}
	if len(selected) > 0 && !entry {
		return nil, &Error{kind: ConfigIntegrity}
	}
	return selected, nil
}
