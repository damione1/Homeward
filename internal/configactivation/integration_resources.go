package configactivation

import (
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/storage"
)

func recordIntegrationResources(tx *storage.ConfigTx, source config.ExactWorkspaceSource) error {
	operational, err := source.OperationalConfig()
	if err != nil {
		return err
	}
	ids, err := operational.DeclaredIntegrationInstances()
	if err != nil {
		return err
	}
	return tx.EnsureIntegrationResourceIdentities(ids)
}
