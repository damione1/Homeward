package configplans

import (
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/storage"
)

// capturedDeclarativeCandidate reads the sole CURRENT declarative bundle.
// Its root document is found by the persisted entry path, never by assuming
// that the workspace namespace is a document ID. A target cannot relabel the
// entry or namespace of an already localizable current bundle.
func capturedDeclarativeCandidate(target Candidate, active storage.ConfigGenerationSnapshot) (Candidate, int64, error) {
	if !target.Valid() || target.family != Declarative1 && target.family != Declarative2 {
		return Candidate{}, 0, ErrInvalid
	}
	if active.Availability() != storage.ConfigGenerationComplete {
		return Candidate{}, 0, ErrUnavailable
	}
	family, identity := target.family, target.identity
	if group, present := active.DeclarativeWorkspace().Group(); present {
		identity = group.String()
	}
	// An old unobserved bundle has no physical group identity to recover.
	// The request identity labels semantic comparison only in that case;
	// the locator remains explicitly absent until the new workspace write.
	var files []parser.SourceFile
	var revision int64
	for _, version := range active.Versions() {
		kind, present := version.Kind()
		if !present {
			return Candidate{}, 0, ErrUnavailable
		}
		if kind != storage.ConfigDocumentKindYAMLSource {
			continue
		}
		schema, present := version.Schema()
		if !present {
			return Candidate{}, 0, ErrUnavailable
		}
		selected := Declarative1
		if schema.String() == Declarative2.Schema() {
			selected = Declarative2
		} else if schema.String() != Declarative1.Schema() {
			return Candidate{}, 0, ErrUnavailable
		}
		if len(files) > 0 && family != selected {
			return Candidate{}, 0, ErrUnavailable
		}
		family = selected
		raw, present := version.Bytes()
		if !present {
			return Candidate{}, 0, ErrUnavailable
		}
		file, err := parser.NewSourceFile(version.Path(), raw)
		if err != nil {
			return Candidate{}, 0, ErrUnavailable
		}
		files = append(files, file)
		if version.Path() == active.Entry() {
			if revision != 0 {
				return Candidate{}, 0, ErrUnavailable
			}
			revision = version.Ref().Revision().Number()
		}
	}
	if len(files) == 0 {
		return Candidate{}, 0, nil
	}
	if revision == 0 {
		return Candidate{}, 0, ErrUnavailable
	}
	prior, err := NewCandidate(family, identity, active.Entry(), files)
	if err != nil {
		return Candidate{}, 0, ErrUnavailable
	}
	return prior, revision, nil
}
