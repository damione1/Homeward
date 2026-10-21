package calendars

import (
	"context"
	"errors"
	"time"

	"github.com/damione1/Homeward/internal/storage"
)

type RemoteStateKind uint8

const (
	RemoteStateInvalid RemoteStateKind = iota
	RemoteNeverValidated
	RemoteValidated
)

type RemoteFreshness uint8

const (
	RemoteFreshnessInvalid RemoteFreshness = iota
	RemoteFresh
	RemoteStale
)

type RemoteReason uint8

const (
	RemoteReasonNone RemoteReason = iota
	RemoteReasonTransport
	RemoteReasonContent
	RemoteReasonCachePolicy
	RemoteReasonExpired
	RemoteReasonReconfigured
	RemoteReasonPublication
	RemoteReasonUnavailable
)

func (r RemoteReason) valid() bool { return r <= RemoteReasonUnavailable }
func reasonToStorage(r RemoteReason) (storage.RemoteCalendarReason, error) {
	switch r {
	case RemoteReasonNone:
		return storage.RemoteCalendarReasonNone, nil
	case RemoteReasonTransport:
		return storage.RemoteCalendarReasonTransport, nil
	case RemoteReasonContent:
		return storage.RemoteCalendarReasonContent, nil
	case RemoteReasonCachePolicy:
		return storage.RemoteCalendarReasonCachePolicy, nil
	case RemoteReasonExpired:
		return storage.RemoteCalendarReasonExpired, nil
	case RemoteReasonReconfigured:
		return storage.RemoteCalendarReasonReconfigured, nil
	case RemoteReasonPublication:
		return storage.RemoteCalendarReasonPublication, nil
	case RemoteReasonUnavailable:
		return storage.RemoteCalendarReasonUnavailable, nil
	default:
		return 0, failure(InvalidInput)
	}
}
func reasonFromStorage(r storage.RemoteCalendarReason) (RemoteReason, error) {
	switch r {
	case storage.RemoteCalendarReasonNone:
		return RemoteReasonNone, nil
	case storage.RemoteCalendarReasonTransport:
		return RemoteReasonTransport, nil
	case storage.RemoteCalendarReasonContent:
		return RemoteReasonContent, nil
	case storage.RemoteCalendarReasonCachePolicy:
		return RemoteReasonCachePolicy, nil
	case storage.RemoteCalendarReasonExpired:
		return RemoteReasonExpired, nil
	case storage.RemoteCalendarReasonReconfigured:
		return RemoteReasonReconfigured, nil
	case storage.RemoteCalendarReasonPublication:
		return RemoteReasonPublication, nil
	case storage.RemoteCalendarReasonUnavailable:
		return RemoteReasonUnavailable, nil
	default:
		return 0, failure(Corrupt)
	}
}

// RemoteState distinguishes never validated from an admitted empty document.
// A local calendar carries no RemoteState, exposed by RemoteState's bool.
type RemoteState struct {
	kind                        RemoteStateKind
	freshness                   RemoteFreshness
	source, representation      [32]byte
	validated, attempted, until time.Time
	reason                      RemoteReason
}

func (s RemoteState) Valid() bool {
	if !s.reason.valid() {
		return false
	}
	switch s.kind {
	case RemoteNeverValidated:
		return s.freshness == RemoteFreshnessInvalid && s.representation == ([32]byte{}) && s.validated.IsZero() && s.until.IsZero()
	case RemoteValidated:
		return (s.freshness == RemoteFresh || s.freshness == RemoteStale) && s.representation != ([32]byte{}) && !s.validated.IsZero() && !s.attempted.IsZero() && !s.until.IsZero() && (s.freshness != RemoteFresh || s.source == s.representation && s.reason == RemoteReasonNone)
	default:
		return false
	}
}
func (s RemoteState) Kind() RemoteStateKind {
	if !s.Valid() {
		return RemoteStateInvalid
	}
	return s.kind
}
func (s RemoteState) Freshness() (RemoteFreshness, bool) {
	return s.freshness, s.Valid() && s.kind == RemoteValidated
}
func (s RemoteState) LastValidated() (time.Time, bool) {
	return s.validated, s.Valid() && s.kind == RemoteValidated
}
func (s RemoteState) LastAttempt() (time.Time, bool) {
	return s.attempted, s.Valid() && !s.attempted.IsZero()
}
func (s RemoteState) FreshUntil() (time.Time, bool) {
	return s.until, s.Valid() && s.kind == RemoteValidated
}
func (s RemoteState) Reason() RemoteReason { return s.reason }
func (s RemoteState) SourceKey() ([32]byte, bool) {
	return s.source, s.Valid() && s.source != ([32]byte{})
}
func (s RemoteState) RepresentationKey() ([32]byte, bool) {
	return s.representation, s.Valid() && s.kind == RemoteValidated
}
func (s RemoteState) String() string               { return "calendars: remote state [redacted]" }
func (s RemoteState) GoString() string             { return s.String() }
func (s RemoteState) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

func remoteStorageFailure(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &Error{code: Canceled, cause: err}
	}
	var e *storage.RemoteCalendarError
	if errors.As(err, &e) {
		switch e.Kind() {
		case storage.RemoteCalendarInvalid:
			return failure(InvalidInput)
		case storage.RemoteCalendarUnavailable:
			return failure(Unavailable)
		case storage.RemoteCalendarConflict:
			return failure(Conflict)
		case storage.RemoteCalendarCorrupt:
			return failure(Corrupt)
		case storage.RemoteCalendarCommitUnknown:
			return failure(CommitUnknown)
		case storage.RemoteCalendarRevisionExhausted:
			return failure(RevisionExhausted)
		}
	}
	return failure(Unavailable)
}
