package storage

import (
	"context"
	"github.com/damione1/Homeward/internal/recorderevidence"
)

// RecorderVisibility is a closed decision from the existing authority. Missing
// provenance is never submitted to the authority and is always invisible.
type RecorderVisibility uint8

const (
	RecorderVisibilityUnspecified RecorderVisibility = iota
	RecorderVisible
	RecorderHidden
)

type RecorderScopeAuthorizer interface {
	AuthorizeRecorderEvidence(context.Context, recorderevidence.Evidence) (RecorderVisibility, error)
}
