package contracts

import (
	"errors"
	"strings"
	"unicode/utf8"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/proto"
)

var ErrInvalidIntegrationAdminContract = errors.New("invalid integration administration contract")

func IntegrationAdminID(raw string) bool {
	return len(raw) > 0 && len(raw) <= 256 && utf8.ValidString(raw) && strings.TrimSpace(raw) != "" && !strings.ContainsAny(raw, "\x00\r\n")
}
func integrationAdminState(state api.IntegrationDesiredState) bool {
	return state == api.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_ENABLED || state == api.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED
}
func integrationAdminEtag(etag string) bool {
	if len(etag) <= 4 || len(etag) > 256 || !strings.HasPrefix(etag, `W/"`) || !strings.HasSuffix(etag, `"`) || !utf8.ValidString(etag) {
		return false
	}
	for _, c := range etag[3 : len(etag)-1] {
		if c < 0x21 || c == 0x7f || c == '"' {
			return false
		}
	}
	return true
}
func integrationAdminRequestID(raw string) bool {
	if raw == "" {
		return true
	}
	_, err := apitypes.ParseRequestID(raw)
	return err == nil
}
func integrationAdminPage(pageSize int32, pageToken string) bool {
	return pageSize >= 0 && len(pageToken) <= 2048 && utf8.ValidString(pageToken) && !strings.ContainsAny(pageToken, "\x00\r\n")
}
func integrationAdminEditable(instance *api.IntegrationInstance, home apitypes.HomeName) bool {
	if instance == nil || !IntegrationAdminID(instance.InstanceKey) {
		return false
	}
	typ, err := apitypes.ParseIntegrationTypeName(instance.IntegrationType)
	if err != nil || typ.Home() != home {
		return false
	}
	cfg, ok := instance.Configuration.(*api.IntegrationInstance_Editable)
	return ok && cfg != nil && AdmitIntegrationConfiguration(cfg.Editable, instance.InstanceKey) == nil
}
func AdmitIntegrationAdminRequest(message proto.Message) error {
	if message == nil || !message.ProtoReflect().IsValid() || !configurationKnown(message.ProtoReflect()) || proto.Size(message) > 128<<10 {
		return ErrInvalidIntegrationAdminContract
	}
	valid := false
	switch m := message.(type) {
	case *api.ListIntegrationTypesRequest:
		_, err := apitypes.ParseHomeName(m.GetParent())
		valid = m != nil && err == nil && integrationAdminPage(m.PageSize, m.PageToken)
	case *api.GetIntegrationTypeRequest:
		_, err := apitypes.ParseIntegrationTypeName(m.GetName())
		valid = m != nil && err == nil
	case *api.ListIntegrationInstancesRequest:
		_, err := apitypes.ParseHomeName(m.GetParent())
		valid = m != nil && err == nil && integrationAdminPage(m.PageSize, m.PageToken)
	case *api.GetIntegrationInstanceRequest:
		_, err := apitypes.ParseIntegrationInstanceName(m.GetName())
		valid = m != nil && err == nil
	case *api.CreateIntegrationInstanceRequest:
		home, err := apitypes.ParseHomeName(m.GetParent())
		valid = m != nil && err == nil && requestedPublicResourceID(m.IntegrationInstanceId) && integrationAdminRequestID(m.RequestId) && m.IntegrationInstance != nil && integrationAdminState(m.IntegrationInstance.DesiredState) && integrationAdminEditable(m.IntegrationInstance, home)
	case *api.UpdateIntegrationInstanceRequest:
		if m != nil && m.IntegrationInstance != nil && m.UpdateMask != nil && len(m.UpdateMask.Paths) > 0 && len(m.UpdateMask.Paths) <= 8 && integrationAdminRequestID(m.RequestId) && integrationAdminEtag(m.IntegrationInstance.Etag) {
			if name, err := apitypes.ParseIntegrationInstanceName(m.IntegrationInstance.Name); err == nil {
				seen := map[string]bool{}
				wantState, wantEditable, pathsOK := false, false, true
				for _, path := range m.UpdateMask.Paths {
					if seen[path] {
						pathsOK = false
						break
					}
					seen[path] = true
					switch path {
					case "*":
						wantState, wantEditable = true, true
					case "desired_state":
						wantState = true
					case "editable":
						wantEditable = true
					default:
						pathsOK = false
					}
				}
				valid = pathsOK && (!wantState || integrationAdminState(m.IntegrationInstance.DesiredState)) && (!wantEditable || integrationAdminEditable(m.IntegrationInstance, name.Home()))
			}
		}
	case *api.DeleteIntegrationInstanceRequest:
		_, err := apitypes.ParseIntegrationInstanceName(m.GetName())
		valid = m != nil && err == nil && integrationAdminEtag(m.Etag) && integrationAdminRequestID(m.RequestId)
	}
	if !valid {
		return ErrInvalidIntegrationAdminContract
	}
	return nil
}
