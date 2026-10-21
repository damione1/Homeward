package config

import (
	"github.com/damione1/Homeward/internal/backups"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
	"go.yaml.in/yaml/v3"
)

func parseS3BackupSettings(id backups.InstanceID, enabled bool, node *yaml.Node) (backups.InstanceSettings, error) {
	fields, err := backupFields(node, "endpoint", "bucket", "prefix", "region", "credential_ref")
	if err != nil || len(fields) != 5 {
		return backups.InstanceSettings{}, ErrInvalidYAML
	}
	for _, k := range []string{"endpoint", "bucket", "prefix", "region"} {
		if !backupString(fields[k]) {
			return backups.InstanceSettings{}, ErrInvalidYAML
		}
	}
	ref, err := backupFields(fields["credential_ref"], "instance", "name")
	if err != nil || len(ref) != 2 || !backupString(ref["instance"]) || !backupString(ref["name"]) {
		return backups.InstanceSettings{}, ErrInvalidYAML
	}
	instance, err := domain.ParseIntegrationInstanceID(ref["instance"].Value)
	if err != nil {
		return backups.InstanceSettings{}, ErrInvalidYAML
	}
	name, err := storage.ParseSecretName(ref["name"].Value)
	if err != nil {
		return backups.InstanceSettings{}, ErrInvalidYAML
	}
	handle, err := storage.NewSecretHandle(instance, name)
	if err != nil {
		return backups.InstanceSettings{}, ErrInvalidYAML
	}
	s, err := backups.NewS3Settings(id, enabled, fields["endpoint"].Value, fields["bucket"].Value, fields["prefix"].Value, fields["region"].Value, handle, backups.DefaultS3Limits())
	if err != nil {
		return backups.InstanceSettings{}, ErrInvalidYAML
	}
	return s, nil
}
