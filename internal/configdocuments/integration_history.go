package configdocuments

import (
	"context"

	"github.com/damione1/Homeward/internal/storage"
)

// ReadHistoricalOperationalSource reads the exact retained native member of a
// sealed generation. Trusted composition only: this method neither grants API
// authority nor claims publication, and never substitutes current source bytes.
func (s *Service) ReadHistoricalOperationalSource(ctx context.Context, generation storage.ConfigGenerationRevision, manifest [32]byte) (storage.ConfigVersionSnapshot, storage.ConfigProtectedSource, error) {
	if s == nil || s.store == nil || s.replay == nil || ctx == nil || !generation.Valid() || manifest == ([32]byte{}) {
		return storage.ConfigVersionSnapshot{}, storage.ConfigProtectedSource{}, &Error{kind: FailureInvalid}
	}
	snapshot, err := s.store.ReadConfigGeneration(ctx, generation)
	if err != nil {
		return storage.ConfigVersionSnapshot{}, storage.ConfigProtectedSource{}, err
	}
	if snapshot.ManifestSHA256() != manifest || snapshot.ManifestFormat() != storage.ConfigManifestCompiledV2 || snapshot.Availability() != storage.ConfigGenerationComplete {
		return storage.ConfigVersionSnapshot{}, storage.ConfigProtectedSource{}, &Error{kind: IncompleteHistory}
	}
	var selected storage.ConfigVersionSnapshot
	for _, version := range snapshot.Versions() {
		kind, present := version.Kind()
		if present && kind == storage.ConfigDocumentKindOperationalYAML {
			if selected.Ref().Valid() {
				return storage.ConfigVersionSnapshot{}, storage.ConfigProtectedSource{}, &Error{kind: IncompleteHistory}
			}
			selected = version
		}
	}
	if !selected.Ref().Valid() {
		return storage.ConfigVersionSnapshot{}, storage.ConfigProtectedSource{}, &Error{kind: IncompleteHistory}
	}
	source, err := s.replay.ReadHistoricalProtectedSource(ctx, generation, manifest, selected.Ref())
	if err != nil {
		return storage.ConfigVersionSnapshot{}, storage.ConfigProtectedSource{}, err
	}
	if source.SHA256() != selected.SHA256() {
		return storage.ConfigVersionSnapshot{}, storage.ConfigProtectedSource{}, &Error{kind: IncompleteHistory}
	}
	return selected, source, nil
}
