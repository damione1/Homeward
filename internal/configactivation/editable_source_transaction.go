package configactivation

import (
	"context"

	"github.com/damione1/Homeward/internal/storage"
)

// WithEditableSourceGroup binds semantic compilation to the exact replacement
// bytes. The owner verifies the complete resulting set again during prepare;
// a caller cannot supply an opaque ProgramImage for unrelated durable sources.
func (r Request) WithEditableSourceGroup(group SourceGroup, compilation SourceCompilation) (Request, error) {
	if !r.admitted || r.sourceReplacement != nil || r.sourceCompilation != nil || !group.valid() || !compilation.Valid() {
		return Request{}, ErrInvalidStartup
	}
	present := false
	for _, compiled := range compilation.groups {
		if compiled.descriptor == group.descriptor && compiled.hash == group.hash {
			present = true
		}
	}
	if !present {
		return Request{}, ErrInvalidStartup
	}
	ownedGroup, ownedCompilation := group, compilation
	r.sourceReplacement, r.sourceCompilation = &ownedGroup, &ownedCompilation
	return r, nil
}

func editableGroupSetHash(groups []SourceGroup) [32]byte {
	// CompileSourceGroups uses the same ordered group transcript. Rebuilding
	// this transcript cannot produce semantic admission; it only confirms that
	// the already-admitted compilation belongs to the exact prepared source set.
	return sourceCompilationDigest(groups)
}

func (o *Owner) prepareEditableSources(ctx context.Context, current AdmittedGeneration, prepared preparedActivation, request Request) (preparedActivation, error) {
	groups, err := replaceSourceGroup(current.extraGroups, request.sourceReplacement)
	if err != nil {
		return preparedActivation{}, err
	}
	if request.sourceReplacement != nil {
		if request.sourceCompilation == nil || !request.sourceCompilation.Valid() || request.sourceCompilation.hash != editableGroupSetHash(groups) {
			return preparedActivation{}, ErrInvalidStartup
		}
	}
	prepared.extraGroups, prepared.sourceReplacement = groups, request.sourceReplacement
	prepared.sourceContext = request.sourceContext
	if len(groups) == 0 {
		return prepared, nil
	}
	active, present, err := o.input.Store.ReadActiveConfig(ctx)
	if err != nil || !present || active.Revision() != current.Revision() || active.ManifestSHA256() != current.ManifestSHA256() {
		return preparedActivation{}, ErrStartupConflict
	}
	replacing := map[storage.ConfigDocumentID]bool{}
	if request.sourceReplacement != nil {
		for _, group := range current.extraGroups {
			if group.descriptor.Kind() == request.sourceReplacement.descriptor.Kind() && group.descriptor.Identity() == request.sourceReplacement.descriptor.Identity() {
				for _, binding := range group.bindings {
					replacing[binding.ID()] = true
				}
			}
		}
	}
	for _, version := range active.Versions() {
		kind, retained := version.Kind()
		if !retained {
			return preparedActivation{}, ErrStartupIntegrity
		}
		switch kind {
		case storage.ConfigDocumentKindDashboardSource, storage.ConfigDocumentKindAutomationSource, storage.ConfigDocumentKindScriptSource, storage.ConfigDocumentKindSceneSource, storage.ConfigDocumentKindOwnershipSource:
			if !replacing[version.Ref().Document()] {
				prepared.extraRefs = append(prepared.extraRefs, version.Ref())
			}
		}
	}
	count, total := 1+len(prepared.legacy.Sources()), len(prepared.source.Bytes())
	for _, file := range prepared.legacy.Sources() {
		total += len(file.Bytes())
	}
	for _, group := range groups {
		count += len(group.files)
		for _, file := range group.files {
			total += len(file.Bytes())
		}
	}
	if count > 256 || total > 4<<20 {
		return preparedActivation{}, ErrInvalidStartup
	}
	prepared.compiled = compiledSourceGroupsHash(prepared.compiled, groups)
	return prepared, nil
}

func (p preparedActivation) appendEditableRefs(tx *storage.ConfigTx, refs []storage.ConfigVersionRef) ([]storage.ConfigVersionRef, error) {
	result := append(refs, p.extraRefs...)
	if p.sourceReplacement == nil {
		return result, nil
	}
	byPath := map[string]storage.ConfigDocumentBinding{}
	for _, binding := range p.sourceReplacement.bindings {
		byPath[binding.Path()] = binding
	}
	for _, file := range p.sourceReplacement.files {
		binding, present := byPath[file.Path()]
		if !present {
			return nil, ErrInvalidStartup
		}
		ref, err := tx.SaveEditableSource(p.sourceReplacement.descriptor, binding, file, p.metadata)
		if err != nil {
			return nil, err
		}
		result = append(result, ref)
	}
	return result, nil
}
