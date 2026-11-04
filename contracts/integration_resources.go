package contracts

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/proto"
)

var ErrInvalidIntegrationResource = errors.New("invalid integration resource")

// AdmitIntegrationRequestShape rejects unknown protocol fields and bounds the
// request before copying its intent. Semantic diagnostics follow authorization
// in the resource handler; output-only known fields are ignored by mutations.
func AdmitIntegrationRequestShape(value proto.Message) error {
	if value == nil || !value.ProtoReflect().IsValid() || !configurationKnown(value.ProtoReflect()) || proto.Size(value) > 128<<10 {
		return ErrInvalidIntegrationResource
	}
	switch value.(type) {
	case *api.GetIntegrationTypeRequest, *api.ListIntegrationTypesRequest, *api.GetIntegrationInstanceRequest, *api.ListIntegrationInstancesRequest, *api.CreateIntegrationInstanceRequest, *api.UpdateIntegrationInstanceRequest, *api.DeleteIntegrationInstanceRequest:
		return nil
	default:
		return ErrInvalidIntegrationResource
	}
}

func AdmitIntegrationConfiguration(value *api.IntegrationConfiguration, instanceKey string) error {
	if value == nil || !configurationKnown(value.ProtoReflect()) || !IntegrationAdminID(instanceKey) || !IntegrationAdminID(value.SchemaVersion) || len(value.YamlSource) < 1 || len(value.YamlSource) > 64<<10 || !utf8.Valid(value.YamlSource) || !integrationResourceSecrets(value.Secrets, instanceKey, false) {
		return ErrInvalidIntegrationResource
	}
	return nil
}

func AdmitIntegrationType(value *api.IntegrationType, home apitypes.HomeName) error {
	if value == nil || !home.Valid() || !configurationKnown(value.ProtoReflect()) || proto.Size(value) > 256<<10 || !IntegrationAdminID(value.TypeKey) || !IntegrationAdminID(value.ManifestVersion) || !IntegrationAdminID(value.SchemaVersion) || len(value.JsonSchema) == 0 || len(value.JsonSchema) > 192<<10 || !json.Valid(value.JsonSchema) || len(value.SchemaSha256) != 32 || len(value.CapabilitySchemaIds) > 64 {
		return ErrInvalidIntegrationResource
	}
	name, err := apitypes.ParseIntegrationTypeName(value.Name)
	if err != nil || name.Home() != home {
		return ErrInvalidIntegrationResource
	}
	digest := sha256.Sum256(value.JsonSchema)
	if !bytes.Equal(digest[:], value.SchemaSha256) {
		return ErrInvalidIntegrationResource
	}
	seen := make(map[string]bool, len(value.CapabilitySchemaIds))
	for _, id := range value.CapabilitySchemaIds {
		if !IntegrationAdminID(id) || seen[id] {
			return ErrInvalidIntegrationResource
		}
		seen[id] = true
	}
	return nil
}

func integrationResourceSecrets(values []*api.IntegrationSecretReference, key string, opaque bool) bool {
	if len(values) > 32 {
		return false
	}
	seen := map[string]bool{}
	for _, value := range values {
		if value == nil || !IntegrationAdminID(value.Field) || value.InstanceKey != key || !IntegrationAdminID(value.SecretKey) {
			return false
		}
		identity := value.Field
		if opaque {
			if value.Field != "secret_names" {
				return false
			}
			identity = value.SecretKey
		}
		if seen[identity] {
			return false
		}
		seen[identity] = true
	}
	return true
}

