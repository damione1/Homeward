package server

import (
	"context"
	"path/filepath"

	"github.com/damione1/Homeward/internal/backups"
	"github.com/damione1/Homeward/internal/backups/local"
	s3backup "github.com/damione1/Homeward/internal/backups/s3"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/storage"
)

func prepareBackupProviders(ctx context.Context, launch config.Config, secrets *storage.SecretRepository) (*backups.Registry, error) {
	configuration, err := launch.BackupProviders()
	if err != nil {
		return nil, err
	}
	directory, err := launch.DataDir()
	if err != nil {
		return nil, err
	}
	s3, err := s3backup.NewFactory(secrets, filepath.Join(directory, "backups-s3-pending"))
	if err != nil {
		return nil, err
	}
	return backups.NewRegistry(ctx, configuration, []backups.Factory{local.Factory{}, s3})
}
