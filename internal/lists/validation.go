package lists

import api "github.com/damione1/Homeward/gen/home/api/v2"

// InputViolation names a fixed, constructor-owned relative field and rule.
// The shared generated enum is vocabulary only: request protobuf messages never
// become list domain values. No untrusted text or rejected value is retained.
type InputViolation struct {
	field string
	rule  api.ValidationReason
}

func (v InputViolation) Field() string              { return v.field }
func (v InputViolation) Rule() api.ValidationReason { return v.rule }

// InputValidationError remains an Invalid lists error for existing callers.
// API boundaries may project its immutable issues into their own field paths.
type InputValidationError struct{ issues []InputViolation }

func (*InputValidationError) Error() string { return "lists: invalid input constraints" }
func (*InputValidationError) Unwrap() error { return failure(Invalid) }
func (e *InputValidationError) Issues() []InputViolation {
	if e == nil {
		return nil
	}
	return append([]InputViolation(nil), e.issues...)
}
