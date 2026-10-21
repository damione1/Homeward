package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"

	"github.com/damione1/Homeward/internal/backups"
	backuparchive "github.com/damione1/Homeward/internal/backups/archive"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/storage"
)

// BackupArchiveServices borrows Run's registry and admitted server key. It
// creates no snapshot, scheduler, ledger or pilot. Producers supply prepared
// snapshot streams; only public recovery material enters the sealing path.
type BackupArchiveServices struct {
	providers *backups.Registry
	store     *storage.Store
	secrets   *storage.SecretRepository
	public    backuparchive.RecoveryPublic
	key       storage.BackupServerKey
	directory string
}

func (BackupArchiveServices) String() string                 { return "backup.ArchiveServices(<private>)" }
func (s BackupArchiveServices) GoString() string             { return s.String() }
func (s BackupArchiveServices) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, s.String()) }
func (s BackupArchiveServices) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }
func newBackupArchiveServices(launch config.Config, providers *backups.Registry, store *storage.Store, secrets *storage.SecretRepository) (BackupArchiveServices, error) {
	public, _, err := launch.BackupRecovery()
	if err != nil {
		return BackupArchiveServices{}, err
	}
	directory, err := launch.DataDir()
	if err != nil {
		return BackupArchiveServices{}, err
	}
	key, err := secrets.BackupKey()
	if err != nil {
		return BackupArchiveServices{}, err
	}
	return BackupArchiveServices{providers: providers, store: store, secrets: secrets, public: public, key: key, directory: filepath.Join(directory, "backup-archive-pending")}, nil
}
func (s BackupArchiveServices) Provider(instance backups.InstanceID) (backups.Provider, error) {
	if s.providers == nil {
		return nil, backups.NewError(backups.InvalidInput)
	}
	return s.providers.Instance(instance)
}
func (s BackupArchiveServices) Seal(ctx context.Context, id backups.ArtifactID, database io.Reader, size int64, yaml []backuparchive.YAMLSource, limits backuparchive.Limits) (*backuparchive.Sealed, error) {
	input, err := backuparchive.NewInput(database, size, yaml, s.key)
	if err != nil {
		return nil, err
	}
	return backuparchive.Seal(ctx, s.directory, id, s.public, input, limits)
}
