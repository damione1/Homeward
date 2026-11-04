package server

import (
	"context"
	"github.com/damione1/Homeward/internal/recorderevidence"
	"github.com/damione1/Homeward/internal/storage"
)

// Explicit native-storage fixture; it is never installed in a public handler.
type nha203RecordedScopeFixture struct{}

func (nha203RecordedScopeFixture) AuthorizeRecorderEvidence(_ context.Context, e recorderevidence.Evidence) (storage.RecorderVisibility, error) {
	if !e.Present() {
		return storage.RecorderHidden, nil
	}
	return storage.RecorderVisible, nil
}
