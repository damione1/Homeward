// Package contracts contains pure wire-boundary validation, with no dispatcher or transport.
package contracts

import (
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	helperwire "github.com/damione1/Homeward/gen/home/helpers/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"math"
	"unicode/utf8"
)

// ValidateCommand demonstrates admission requirements. Authentication/authorization,
// expiry, identity resolution and physical execution remain responsibilities of later services.
func ValidateCommand(c *actions.CommandRequest, epoch string, revision uint64) *actions.ContractError {
	if err := validateEnvelope(c, epoch, revision); err != nil {
		return err
	}
	if c.CapabilitySchemaId != SwitchSchema && c.CapabilitySchemaId != HelperSchema {
		return commandError(actions.ErrorCode_ERROR_CODE_UNSUPPORTED_CAPABILITY, "capability_schema_id", "command rejected by contract")
	}
	if c.CapabilitySchemaId == HelperSchema {
		action := c.GetArguments().GetHelper()
		if action == nil || !knownFieldsOnly(action.ProtoReflect()) || action.ExpectedHelperRevision == nil || *action.ExpectedHelperRevision > math.MaxInt64 || admittedSimpleHelperOperation(action.Operation) == false {
			return commandError(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "arguments.helper", "command rejected by contract")
		}
		if action.Operation == helperwire.HelperOperation_HELPER_OPERATION_SET {
			if _, err := AdmitHelperSetValue(action.Value); err != nil {
				return commandError(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "arguments.helper.value", "command rejected by contract")
			}
		} else if action.Value != nil {
			return commandError(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "arguments.helper.value", "command rejected by contract")
		}
		return nil
	}
	if !validArguments(c.Arguments) {
		return commandError(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "arguments", "command rejected by contract")
	}
	return nil
}

func admittedSimpleHelperOperation(operation helperwire.HelperOperation) bool {
	return operation >= helperwire.HelperOperation_HELPER_OPERATION_SET && operation <= helperwire.HelperOperation_HELPER_OPERATION_PREVIOUS
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
	if err := RejectUnknownCommandFields(c); err != nil {
		return err
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

// RejectUnknownCommandFields keeps future wire fields decodable but denies
// operational admission until this server understands their semantics.
func RejectUnknownCommandFields(c *actions.CommandRequest) *actions.ContractError {
	if c == nil {
		return commandError(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "command", "unknown command field")
	}
	if len(c.ProtoReflect().GetUnknown()) != 0 {
		return commandError(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "command", "unknown command field")
	}
	if c.Target != nil && !knownFieldsOnly(c.Target.ProtoReflect()) {
		return commandError(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "target", "unknown target field")
	}
	if c.Arguments != nil && !knownFieldsOnly(c.Arguments.ProtoReflect()) {
		return commandError(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "arguments", "unknown argument field")
	}
	if c.ExpiresAt != nil && !knownFieldsOnly(c.ExpiresAt.ProtoReflect()) {
		return commandError(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "expires_at", "unknown timestamp field")
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
