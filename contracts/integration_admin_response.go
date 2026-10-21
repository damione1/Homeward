package contracts

import (
	"bytes"
	"crypto/sha256"
	cfg "github.com/damione1/Homeward/gen/home/configuration/v1"
	wire "github.com/damione1/Homeward/gen/home/integrationadmin/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"google.golang.org/protobuf/proto"
)

func integrationAdminType(t *wire.IntegrationType) bool {
	if t == nil || !IntegrationAdminID(t.TypeId) || !IntegrationAdminID(t.ManifestVersion) || !IntegrationAdminID(t.SchemaVersion) || len(t.JsonSchema) < 1 || len(t.JsonSchema) > 64<<10 || len(t.CapabilitySchemaIds) > 32 {
		return false
	}
	digest := sha256.Sum256(t.JsonSchema)
	if !bytes.Equal(digest[:], t.SchemaSha256) {
		return false
	}
	seen := map[string]bool{}
	for _, id := range t.CapabilitySchemaIds {
		if !IntegrationAdminID(id) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}
func integrationAdminInstance(i *wire.IntegrationInstance) bool {
	if i == nil || !IntegrationAdminID(i.IntegrationInstanceId) || !integrationAdminState(i.DesiredState) {
		return false
	}
	switch c := i.Configuration.(type) {
	case *wire.IntegrationInstance_Editable:
		if c == nil || !IntegrationAdminID(i.TypeId) || !integrationAdminConfiguration(c.Editable, i.IntegrationInstanceId) {
			return false
		}
	case *wire.IntegrationInstance_Opaque:
		if c == nil || c.Opaque == nil || i.TypeId != "" || len(c.Opaque.Secrets) > 32 {
			return false
		}
		for _, ref := range c.Opaque.Secrets {
			if ref == nil || ref.Field != "secret_names" || ref.IntegrationInstanceId != i.IntegrationInstanceId || !IntegrationAdminID(ref.Name) {
				return false
			}
		}
	default:
		return false
	}
	return AdmitObservabilityResponse(&obs.GetIntegrationHealthResponse{Report: i.Report, Supervision: i.Supervision}) == nil
}
func integrationAdminDocument(d *cfg.ReadDocumentResponse) bool {
	return d != nil && AdmitConfigurationResponse(d) == nil
}
func integrationAdminMutation(a *cfg.ActivationResult, d *cfg.ReadDocumentResponse) bool {
	if a == nil || d != nil && !integrationAdminDocument(d) {
		return false
	}
	return AdmitConfigurationResponse(&cfg.WriteDocumentResponse{Activation: a, Current: d}) == nil
}
func AdmitIntegrationAdminResponse(message proto.Message) error {
	if message == nil || !configurationKnown(message.ProtoReflect()) || proto.Size(message) > 1<<20 {
		return ErrInvalidIntegrationAdminContract
	}
	valid := false
	switch m := message.(type) {
	case *wire.ListIntegrationTypesResponse:
		valid = m != nil && len(m.Types) <= 64
		if valid {
			seen := map[string]bool{}
			for _, t := range m.Types {
				if !integrationAdminType(t) || seen[t.TypeId] {
					valid = false
					break
				}
				seen[t.TypeId] = true
			}
		}
	case *wire.GetIntegrationTypeResponse:
		valid = m != nil && integrationAdminType(m.Type)
	case *wire.ListInstancesResponse:
		valid = m != nil && len(m.Instances) <= 1024 && integrationAdminDocument(m.Document)
		if valid {
			seen := map[string]bool{}
			for _, i := range m.Instances {
				if !integrationAdminInstance(i) || seen[i.IntegrationInstanceId] {
					valid = false
					break
				}
				seen[i.IntegrationInstanceId] = true
			}
		}
	case *wire.GetInstanceResponse:
		valid = m != nil && integrationAdminInstance(m.Instance) && integrationAdminDocument(m.Document)
	case *wire.CreateInstanceResponse:
		valid = m != nil && integrationAdminMutation(m.Activation, m.Document)
	case *wire.UpdateInstanceResponse:
		valid = m != nil && integrationAdminMutation(m.Activation, m.Document)
	case *wire.DeleteInstanceResponse:
		valid = m != nil && integrationAdminMutation(m.Activation, m.Document)
	case *wire.SetInstanceEnabledResponse:
		valid = m != nil && integrationAdminMutation(m.Activation, m.Document)
	}
	if !valid {
		return ErrInvalidIntegrationAdminContract
	}
	return nil
}
