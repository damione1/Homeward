// Package contracts contains pure wire-boundary validation, with no dispatcher or transport.
package contracts

import (
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
)

// ValidateCommand demonstrates admission requirements. Authentication/authorization,
// expiry, identity resolution and physical execution remain responsibilities of later services.
func ValidateCommand(c *actions.CommandRequest, epoch string, revision uint64) *actions.ContractError {
	fail := func(code actions.ErrorCode, path string) *actions.ContractError {
		return &actions.ContractError{Code: code, FieldPath: path, Message: "command rejected by contract"}
	}
	if c == nil || c.CommandId == "" {
		return fail(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "command_id")
	}
	if c.AdmissionEpoch != epoch {
		return fail(actions.ErrorCode_ERROR_CODE_STALE_EPOCH, "admission_epoch")
	}
	if c.ConfigRevision != revision {
		return fail(actions.ErrorCode_ERROR_CODE_STALE_CONFIGURATION, "config_revision")
	}
	if c.Target == nil || c.Target.Reference == nil || (c.Target.GetEntityId() == "" && c.Target.GetHaAlias() == "") {
		return fail(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "target")
	}
	if c.CapabilitySchemaId != "core.switch.v1" {
		return fail(actions.ErrorCode_ERROR_CODE_UNSUPPORTED_CAPABILITY, "capability_schema_id")
	}
	if c.Arguments == nil || c.Arguments.GetSetSwitch() == nil {
		return fail(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "arguments")
	}
	return nil
}