// AdmitIntegrationInstance admits a redacted resource, not editable native
// source authority. Missing observation arms mean not included, never READY.
func AdmitIntegrationInstance(value *api.IntegrationInstance, home apitypes.HomeName) error {
	invalid := ErrInvalidIntegrationResource
	if value == nil || !home.Valid() || proto.Size(value) > 128<<10 || !configurationKnown(value.ProtoReflect()) || !IntegrationAdminID(value.InstanceKey) {
		return invalid
	}
	name, err := apitypes.ParseIntegrationInstanceName(value.Name)
	if err != nil || name.Home() != home {
		return invalid
	}
	document, err := apitypes.ParseConfigurationDocumentName(value.ConfigurationDocument)
	if err != nil || document.Home() != home {
		return invalid
	}
	if len(value.Etag) > 256 || !strings.HasPrefix(value.Etag, `W/"`) || !strings.HasSuffix(value.Etag, `"`) || len(value.Etag) <= 4 || !utf8.ValidString(value.Etag) {
		return invalid
	}
	for _, c := range value.Etag[3 : len(value.Etag)-1] {
		if c < 0x21 || c == 0x7f || c == '"' {
			return invalid
		}
	}
	switch value.DesiredState {
	case api.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_ENABLED, api.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED:
	default:
		return invalid
	}
	switch config := value.Configuration.(type) {
	case *api.IntegrationInstance_Editable:
		typ, err := apitypes.ParseIntegrationTypeName(value.IntegrationType)
		if err != nil || typ.Home() != home || config == nil || config.Editable == nil {
			return invalid
		}
		c := config.Editable
		if !IntegrationAdminID(c.SchemaVersion) || len(c.YamlSource) < 1 || len(c.YamlSource) > 64<<10 || !utf8.Valid(c.YamlSource) || !integrationResourceSecrets(c.Secrets, value.InstanceKey, false) {
			return invalid
		}
	case *api.IntegrationInstance_Opaque:
		if config == nil || config.Opaque == nil || value.IntegrationType != "" || !integrationResourceSecrets(config.Opaque.Secrets, value.InstanceKey, true) {
			return invalid
		}
	default:
		return invalid
	}
	switch report := value.ObservedHealth.(type) {
	case nil:
	case *api.IntegrationInstance_NoHealthReport:
		if report == nil || report.NoHealthReport == nil {
			return invalid
		}
	case *api.IntegrationInstance_HealthReport:
		if report == nil || report.HealthReport == nil {
			return invalid
		}
		switch report.HealthReport.Status {
		case api.IntegrationHealthStatus_INTEGRATION_HEALTH_STATUS_READY, api.IntegrationHealthStatus_INTEGRATION_HEALTH_STATUS_DEGRADED, api.IntegrationHealthStatus_INTEGRATION_HEALTH_STATUS_UNAVAILABLE:
		default:
			return invalid
		}
		switch admission := report.HealthReport.Admission.(type) {
		case *api.IntegrationHealthReport_AdmissionTime:
			if admission == nil || admission.AdmissionTime == nil || admission.AdmissionTime.CheckValid() != nil {
				return invalid
			}
		case *api.IntegrationHealthReport_AdmissionNotRecorded:
			if admission == nil || admission.AdmissionNotRecorded == nil {
				return invalid
			}
		default:
			return invalid
		}
	default:
		return invalid
	}
	switch supervision := value.ObservedSupervision.(type) {
	case nil:
	case *api.IntegrationInstance_NotSupervised:
		if supervision == nil || supervision.NotSupervised == nil {
			return invalid
		}
	case *api.IntegrationInstance_Supervision:
		if supervision == nil || supervision.Supervision == nil || supervision.Supervision.Attempts < 0 || supervision.Supervision.Tasks < 0 {
			return invalid
		}
		switch supervision.Supervision.Lifecycle {
		case api.IntegrationLifecycle_INTEGRATION_LIFECYCLE_STARTING, api.IntegrationLifecycle_INTEGRATION_LIFECYCLE_RUNNING, api.IntegrationLifecycle_INTEGRATION_LIFECYCLE_RETRYING, api.IntegrationLifecycle_INTEGRATION_LIFECYCLE_DEGRADED, api.IntegrationLifecycle_INTEGRATION_LIFECYCLE_STOPPED:
		default:
			return invalid
		}
	default:
		return invalid
	}
	return nil
}
