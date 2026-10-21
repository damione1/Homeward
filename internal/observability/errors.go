package observability

import (
	"fmt"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
)

type Error struct {
	code  obs.ObservabilityErrorCode
	field obs.ObservabilityField
}

func failure(code obs.ObservabilityErrorCode, field obs.ObservabilityField) *Error {
	return &Error{code: code, field: field}
}
func invalid(field obs.ObservabilityField) *Error {
	return failure(obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_INVALID_ARGUMENT, field)
}
func stale() *Error {
	return failure(obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_STALE_CURSOR, obs.ObservabilityField_OBSERVABILITY_FIELD_PAGE_TOKEN)
}
func capacity() *Error {
	return failure(obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_CAPACITY, obs.ObservabilityField_OBSERVABILITY_FIELD_CAPTURE)
}
func (e *Error) Error() string                  { return "observability: operation rejected" }
func (e *Error) Format(state fmt.State, _ rune) { _, _ = state.Write([]byte(e.Error())) }
func (e *Error) Code() obs.ObservabilityErrorCode {
	if e == nil || e.code == obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_UNSPECIFIED {
		return obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_INVALID_ARGUMENT
	}
	return e.code
}
func (e *Error) Wire() *obs.ObservabilityError {
	if e == nil || e.code == obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_UNSPECIFIED {
		return &obs.ObservabilityError{Code: obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_INVALID_ARGUMENT, Field: obs.ObservabilityField_OBSERVABILITY_FIELD_NONE}
	}
	return &obs.ObservabilityError{Code: e.code, Field: e.field}
}
