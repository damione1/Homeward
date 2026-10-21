package server

import (
	"github.com/damione1/Homeward/internal/backups/installation"
)

// Installation borrows Run's exact Store, key repository, recovery recipient,
// and private archive directory. No second owner, scheduler or key is made.
func (s BackupArchiveServices) Installation() (*installation.Service, error) {
	return installation.NewService(s.store, s.secrets, s.public, s.directory)
}
