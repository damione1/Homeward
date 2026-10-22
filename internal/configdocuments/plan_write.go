package configdocuments

import (
	"context"
	"time"

	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/storage"
)

// WriteBasis adds the optional preview's global authority cut to the ordinary
// document CAS. It carries no permission or source payload.
type WriteBasis struct {
	generation storage.ConfigGenerationRevision
	manifest   [32]byte
	catalog    [32]byte
	expires    time.Time
}

func NewWriteBasis(generation storage.ConfigGenerationRevision, manifest, catalog [32]byte, expires time.Time) (WriteBasis, error) {
	if !generation.Valid() || manifest == ([32]byte{}) || catalog == ([32]byte{}) || expires.IsZero() {
		return WriteBasis{}, &Error{kind: FailureInvalid}
	}
	return WriteBasis{generation: generation, manifest: manifest, catalog: catalog, expires: expires.Round(0).UTC()}, nil
}

func (b WriteBasis) valid() bool {
	_, err := NewWriteBasis(b.generation, b.manifest, b.catalog, b.expires)
	return err == nil
}

func (b WriteBasis) confirm(current Snapshot) error {
	if b == (WriteBasis{}) {
		return nil
	}
	if !b.valid() || current.generation != b.generation || current.manifest != b.manifest || current.catalog.SHA256() != b.catalog {
		return &Error{kind: Conflict}
	}
	return b.confirmDeadline()
}

func (b WriteBasis) confirmDeadline() error {
	if b != (WriteBasis{}) && (!b.valid() || !time.Now().Before(b.expires)) {
		return &Error{kind: Conflict}
	}
	return nil
}

// WritePlanned uses the same writer, workspace and publication operation as
// Write and watcher activation. The signed basis is checked again while both
// the document mutation and 029 operation are reserved, before staging bytes.
func (s *Service) WritePlanned(ctx context.Context, expected ExpectedDocument, raw []byte, author storage.ConfigAuthor, basis WriteBasis) (configactivation.Result, error) {
	if !expected.valid() || !author.Valid() || !basis.valid() || len(raw) < 1 || len(raw) > 1<<20 {
		return configactivation.Result{}, &Error{kind: FailureInvalid}
	}
	release, err := s.acquireMutation(ctx)
	if err != nil {
		return configactivation.Result{}, err
	}
	defer release()
	return s.writeGuardedLocked(ctx, expected, raw, author, basis)
}
