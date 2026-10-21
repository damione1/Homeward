package contracts

import (
	"errors"
	wire "github.com/damione1/Homeward/gen/home/integrationadmin/v1"
	"google.golang.org/protobuf/proto"
	"strings"
	"unicode/utf8"
)

var ErrInvalidIntegrationAdminContract = errors.New("invalid integration administration contract")

func IntegrationAdminID(raw string) bool {
	return len(raw) > 0 && len(raw) <= 256 && utf8.ValidString(raw) && strings.TrimSpace(raw) != "" && !strings.ContainsAny(raw, "\x00\r\n")
}
func integrationAdminState(state wire.DesiredState) bool {
	return state == wire.DesiredState_DESIRED_STATE_ENABLED || state == wire.DesiredState_DESIRED_STATE_DISABLED
}
func integrationAdminExpected(p *wire.MutationPrecondition) bool {
	return p != nil && configurationVersion(p.Expected) && configurationHash(p.ExpectedWorkspaceSha256)
}
func integrationAdminConfiguration(c *wire.PublicConfiguration, id string) bool {
	if c == nil || !IntegrationAdminID(c.SchemaVersion) || len(c.YamlSource) < 1 || len(c.YamlSource) > 64<<10 || len(c.Secrets) > 32 {
		return false
	}
	seen := map[string]bool{}
	for _, ref := range c.Secrets {
		if ref == nil || !IntegrationAdminID(ref.Field) || ref.IntegrationInstanceId != id || !IntegrationAdminID(ref.Name) || seen[ref.Field] {
			return false
		}
		seen[ref.Field] = true
	}
	return true
}
func AdmitIntegrationAdminRequest(message proto.Message) error {
	if message == nil || !configurationKnown(message.ProtoReflect()) {
		return ErrInvalidIntegrationAdminContract
	}
	valid := false
	switch m := message.(type) {
	case *wire.ListIntegrationTypesRequest:
		valid = m != nil
	case *wire.GetIntegrationTypeRequest:
		valid = m != nil && IntegrationAdminID(m.TypeId)
	case *wire.ListInstancesRequest:
		valid = m != nil
	case *wire.GetInstanceRequest:
		valid = m != nil && IntegrationAdminID(m.IntegrationInstanceId)
	case *wire.CreateInstanceRequest:
		valid = m != nil && IntegrationAdminID(m.TypeId) && IntegrationAdminID(m.IntegrationInstanceId) && integrationAdminState(m.DesiredState) && integrationAdminConfiguration(m.Configuration, m.IntegrationInstanceId) && integrationAdminExpected(m.Precondition)
	case *wire.UpdateInstanceRequest:
		valid = m != nil && IntegrationAdminID(m.TypeId) && IntegrationAdminID(m.IntegrationInstanceId) && integrationAdminConfiguration(m.Configuration, m.IntegrationInstanceId) && integrationAdminExpected(m.Precondition)
	case *wire.DeleteInstanceRequest:
		valid = m != nil && IntegrationAdminID(m.IntegrationInstanceId) && integrationAdminExpected(m.Precondition)
	case *wire.SetInstanceEnabledRequest:
		valid = m != nil && IntegrationAdminID(m.IntegrationInstanceId) && integrationAdminState(m.DesiredState) && integrationAdminExpected(m.Precondition)
	}
	if !valid {
		return ErrInvalidIntegrationAdminContract
	}
	return nil
}
