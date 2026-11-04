package apiv2

import (
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
)

// ParseEntityUpdate admits representation only, AFTER target and administrative
// authorization. Output-only values are ignored; their presence never creates
// a state write. Relative masks admit only known Entity fields.
func ParseEntityUpdate(request *wire.UpdateEntityRequest) (apitypes.EntityUpdate, error) {
	reject := func(field string, rule wire.ValidationReason) (apitypes.EntityUpdate, error) {
		return apitypes.EntityUpdate{}, &Violation{field: field, kind: rule, root: (&wire.UpdateEntityRequest{}).ProtoReflect().Descriptor()}
	}
	if request == nil || request.Entity == nil {
		return reject("entity", wire.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE)
	}
	if len(request.ProtoReflect().GetUnknown()) != 0 || len(request.Entity.ProtoReflect().GetUnknown()) != 0 {
		return reject("", UnknownField)
	}
	target, err := apitypes.ParseEntityName(request.Entity.Name)
	if err != nil {
		return reject("entity.name", InvalidName)
	}
	etag, err := apitypes.ParseEntityETag(request.Entity.Etag)
	if err != nil {
		return reject("entity.etag", wire.ValidationReason_VALIDATION_REASON_INVALID_FORMAT)
	}
	var id *apitypes.RequestID
	if request.RequestId != "" {
		value, err := apitypes.ParseRequestID(request.RequestId)
		if err != nil {
			return reject("request_id", InvalidRequestID)
		}
		id = &value
	}
	selected := false
	if request.UpdateMask == nil {
		// An omitted mask selects populated mutable fields; explicit empty
		// values require a present mask, so clearing is never guessed.
		selected = request.Entity.DisplayName != ""
	} else {
		if len(request.UpdateMask.ProtoReflect().GetUnknown()) != 0 || len(request.UpdateMask.Paths) > 32 {
			return reject("update_mask", wire.ValidationReason_VALIDATION_REASON_INVALID_MASK)
		}
		for _, path := range request.UpdateMask.Paths {
			if path == "*" && len(request.UpdateMask.Paths) != 1 {
				return reject("update_mask", wire.ValidationReason_VALIDATION_REASON_INVALID_MASK)
			}
			switch path {
			case "display_name", "*":
				selected = true
			case "name", "etag":
				// The request's name is the exact addressed resource and etag
				// is an independent precondition; neither is updated.
			case "create_time", "update_time":
				// Ignore output-only fields, including any input values.
			default:
				return reject("update_mask", wire.ValidationReason_VALIDATION_REASON_INVALID_MASK)
			}
		}
	}
	patch := apitypes.KeepEntityMetadata()
	if selected {
		label, err := apitypes.NewDisplayName(request.Entity.DisplayName)
		if err != nil {
			return reject("entity.display_name", wire.ValidationReason_VALIDATION_REASON_INVALID_FORMAT)
		}
		patch = apitypes.SetEntityDisplayName(label)
	}
	update, err := apitypes.NewEntityUpdate(target, etag, patch, id)
	if err != nil {
		return reject("", wire.ValidationReason_VALIDATION_REASON_INCONSISTENT_FIELDS)
	}
	return update, nil
}
