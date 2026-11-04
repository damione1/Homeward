package entityfilter

// ErrorKind is closed admission classification. Error text never echoes input.
type ErrorKind uint8

const (
	Syntax ErrorKind = iota + 1
	UnknownField
	UnknownFunction
	UnknownTrait
	InvalidValue
	UnsupportedOperator
	TextLimit
	TokenLimit
	NodeLimit
	DepthLimit
	InvalidCandidate
)

type Error struct {
	kind   ErrorKind
	offset int
}

func (e *Error) Kind() ErrorKind { return e.kind }
func (e *Error) Offset() int     { return e.offset }
func (e *Error) Error() string {
	switch e.kind {
	case Syntax:
		return "invalid entity filter syntax"
	case UnknownField:
		return "unsupported entity filter field"
	case UnknownFunction:
		return "unsupported entity filter function"
	case UnknownTrait:
		return "unknown capability trait"
	case InvalidValue:
		return "invalid entity filter literal"
	case UnsupportedOperator:
		return "unsupported entity filter operator"
	case TextLimit:
		return "entity filter text limit exceeded"
	case TokenLimit:
		return "entity filter token limit exceeded"
	case NodeLimit:
		return "entity filter node limit exceeded"
	case DepthLimit:
		return "entity filter depth limit exceeded"
	case InvalidCandidate:
		return "invalid authorized entity filter image"
	default:
		return "invalid entity filter"
	}
}
func reject(kind ErrorKind, offset int) error { return &Error{kind, offset} }

const (
	MaxTextBytes = 4096
	MaxTokens    = 256
	MaxNodes     = 64
	MaxDepth     = 8
	// This matches the maximum number of minimum-size immutable capability
	// images that can fit the existing 16MiB view preparation budget.
	MaxCandidateCapabilities = 32768
)
