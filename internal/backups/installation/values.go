// Package installation composes the existing SQL, provider and encrypted
// archive authorities. It never starts a Host, scheduler or physical pilot.
package installation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/damione1/Homeward/internal/backups"
	backuparchive "github.com/damione1/Homeward/internal/backups/archive"
	"github.com/damione1/Homeward/internal/storage"
)

type ErrorKind uint8

const (
	Invalid ErrorKind = iota + 1
	Integrity
	Unavailable
	Conflict
	Cancelled
)

type Error struct{ kind ErrorKind }

func (e *Error) Kind() ErrorKind {
	if e == nil {
		return Unavailable
	}
	switch e.kind {
	case Invalid, Integrity, Unavailable, Conflict, Cancelled:
		return e.kind
	default:
		return Unavailable
	}
}
func (e *Error) Error() string {
	switch e.Kind() {
	case Invalid:
		return "installation: invalid input"
	case Integrity:
		return "installation: integrity failure"
	case Conflict:
		return "installation: destination unavailable"
	case Cancelled:
		return "installation: cancelled"
	default:
		return "installation: unavailable"
	}
}
func (e *Error) Is(other error) bool {
	t, ok := other.(*Error)
	return ok && e != nil && t != nil && e.Kind() == t.Kind()
}
func failure(kind ErrorKind) error          { return &Error{kind} }
func contextValid(ctx context.Context) bool { return !backups.IsNil(ctx) && ctx.Err() == nil }

type Service struct {
	store     *storage.Store
	secrets   *storage.SecretRepository
	key       storage.BackupServerKey
	public    backuparchive.RecoveryPublic
	directory string
}

func NewService(store *storage.Store, secrets *storage.SecretRepository, public backuparchive.RecoveryPublic, directory string) (*Service, error) {
	if store == nil || !secrets.Valid() || !public.Valid() || directory == "" {
		return nil, failure(Invalid)
	}
	// Reuse the existing NHA-051 owner admission before borrowing key bytes or
	// creating a directory. A valid key from another Store is not this DB key.
	if _, err := storage.NewConfigReplayRepository(store, secrets); err != nil {
		return nil, failure(Invalid)
	}
	key, err := secrets.BackupKey()
	if err != nil {
		return nil, failure(Invalid)
	}
	root, err := storage.OpenPrivateDataDir(directory)
	if err != nil {
		return nil, failure(Unavailable)
	}
	if root.Close() != nil {
		return nil, failure(Unavailable)
	}
	return &Service{store, secrets, key, public, directory}, nil
}
func (*Service) String() string                 { return "backup.Installation(<private>)" }
func (s *Service) GoString() string             { return s.String() }
func (s *Service) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, s.String()) }
func (s *Service) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

type RestoreState uint8

const (
	NotPublished RestoreState = iota + 1
	Restored
	Indeterminate
)

type Measurement struct {
	CutUTC          time.Time     `json:"cut_utc"`
	InterventionUTC time.Time     `json:"intervention_utc"`
	CompletedUTC    time.Time     `json:"completed_utc"`
	DataAge         time.Duration `json:"data_age_ns"`
	RestoreDuration time.Duration `json:"restore_duration_ns"`
}

func (m Measurement) WithinTargets() bool {
	return !m.CutUTC.IsZero() && !m.InterventionUTC.IsZero() && !m.CompletedUTC.IsZero() && m.DataAge >= 0 && m.DataAge <= time.Hour && m.RestoreDuration >= 0 && m.RestoreDuration <= 30*time.Minute
}

type RestoreResult struct {
	state       RestoreState
	generation  storage.ConfigGenerationRevision
	measurement Measurement
	staged      string
}

func (r RestoreResult) State() RestoreState                          { return r.state }
func (r RestoreResult) Generation() storage.ConfigGenerationRevision { return r.generation }
func (r RestoreResult) Measurement() Measurement                     { return r.measurement }

func (r RestoreResult) RetainedStage() string { return r.staged }
