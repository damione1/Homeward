package helpers

// ErrorCode is the closed set of operational repository failures.
type ErrorCode uint8

const (
	ErrorInvalid ErrorCode = iota
	InvalidInput
	InvalidDefinition
	InvalidValue
	Unregistered
	Retired
	BindingMismatch
	Conflict
	Incompatible
	Corrupt
	Unavailable
	CommitUnknown
	InitialAbsent
	RevisionExhausted
	NoAuthorizedTarget
)

type Error struct{ code ErrorCode }

func failure(code ErrorCode) *Error { return &Error{code: code} }
func (e *Error) Code() ErrorCode {
	if e == nil {
		return ErrorInvalid
	}
	return e.code
}
func (e *Error) Error() string {
	switch e.Code() {
	case InvalidInput:
		return "helpers: invalid input"
	case InvalidDefinition:
		return "helpers: definition is not admitted"
	case InvalidValue:
		return "helpers: value is invalid or outside active constraints"
	case Unregistered:
		return "helpers: identity has no registry binding"
	case Retired:
		return "helpers: identity is retired"
	case BindingMismatch:
		return "helpers: definition does not belong to this identity"
	case Conflict:
		return "helpers: revision conflict"
	case Incompatible:
		return "helpers: persisted value is incompatible with the active definition"
	case Corrupt:
		return "helpers: persisted value is corrupt"
	case Unavailable:
		return "helpers: storage unavailable"
	case CommitUnknown:
		return "helpers: transaction commit outcome is unknown"
	case InitialAbsent:
		return "helpers: active definition has no initial value"
	case RevisionExhausted:
		return "helpers: revision exhausted"
	case NoAuthorizedTarget:
		return "helpers: button has no authorized target"
	default:
		return "helpers: invalid error"
	}
}
