package contracts

import (
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
)

func configurationRevisionResource(value *api.ConfigurationRevision) bool {
	if value == nil || !configurationID(value.Id) || !configurationVersion(value.Version) {
		return false
	}
	name, err := apitypes.ParseConfigurationRevisionName(value.Name)
	if err != nil || name.Revision() != value.Version.Revision {
		return false
	}
	switch value.Availability {
	case api.VersionAvailability_VERSION_AVAILABILITY_RETAINED:
		return configurationTime(value.UpdateTime) && len(value.SchemaVersion) > 0 && len(value.SchemaVersion) <= 128
	case api.VersionAvailability_VERSION_AVAILABILITY_PURGED:
		return value.UpdateTime == nil && value.SchemaVersion == ""
	default:
		return false
	}
}
func configurationGenerationResource(value *api.ConfigurationGeneration) bool {
	if value == nil || !configurationGeneration(value.Generation) || !configurationTime(value.CreateTime) {
		return false
	}
	name, err := apitypes.ParseConfigurationGenerationName(value.Name)
	if err != nil || name.Revision() != value.Generation.Revision {
		return false
	}
	return value.Availability == api.GenerationAvailability_GENERATION_AVAILABILITY_COMPLETE || value.Availability == api.GenerationAvailability_GENERATION_AVAILABILITY_PARTIALLY_PURGED
}
func configurationSchemaResource(value *api.ConfigurationSchema) bool {
	if value == nil {
		return false
	}
	if _, err := apitypes.ParseConfigurationSchemaName(value.Name); err != nil {
		return false
	}
	return configurationSchema(value.Kind, value.SchemaVersion, value.SyntaxProfile) && value.EditableOperationalWorkspace == (value.Kind == api.DocumentKind_DOCUMENT_KIND_OPERATIONAL_YAML)
}
