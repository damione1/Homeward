// Package apiv2 is the untrusted protobuf boundary for home.api.v2. It parses
// representation once; services retain authz, live constraints and durable I/O.
package apiv2

import (
	"fmt"

	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ViolationKind uses the generated vocabulary, never a parallel integer set.
// The private violation constructor emits only declared nonzero values.
type ViolationKind = wire.ValidationReason

const (
	InvalidName      = wire.ValidationReason_VALIDATION_REASON_INVALID_NAME
	MissingIntent    = wire.ValidationReason_VALIDATION_REASON_MISSING_INTENT
	UnknownValue     = wire.ValidationReason_VALIDATION_REASON_UNKNOWN_VALUE
	OutOfRange       = wire.ValidationReason_VALIDATION_REASON_OUT_OF_RANGE
	InvalidRequestID = wire.ValidationReason_VALIDATION_REASON_INVALID_REQUEST_ID
	InvalidDeadline  = wire.ValidationReason_VALIDATION_REASON_INVALID_DEADLINE
	UnknownField     = wire.ValidationReason_VALIDATION_REASON_UNKNOWN_FIELD
)

// Violation carries a protobuf field path, not a Go name or a oneof container.
type Violation struct {
	field string
	kind  ViolationKind
}

func (v *Violation) Field() string       { return v.field }
func (v *Violation) Kind() ViolationKind { return v.kind }
func (v *Violation) Error() string {
	return fmt.Sprintf("invalid command field %s (%d)", v.field, v.kind)
}

func reject(field string, kind ViolationKind) error { return &Violation{field, kind} }

// knownFields rejects future executable fields, including nested unknown fields.
// Unknown observation fields are a different, non-actionable read policy.
func knownFields(message protoreflect.Message) bool {
	if !message.IsValid() || len(message.GetUnknown()) != 0 {
		return false
	}
	known := true
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Message() == nil {
			return true
		}
		if field.IsList() {
			list := value.List()
			for i := 0; i < list.Len(); i++ {
				if !knownFields(list.Get(i).Message()) {
					known = false
					return false
				}
			}
		} else if field.IsMap() {
			// This intent schema deliberately has no arbitrary map payloads.
			known = false
		} else {
			known = knownFields(value.Message())
		}
		return known
	})
	return known
}

// ParsePower explicitly handles every declared proto value. Go does not enforce
// exhaustiveness: TestPowerDescriptorCoverage checks this against the descriptor.
func ParsePower(value wire.PowerValue) (apitypes.PowerCommand, error) {
	switch value {
	case wire.PowerValue_POWER_VALUE_ON:
		return apitypes.PowerOn(), nil
	case wire.PowerValue_POWER_VALUE_OFF:
		return apitypes.PowerOff(), nil
	case wire.PowerValue_POWER_VALUE_UNSPECIFIED:
		return apitypes.PowerCommand{}, reject("power.value", MissingIntent)
	default:
		return apitypes.PowerCommand{}, reject("power.value", UnknownValue)
	}
}

func parseIntent(request *wire.ExecuteCapabilityRequest) (apitypes.Intent, error) {
	switch command := request.Command.(type) {
	case *wire.ExecuteCapabilityRequest_Power:
		if command == nil || command.Power == nil {
			return nil, reject("power", MissingIntent)
		}
		return ParsePower(command.Power.Value)
	case *wire.ExecuteCapabilityRequest_Level:
		if command == nil || command.Level == nil {
			return nil, reject("level", MissingIntent)
		}
		switch target := command.Level.Target.(type) {
		case *wire.SetLevelCommand_Percentage:
			if target == nil {
				return nil, reject("level.percentage", MissingIntent)
			}
			percentage, err := apitypes.NewPercentage(target.Percentage)
			if err != nil {
				return nil, reject("level.percentage", OutOfRange)
			}
			return apitypes.SetLevel(percentage), nil
		default:
			return nil, reject("level", MissingIntent)
		}
	default:
		return nil, reject("command", MissingIntent)
	}
}

// ParseCommand is pure and must be called AFTER object authorization. Success
// confers no permit and does not reserve or execute an action. Every unknown
// executable field is rejected instead of silently reducing the user's intent.
func ParseCommand(request *wire.ExecuteCapabilityRequest) (apitypes.Command, error) {
	if request == nil {
		return apitypes.Command{}, reject("command", MissingIntent)
	}
	if !knownFields(request.ProtoReflect()) {
		return apitypes.Command{}, reject("", UnknownField)
	}
	target, err := apitypes.ParseCapabilityName(request.Name)
	if err != nil {
		return apitypes.Command{}, reject("name", InvalidName)
	}
	intent, err := parseIntent(request)
	if err != nil {
		return apitypes.Command{}, err
	}
	var id *apitypes.RequestID
	if request.RequestId != "" {
		value, err := apitypes.ParseRequestID(request.RequestId)
		if err != nil {
			return apitypes.Command{}, reject("request_id", InvalidRequestID)
		}
		id = &value
	}
	if request.ExpireTime == nil || request.ExpireTime.CheckValid() != nil || request.ExpireTime.AsTime().IsZero() {
		return apitypes.Command{}, reject("expire_time", InvalidDeadline)
	}
	return apitypes.NewCommand(target, intent, id, request.ExpireTime.AsTime())
}
