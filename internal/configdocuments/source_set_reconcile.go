package configdocuments

import (
	"context"

	"github.com/damione1/Homeward/internal/storage"
)

// reconcileSourceSetLocked borrows the existing149 mutation reservation. It
// neither opens another writer nor attempts to resolve an unknown 029 COMMIT.
// The caller must obtain an authoritative owner stamp first; the second read
// proves the catalogue/owner cut stayed identical while SQL members were read.
func (s *Service) reconcileSourceSetLocked(ctx context.Context) (bool, error) {
	ids, present, err := s.workspace.PendingSetDocuments()
	if err != nil || !present {
		return present, err
	}
	before, err := s.Read(ctx, operationalID())
	if err != nil {
		return true, err
	}
	active, present, err := s.store.ReadActiveConfig(ctx)
	if err != nil || !present || active.Revision() != before.generation || active.ManifestSHA256() != before.manifest || active.Availability() != storage.ConfigGenerationComplete {
		return true, &Error{kind: ReconciliationRequired}
	}
	admitted, err := s.selectedDocumentAuthorities(ctx, active, ids)
	if err != nil {
		return true, &Error{kind: ReconciliationRequired}
	}

	after, err := s.Read(ctx, operationalID())
	if err != nil {
		return true, err
	}
	if before.generation != after.generation || before.manifest != after.manifest || before.catalog != after.catalog || before.observation.hash != after.observation.hash {
		return true, &Error{kind: ReconciliationRequired}
	}
	return s.workspace.ReconcileSet(admitted)
}
