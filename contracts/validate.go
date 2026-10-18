// Package contracts contains pure wire-boundary validation, with no dispatcher or transport.
package contracts

import (
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"unicode/utf8"
)

// ValidateCommand demonstrates admission requirements. Authentication/authorization,
// expiry, identity resolution and physical execution remain responsibilities of later services.
func ValidateCommand(c *actions.CommandRequest, epoch string, revision uint64) *actions.ContractError {
	if err := validateEnvelope(c, epoch, revision); err != nil {
		return err
	}
	if c.CapabilitySchemaId != SwitchSchema {
		return commandError(actions.ErrorCode_ERROR_CODE_UNSUPPORTED_CAPABILITY, "capability_schema_id", "command rejected by contract")
	}
	if !validArguments(c.Arguments) {
		return commandError(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "arguments", "command rejected by contract")
	}
	return nil
}

func commandError(code actions.ErrorCode, path, message string) *actions.ContractError {
	return &actions.ContractError{Code: code, FieldPath: path, Message: message}
}

func validateEnvelope(c *actions.CommandRequest, epoch string, revision uint64) *actions.ContractError {
	fail := func(code actions.ErrorCode, path string) *actions.ContractError {
		return commandError(code, path, "command rejected by contract")
	}
	if c == nil || c.CommandId == "" {
		return fail(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "command_id")
	}
	if c.AdmissionEpoch == "" || epoch == "" {
		return fail(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "admission_epoch")
	}
	if c.AdmissionEpoch != epoch {
		return fail(actions.ErrorCode_ERROR_CODE_STALE_EPOCH, "admission_epoch")
	}
	if c.ConfigRevision != revision {
		return fail(actions.ErrorCode_ERROR_CODE_STALE_CONFIGURATION, "config_revision")
	}
	if !validTarget(c.Target) {
		return fail(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "target")
	}
	return nil
}

func validArguments(args *actions.ActionArgs) bool {
	if args == nil {
		return false
	}
	switch value := args.Arguments.(type) {
	case *actions.ActionArgs_SetSwitch:
		return value != nil && value.SetSwitch != nil
	case *actions.ActionArgs_Extension:
		return false
	case *actions.ActionArgs_SetLight, *actions.ActionArgs_SetFan, *actions.ActionArgs_SetClimate:
		return false
	default:
		return false
	}
}

func validTarget(target *registry.EntityRef) bool {
	if target == nil {
		return false
	}
	switch ref := target.Reference.(type) {
	case *registry.EntityRef_EntityId:
		if ref == nil {
			return false
		}
		_, err := domain.ParseEntityID(ref.EntityId)
		return err == nil
	case *registry.EntityRef_HaAlias:
		return ref != nil && ref.HaAlias != "" && utf8.ValidString(ref.HaAlias)
	default:
		// Unknown wire branches remain decodable, but cannot be admitted here.
		return false
	}
}
