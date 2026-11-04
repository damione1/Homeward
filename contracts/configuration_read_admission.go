package contracts

import (
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
)

// ConfigurationViolation names a declared schema path and generated rule. It
// cannot retain rejected input, SQL, source bytes or a credential.
type ConfigurationViolation struct {
	field string
	rule  wire.ValidationReason
}

func (v *ConfigurationViolation) Error() string {
	return "configuration request violates its documented contract"
}
func (v *ConfigurationViolation) Field() string               { return v.field }
func (v *ConfigurationViolation) Rule() wire.ValidationReason { return v.rule }

// ConfigurationDocumentRead is an admitted name; its zero value is invalid.
type ConfigurationDocumentRead struct {
	name apitypes.ConfigurationDocumentName
}

func (v ConfigurationDocumentRead) Name() apitypes.ConfigurationDocumentName { return v.name }
func ParseGetConfigurationDocument(value *wire.GetConfigurationDocumentRequest) (ConfigurationDocumentRead, error) {
	reject := func(field string, rule wire.ValidationReason) (ConfigurationDocumentRead, error) {
		return ConfigurationDocumentRead{}, &ConfigurationViolation{field: field, rule: rule}
	}
	if value == nil {
		return reject("name", wire.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE)
	}
	if !configurationKnown(value.ProtoReflect()) {
		return reject("", wire.ValidationReason_VALIDATION_REASON_UNKNOWN_FIELD)
	}
	if value.Name == "" {
		return reject("name", wire.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE)
	}
	name, err := apitypes.ParseConfigurationDocumentName(value.Name)
	if err != nil {
		return reject("name", wire.ValidationReason_VALIDATION_REASON_INVALID_NAME)
	}
	return ConfigurationDocumentRead{name: name}, nil
}
