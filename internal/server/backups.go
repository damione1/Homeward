package server

import (
	"context"

	"github.com/damione1/Homeward/internal/backups"
	"github.com/damione1/Homeward/internal/backups/local"
	"github.com/damione1/Homeward/internal/config"
)

func prepareBackupProviders(ctx context.Context, launch config.Config) (*backups.Registry, error) {
	configuration, err := launch.BackupProviders()
	if err != nil {
		return nil, err
	}
	return backups.NewRegistry(ctx, configuration, []backups.Factory{local.Factory{}})
}
