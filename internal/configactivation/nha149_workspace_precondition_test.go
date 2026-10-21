package configactivation

import (
	"crypto/sha256"
	"testing"

	"github.com/damione1/Homeward/internal/storage"
)

func TestNHA149WorkspacePreconditionRejectsZerosAndKeepsDefault029Request(t *testing.T) {
	revision, _ := storage.NewConfigGenerationRevision(1)
	digest := sha256.Sum256([]byte("manifest"))
	source := sha256.Sum256([]byte("exact source"))
	for _, input := range []struct {
		revision         storage.ConfigGenerationRevision
		manifest, source [32]byte
	}{{storage.ConfigGenerationRevision{}, digest, source}, {revision, [32]byte{}, source}, {revision, digest, [32]byte{}}} {
		if _, err := NewWorkspacePrecondition(input.revision, input.manifest, input.source); err == nil {
			t.Fatal("zero workspace precondition admitted")
		}
	}
	precondition, err := NewWorkspacePrecondition(revision, digest, source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (Request{}).WithWorkspacePrecondition(precondition); err == nil {
		t.Fatal("unadmitted request acquired precondition")
	}
	author, _ := storage.NewConfigAuthor("nha149:test")
	origin, _ := storage.NewConfigSource("nha149:test")
	request, err := NewWorkspaceRequest(RetainDefinitions(), author, origin, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := request.confirmWorkspacePrecondition(AdmittedGeneration{}, preparedActivation{}); err != nil {
		t.Fatal("default029 request was changed", err)
	}
	guarded, err := request.WithWorkspacePrecondition(precondition)
	if err != nil {
		t.Fatal(err)
	}
	if request.workspacePrecondition != nil {
		t.Fatal("value conversion mutated original request")
	}
	if _, err := guarded.WithWorkspacePrecondition(precondition); err == nil {
		t.Fatal("precondition silently replaced")
	}
	if err := guarded.confirmWorkspacePrecondition(AdmittedGeneration{}, preparedActivation{}); err != ErrWorkspacePreconditionConflict {
		t.Fatal("invalid current/candidate did not fail as pre-COMMIT conflict", err)
	}
}
