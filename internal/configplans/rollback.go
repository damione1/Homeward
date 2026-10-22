package configplans

import (
	"context"

	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/storage"
)

// Rollback restores a retained editable document through the existing CAS and
// 029 pipeline. It never rewinds a revision, replays a run or inverts commands.
// RestoreDocument requires a complete historical generation before any write.
func (s *Service) Rollback(ctx context.Context, expected configdocuments.ExpectedDocument, generation storage.ConfigGenerationRevision, manifest [32]byte, author storage.ConfigAuthor) (configactivation.Result, error) {
	if !s.valid() || !usableContext(ctx) || !generation.Valid() || manifest == ([32]byte{}) || !author.Valid() {
		return configactivation.Result{}, ErrInvalid
	}
	return s.documents.RestoreDocument(ctx, expected, generation, manifest, author)
}
