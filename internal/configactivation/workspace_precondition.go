package configactivation

import (
	"crypto/sha256"
	"errors"

	"github.com/damione1/Homeward/internal/storage"
)

// ErrWorkspacePreconditionConflict means no reservation or SQL transaction was
// started for this request. It cannot represent an indeterminate COMMIT.
var ErrWorkspacePreconditionConflict = errors.New("config activation: workspace precondition conflict")

// WorkspacePrecondition binds a trusted workspace mutation to the SQL authority
// it observed and to the exact candidate bytes it published. It is independent
// of the public document revision/hash CAS, which belongs to configdocuments.
type WorkspacePrecondition struct {
	generation storage.ConfigGenerationRevision
	manifest   [sha256.Size]byte
	source     [sha256.Size]byte
}

func NewWorkspacePrecondition(generation storage.ConfigGenerationRevision, manifest, source [sha256.Size]byte) (WorkspacePrecondition, error) {
	if !generation.Valid() || manifest == ([sha256.Size]byte{}) || source == ([sha256.Size]byte{}) {
		return WorkspacePrecondition{}, ErrInvalidStartup
	}
	return WorkspacePrecondition{generation: generation, manifest: manifest, source: source}, nil
}

func (p WorkspacePrecondition) valid() bool {
	return p.generation.Valid() && p.manifest != ([sha256.Size]byte{}) && p.source != ([sha256.Size]byte{})
}

// WithWorkspacePrecondition is trusted Go composition. Calling it never admits
// a human request: the existing administrative permission and lease are needed
// before the server can construct the underlying workspace request.
func (r Request) WithWorkspacePrecondition(p WorkspacePrecondition) (Request, error) {
	if !r.admitted || !p.valid() || r.workspacePrecondition != nil {
		return Request{}, ErrInvalidStartup
	}
	owned := p
	r.workspacePrecondition = &owned
	return r, nil
}

// Called by Owner.Activate after exact source capture, under its operation
// serialization, and before reserve. Ordinary 029 requests remain unchanged.
func (r Request) confirmWorkspacePrecondition(current AdmittedGeneration, prepared preparedActivation) error {
	if r.workspacePrecondition == nil {
		return nil
	}
	p := *r.workspacePrecondition
	if !p.valid() || !current.Valid() || !prepared.source.Valid() || current.Revision() != p.generation ||
		current.ManifestSHA256() != p.manifest || prepared.source.SHA256() != p.source {
		return ErrWorkspacePreconditionConflict
	}
	return nil
}
