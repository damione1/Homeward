package server

import (
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/integrationadmin"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// The selection is evaluated against the captured original definition. It does
// not bypass the instance etag or either native source/workspace precondition.
func integrationUpdateSelection(value, original *api.IntegrationInstance, mask *fieldmaskpb.FieldMask) (integrationadmin.UpdateSelection, error) {
	if value == nil || original == nil || value.Name != original.Name {
		return 0, integrationadmin.ErrInvalid
	}
	paths := []string{}
	if mask == nil {
		// Absent mask selects populated writable/immutable fields; a present
		// empty mask is an explicit no-op (still subject to native CAS).
		if value.DesiredState != api.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_UNSPECIFIED {
			paths = append(paths, "desired_state")
		}
		if value.GetEditable() != nil {
			paths = append(paths, "editable")
		}
		if value.InstanceKey != "" {
			paths = append(paths, "instance_key")
		}
		if value.IntegrationType != "" {
			paths = append(paths, "integration_type")
		}
	} else {
		paths = mask.Paths
	}
	configuration, desired := false, false
	for _, path := range paths {
		switch path {
		case "*":
			if len(paths) != 1 {
				return 0, integrationadmin.ErrInvalid
			}
			// The opaque arm is output-only. Read/modify/write of an external
			// worker can select its desired state but never its executable.
			configuration = original.GetEditable() != nil || value.GetEditable() != nil
			desired = true
			if value.InstanceKey != "" && value.InstanceKey != original.InstanceKey || value.IntegrationType != "" && value.IntegrationType != original.IntegrationType {
				return 0, integrationadmin.ErrInvalid
			}
		case "editable":
			configuration = true
		case "desired_state":
			desired = true
		case "instance_key":
			if value.InstanceKey != original.InstanceKey {
				return 0, integrationadmin.ErrInvalid
			}
		case "integration_type":
			if value.IntegrationType != original.IntegrationType {
				return 0, integrationadmin.ErrInvalid
			}
		case "name", "etag", "configuration_document", "opaque", "health_report", "no_health_report", "supervision", "not_supervised":
			// Identity/etag are checked separately, and output-only fields do
			// not participate in the native mutation or its fingerprint.
		default:
			return 0, integrationadmin.ErrInvalid
		}
	}
	switch {
	case configuration && desired:
		return integrationadmin.UpdateConfigurationAndDesiredState, nil
	case configuration:
		return integrationadmin.UpdateConfiguration, nil
	case desired:
		return integrationadmin.UpdateDesiredState, nil
	default:
		return integrationadmin.UpdateNothing, nil
	}
}
