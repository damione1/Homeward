package config

import (
	"path/filepath"

	"github.com/damione1/Homeward/internal/backups"
	"go.yaml.in/yaml/v3"
)

// backups is a launch resource, never an operational configuration participant.
// Absence enables local; an explicit instances: [] disables every instance.
func parseBackupProvidersInput(raw yaml.Node, dataDir string) (backups.Configuration, error) {
	defaultRoot := filepath.Join(dataDir, "backups")
	if raw.Kind == 0 {
		id, _ := backups.ParseInstanceID(backups.Local.String())
		instance, err := backups.NewLocalSettings(id, true, defaultRoot, backups.DefaultLimits())
		if err != nil {
			return backups.Configuration{}, ErrInvalidYAML
		}
		return backups.NewConfiguration([]backups.InstanceSettings{instance})
	}
	fields, err := backupFields(&raw, "instances")
	if err != nil || len(fields) != 1 {
		return backups.Configuration{}, ErrInvalidYAML
	}
	instances := fields["instances"]
	if instances == nil || instances.Kind != yaml.SequenceNode || instances.Tag != "!!seq" || len(instances.Content) > backups.MaxInstances {
		return backups.Configuration{}, ErrInvalidYAML
	}
	admitted := make([]backups.InstanceSettings, 0, len(instances.Content))
	for _, node := range instances.Content {
		fields, err := backupFields(node, "id", "provider", "enabled", backups.Local.String())
		if err != nil {
			return backups.Configuration{}, ErrInvalidYAML
		}
		idNode, kindNode := fields["id"], fields["provider"]
		if !backupString(idNode) || !backupString(kindNode) {
			return backups.Configuration{}, ErrInvalidYAML
		}
		id, err := backups.ParseInstanceID(idNode.Value)
		if err != nil {
			return backups.Configuration{}, ErrInvalidYAML
		}
		kind, err := backups.ParseProviderKind(kindNode.Value)
		if err != nil {
			return backups.Configuration{}, ErrInvalidYAML
		}
		enabled := true
		if node := fields["enabled"]; node != nil {
			if node.Kind != yaml.ScalarNode || node.Tag != "!!bool" || node.Decode(&enabled) != nil {
				return backups.Configuration{}, ErrInvalidYAML
			}
		}
		switch kind {
		case backups.Local:
			root := defaultRoot
			if node := fields[backups.Local.String()]; node != nil {
				local, err := backupFields(node, "root")
				if err != nil {
					return backups.Configuration{}, ErrInvalidYAML
				}
				if path := local["root"]; path != nil {
					if !backupString(path) {
						return backups.Configuration{}, ErrInvalidYAML
					}
					root = path.Value
				}
			}
			settings, err := backups.NewLocalSettings(id, enabled, root, backups.DefaultLimits())
			if err != nil {
				return backups.Configuration{}, ErrInvalidYAML
			}
			admitted = append(admitted, settings)
		default:
			return backups.Configuration{}, ErrInvalidYAML
		}
	}
	configuration, err := backups.NewConfiguration(admitted)
	if err != nil {
		return backups.Configuration{}, ErrInvalidYAML
	}
	return configuration, nil
}
func backupString(node *yaml.Node) bool {
	return node != nil && node.Kind == yaml.ScalarNode && node.Tag == "!!str"
}
func backupFields(node *yaml.Node, names ...string) (map[string]*yaml.Node, error) {
	if node == nil || node.Kind != yaml.MappingNode || node.Tag != "!!map" || len(node.Content)%2 != 0 {
		return nil, ErrInvalidYAML
	}
	fields := make(map[string]*yaml.Node, len(node.Content)/2)
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if !backupString(key) || fields[key.Value] != nil || value.Kind == yaml.AliasNode {
			return nil, ErrInvalidYAML
		}
		known := false
		for _, name := range names {
			if key.Value == name {
				known = true
				break
			}
		}
		if !known {
			return nil, ErrInvalidYAML
		}
		fields[key.Value] = value
	}
	return fields, nil
}
func (c Config) BackupProviders() (backups.Configuration, error) {
	if !c.backupProviders.Valid() {
		return backups.Configuration{}, ErrUninitialized
	}
	return c.backupProviders, nil
}
