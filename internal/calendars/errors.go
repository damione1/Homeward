// Package calendars owns admitted calendar data and bounded recurrence
// expansion. Network synchronization and automation execution live elsewhere.
package calendars

import "context"

// ErrorCode is the single closed vocabulary of calendar failures.
type ErrorCode uint8

const (
	ErrorInvalid ErrorCode = iota
	InvalidInput
	InvalidDate
	InvalidZone
	MissingEnd
	UnsupportedRule
	InvalidOccurrence
	ReadOnly
	Unregistered
	Retired
	BindingMismatch
	NotFound
	Deleted
	Conflict
	Incompatible
	Corrupt
	Unavailable
	CommitUnknown
	RevisionExhausted
	BudgetExceeded
	CapacityExceeded
	Canceled
	Busy
	Closed
	StaleCursor
	ResyncRequired
	MissingFloatingZone
	ConflictingEnd
	EndType
	ContradictoryIdentity
	UnsupportedObservanceRule
	ZoneCoverageUnknown
	InconsistentObservance
	InvalidDuration
	DurationLimit
	InvalidStatus
)

// RuleIssue makes rejection precise without rendering untrusted rule text.
type RuleIssue uint8

const (
	RuleIssueNone RuleIssue = iota
	RuleSyntax
	RuleProperty
	RuleDuplicate
	RuleFrequency
	RuleInterval
	RuleCount
	RuleCountUntil
	RuleUntilType
	RuleWeekStart
	RuleWeekday
	RuleMonthDay
	RuleMonth
	RuleAnchor
	RuleZeroAnchor
)

type Error struct {
	code  ErrorCode
	issue RuleIssue
	cause error
}

func failure(code ErrorCode) *Error      { return &Error{code: code} }
func ruleFailure(issue RuleIssue) *Error { return &Error{code: UnsupportedRule, issue: issue} }
func (e *Error) Code() ErrorCode {
	if e == nil {
		return ErrorInvalid
	}
	return e.code
}
func (e *Error) RuleIssue() RuleIssue {
	if e == nil {
		return RuleIssueNone
	}
	return e.issue
}
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}
func (e *Error) GoString() string { return e.Error() }
func (e *Error) Error() string {
	switch e.Code() {
	case InvalidInput:
		return "calendars: invalid input"
	case InvalidDate:
		return "calendars: invalid civil date or instant"
	case InvalidZone:
		return "calendars: explicit IANA timezone required"
	case MissingEnd:
		return "calendars: explicit exclusive end required"
	case UnsupportedRule:
		return "calendars: recurrence is outside profile homeward.calendar-recurrence/1"
	case InvalidOccurrence:
		return "calendars: recurrence identity is not in this series"
	case ReadOnly:
		return "calendars: source is read-only"
	case Unregistered:
		return "calendars: identity has no registry binding"
	case Retired:
		return "calendars: identity is retired"
	case BindingMismatch:
		return "calendars: registry binding changed"
	case NotFound:
		return "calendars: resource is absent"
	case Deleted:
		return "calendars: resource is deleted"
	case Conflict:
		return "calendars: revision conflict"
	case Incompatible:
		return "calendars: persisted data uses an incompatible profile"
	case Corrupt:
		return "calendars: persisted data is corrupt"
	case Unavailable:
		return "calendars: storage unavailable"
	case CommitUnknown:
		return "calendars: transaction commit outcome is unknown"
	case RevisionExhausted:
		return "calendars: revision exhausted"
	case BudgetExceeded:
		return "calendars: bounded calculation budget exceeded"
	case CapacityExceeded:
		return "calendars: bounded image or resource capacity exceeded"
	case Canceled:
		return "calendars: operation canceled"
	case Busy:
		return "calendars: expansion capacity unavailable"
	case Closed:
		return "calendars: service is closed"
	case StaleCursor:
		return "calendars: page cursor requires a fresh snapshot"
	case ResyncRequired:
		return "calendars: subscriber requires resynchronization"
	case MissingFloatingZone:
		return "calendars: floating time requires an explicit projection zone"
	case ConflictingEnd:
		return "calendars: DTEND and DURATION conflict"
	case EndType:
		return "calendars: start and end temporal forms differ"
	case ContradictoryIdentity:
		return "calendars: duplicate UID and recurrence identity"
	case UnsupportedObservanceRule:
		return "calendars: VTIMEZONE observance is outside admitted profile"
	case InconsistentObservance:
		return "calendars: inconsistent or empty observance"
	case InvalidDuration:
		return "calendars: invalid positive event duration"
	case DurationLimit:
		return "calendars: duration exceeds admitted bound"
	case InvalidStatus:
		return "calendars: invalid event status"
	case ZoneCoverageUnknown:
		return "calendars: embedded zone has no admitted coverage"
	default:
		return "calendars: invalid error"
	}
}

func canceled(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return &Error{code: Canceled, cause: err}
	}
	return nil
}
