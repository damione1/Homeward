package config

import (
	"crypto/sha256"
	"errors"

	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/mqtt"
	"github.com/damione1/Homeward/internal/workday"
	"github.com/damione1/Homeward/scope"
)

// This private marker is produced at the existing pure YAML decoder, before
// operational admission can consult worker files or other runtime resources.
type yamlSourceRejection struct{}

func (*yamlSourceRejection) Error() string { return ErrInvalidYAML.Error() }
func (*yamlSourceRejection) Unwrap() error { return ErrInvalidYAML }

// WorkspaceRejection identifies a positive semantic refusal of the bytes
// actually captured by 029. Construction is private to CaptureWorkspace; a
// caller-created zero has no admitted digest. No source or parser text is public.
type WorkspaceRejection struct {
	hash     [sha256.Size]byte
	admitted bool
	cause    error
}

func (e *WorkspaceRejection) Error() string {
	return "configuration: captured operational source rejected"
}
func (e *WorkspaceRejection) GoString() string { return e.Error() }
func (e *WorkspaceRejection) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}
func (e *WorkspaceRejection) SourceSHA256() ([sha256.Size]byte, bool) {
	if e == nil || !e.admitted || e.hash == ([sha256.Size]byte{}) {
		return [sha256.Size]byte{}, false
	}
	return e.hash, true
}

func capturedWorkspaceFailure(raw []byte, err error) error {
	if len(raw) == 0 || len(raw) > maxStartupSourceBytes || !positiveWorkspaceRejection(err) {
		return err
	}
	return &WorkspaceRejection{hash: sha256.Sum256(raw), admitted: true, cause: err}
}
func positiveWorkspaceRejection(err error) bool {
	if err == nil {
		return false
	}
	var syntax *yamlSourceRejection
	if errors.As(err, &syntax) && syntax != nil {
		return true
	}
	// These validators inspect only bounded input values, never files/network.
	if errors.Is(err, ErrInvalidEnergy) || errors.Is(err, ErrInvalidDerivedPower) || errors.Is(err, ErrUnsupportedWorkdayProfile) || errors.Is(err, scope.ErrInvalidRevision) || errors.Is(err, mqtt.ErrInvalidSettings) || errors.Is(err, mqtt.ErrUnsupportedProfile) {
		return true
	}
	var profile *workday.Error
	var calendar *calendars.Error
	if errors.As(err, &profile) && profile != nil && profile.Code() == workday.InvalidProfile && errors.As(err, &calendar) && calendar != nil {
		// Workday uses the qualified embedded zone archive. In particular,
		// CapacityExceeded must not become a permanent invalid-source cache.
		return calendar.Code() == calendars.InvalidZone
	}
	// ErrInvalidYAML outside the pure decoder may hide resource failures in
	// another validator. Worker/Neviweb/Hilo settings also conflate local file
	// or ambient timezone availability with input errors. Unknown is retryable.
	return false
}
