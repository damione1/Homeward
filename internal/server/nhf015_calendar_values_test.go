package server

// Explicit synthetic qualification only. This adapter is not compiled into the
// production server and never starts an integration or a device driver.

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	cv "github.com/damione1/Homeward/gen/home/calendars/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/calendars"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/emptypb"
)

func nhf015CalendarFailure(code cv.ErrorCode) error {
	status := connect.CodeUnavailable
	switch code {
	case cv.ErrorCode_ERROR_CODE_INVALID:
		status = connect.CodeInvalidArgument
	case cv.ErrorCode_ERROR_CODE_DENIED:
		status = connect.CodePermissionDenied
	case cv.ErrorCode_ERROR_CODE_NOT_FOUND:
		status = connect.CodeNotFound
	case cv.ErrorCode_ERROR_CODE_CONFLICT, cv.ErrorCode_ERROR_CODE_COMMIT_UNKNOWN, cv.ErrorCode_ERROR_CODE_COMMITTED_RESPONSE_WITHHELD:
		status = connect.CodeAborted
	case cv.ErrorCode_ERROR_CODE_READ_ONLY, cv.ErrorCode_ERROR_CODE_STALE_CURSOR:
		status = connect.CodeFailedPrecondition
	case cv.ErrorCode_ERROR_CODE_CAPACITY:
		status = connect.CodeResourceExhausted
	case cv.ErrorCode_ERROR_CODE_UNAVAILABLE, cv.ErrorCode_ERROR_CODE_UNSPECIFIED:
	default:
	}
	err := connect.NewError(status, errors.New("calendar request rejected"))
	if detail, e := connect.NewErrorDetail(&cv.CalendarError{Code: code}); e == nil {
		err.AddDetail(detail)
	}
	return err
}
func nhf015CalendarRPCError(err error) error {
	if err == nil {
		return nil
	}
	var existing *connect.Error
	if errors.As(err, &existing) {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, errors.New("calendar request canceled"))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("calendar request timed out"))
	}
	if errors.Is(err, authorization.ErrDenied) || errors.Is(err, authorization.ErrUnavailable) {
		return nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_DENIED)
	}
	var e *calendars.Error
	if !errors.As(err, &e) {
		return nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_UNAVAILABLE)
	}
	code := cv.ErrorCode_ERROR_CODE_UNAVAILABLE
	switch e.Code() {
	case calendars.InvalidInput, calendars.InvalidDate, calendars.InvalidZone, calendars.MissingEnd, calendars.UnsupportedRule, calendars.InvalidOccurrence, calendars.Incompatible, calendars.MissingFloatingZone, calendars.ConflictingEnd, calendars.EndType, calendars.ContradictoryIdentity, calendars.UnsupportedObservanceRule, calendars.ZoneCoverageUnknown, calendars.InconsistentObservance, calendars.InvalidDuration, calendars.DurationLimit, calendars.InvalidStatus:
		code = cv.ErrorCode_ERROR_CODE_INVALID
	case calendars.ReadOnly:
		code = cv.ErrorCode_ERROR_CODE_READ_ONLY
	case calendars.Unregistered, calendars.Retired, calendars.NotFound, calendars.Deleted:
		code = cv.ErrorCode_ERROR_CODE_NOT_FOUND
	case calendars.Conflict, calendars.BindingMismatch:
		code = cv.ErrorCode_ERROR_CODE_CONFLICT
	case calendars.StaleCursor, calendars.ResyncRequired:
		code = cv.ErrorCode_ERROR_CODE_STALE_CURSOR
	case calendars.BudgetExceeded, calendars.CapacityExceeded, calendars.Busy, calendars.RevisionExhausted:
		code = cv.ErrorCode_ERROR_CODE_CAPACITY
	case calendars.CommitUnknown:
		code = cv.ErrorCode_ERROR_CODE_COMMIT_UNKNOWN
	case calendars.ErrorInvalid, calendars.Corrupt, calendars.Unavailable, calendars.Closed, calendars.Canceled:
		code = cv.ErrorCode_ERROR_CODE_UNAVAILABLE
	}
	return nhf015CalendarFailure(code)
}

// Recursively reject unknown command variants before treating absence as an
// admitted domain value. Forward decoding alone is never mutation admission.
func nhf015CalendarKnown(message proto.Message) bool {
	if message == nil {
		return false
	}
	m := message.ProtoReflect()
	if !m.IsValid() || len(m.GetUnknown()) != 0 {
		return false
	}
	valid := true
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() != protoreflect.MessageKind {
			return true
		}
		if fd.IsList() {
			list := v.List()
			for i := 0; i < list.Len(); i++ {
				if !nhf015CalendarKnown(list.Get(i).Message().Interface()) {
					valid = false
					return false
				}
			}
		} else if !nhf015CalendarKnown(v.Message().Interface()) {
			valid = false
			return false
		}
		return true
	})
	return valid
}
func nhf015CalendarRevision(value calendars.Revision, id calendars.CalendarID, uid string) *cv.Revision {
	number, _ := value.Number()
	entity, _ := id.Entity()
	text, _ := entity.Value()
	return &cv.Revision{Resource: &cv.EventKey{Calendar: &cv.CalendarId{EntityId: text}, Uid: uid}, Presence: nhf015WireCalendarPresence(value.Presence()), Number: number}
}
func nhf015CalendarExpected(raw *cv.Revision, id calendars.CalendarID, uid calendars.EventUID) (calendars.Revision, error) {
	entity, _ := id.Entity()
	text, _ := entity.Value()
	u, _ := uid.Value()
	if raw == nil || !nhf015CalendarKnown(raw) || raw.Resource == nil || raw.Resource.Calendar == nil || raw.Resource.Calendar.EntityId != text || raw.Resource.Uid != u {
		return calendars.Revision{}, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
	}
	var p calendars.Presence
	switch raw.Presence {
	case cv.Presence_PRESENCE_ABSENT:
		p = calendars.Absent
	case cv.Presence_PRESENCE_PRESENT:
		p = calendars.Present
	case cv.Presence_PRESENCE_REMOVED:
		p = calendars.Removed
	default:
		return calendars.Revision{}, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
	}
	return calendars.ExpectedRevision(id, uid, p, raw.Number)
}
func nhf015CalendarSpan(ctx context.Context, raw *cv.Span) (calendars.Span, error) {
	if raw == nil || !nhf015CalendarKnown(raw) {
		return calendars.Span{}, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
	}
	switch v := raw.Value.(type) {
	case *cv.Span_Dates:
		if v == nil || v.Dates == nil {
			break
		}
		a, e := calendars.ParseDate(v.Dates.Start)
		if e != nil {
			return calendars.Span{}, e
		}
		b, e := calendars.ParseDate(v.Dates.EndExclusive)
		if e != nil {
			return calendars.Span{}, e
		}
		return calendars.AllDaySpan(a, b)
	case *cv.Span_Times:
		if v == nil || v.Times == nil {
			break
		}
		z, e := calendars.ParseZone(v.Times.Zone)
		if e != nil {
			return calendars.Span{}, e
		}
		a, e := calendars.ParseLocalDateTime(v.Times.AnchorLocal)
		if e != nil {
			return calendars.Span{}, e
		}
		b, e := calendars.ParseInstant(v.Times.EndInstant)
		if e != nil {
			return calendars.Span{}, e
		}
		start, e := calendars.ParseInstant(v.Times.StartInstant)
		if e != nil {
			return calendars.Span{}, e
		}
		return calendars.SelectedTimedSpan(ctx, a, start, b, z)
	}
	return calendars.Span{}, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
}
func nhf015WireCalendarSpan(span calendars.Span) *cv.Span {
	if a, b, ok := span.Dates(); ok {
		return &cv.Span{Value: &cv.Span_Dates{Dates: &cv.DateSpan{Start: a.String(), EndExclusive: b.String()}}}
	}
	a, b, z, ok := span.Instants()
	if !ok {
		return nil
	}
	anchor, _ := span.Anchor()
	return &cv.Span{Value: &cv.Span_Times{Times: &cv.TimeSpan{StartInstant: a.String(), EndInstant: b.String(), AnchorLocal: anchor.String(), Zone: z.Name()}}}
}
func nhf015CalendarEvent(ctx context.Context, raw *cv.Event) (calendars.Event, error) {
	if raw == nil || !nhf015CalendarKnown(raw) || len(raw.Additions) > calendars.MaxDates || len(raw.Exclusions) > calendars.MaxDates || len(raw.Exceptions) > calendars.MaxExceptions {
		return calendars.Event{}, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
	}
	span, err := nhf015CalendarSpan(ctx, raw.Span)
	if err != nil {
		return calendars.Event{}, err
	}
	rule := calendars.NoRecurrence()
	if raw.RecurrenceRule != nil {
		rule, err = calendars.ParseRule(*raw.RecurrenceRule, span)
		if err != nil {
			return calendars.Event{}, err
		}
	}
	input := calendars.EventInput{Summary: raw.Summary, Description: raw.Description, Location: raw.Location, Span: span, Rule: rule}
	for _, text := range raw.Additions {
		p, e := calendars.ParsePoint(span.Kind(), text)
		if e != nil {
			return calendars.Event{}, e
		}
		input.Additions = append(input.Additions, p)
	}
	for _, text := range raw.Exclusions {
		p, e := calendars.ParsePoint(span.Kind(), text)
		if e != nil {
			return calendars.Event{}, e
		}
		input.Exclusions = append(input.Exclusions, p)
	}
	for _, ex := range raw.Exceptions {
		if ex == nil || ex.ReplacementSummary != nil || ex.ReplacementDescription != nil || ex.ReplacementLocation != nil {
			return calendars.Event{}, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
		}
		p, e := calendars.ParsePoint(span.Kind(), ex.Original)
		if e != nil {
			return calendars.Event{}, e
		}
		var exception calendars.Exception
		switch v := ex.Change.(type) {
		case *cv.Exception_Cancel:
			if v == nil || v.Cancel == nil {
				return calendars.Event{}, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
			}
			exception, e = calendars.Cancel(p)
		case *cv.Exception_Replacement:
			if v == nil {
				return calendars.Event{}, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
			}
			replacement, x := nhf015CalendarSpan(ctx, v.Replacement)
			if x != nil {
				return calendars.Event{}, x
			}
			exception, e = calendars.Move(p, replacement)
		default:
			return calendars.Event{}, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
		}
		if e != nil {
			return calendars.Event{}, e
		}
		input.Exceptions = append(input.Exceptions, exception)
	}
	return calendars.NewEvent(ctx, input)
}
func nhf015WireCalendarEvent(value calendars.Event) *cv.Event {
	result := &cv.Event{Summary: value.Summary(), Description: value.Description(), Location: value.Location(), Span: nhf015WireCalendarSpan(value.Span())}
	if text, ok := value.Rule().Text(); ok {
		result.RecurrenceRule = &text
	}
	for _, p := range value.Additions() {
		result.Additions = append(result.Additions, p.String())
	}
	for _, p := range value.Exclusions() {
		result.Exclusions = append(result.Exclusions, p.String())
	}
	for _, e := range value.Exceptions() {
		x := &cv.Exception{Original: e.Original().String()}
		if e.Kind() == calendars.CancelOccurrence {
			x.Change = &cv.Exception_Cancel{Cancel: &emptypb.Empty{}}
		} else if span, ok := e.Replacement(); ok {
			x.Change = &cv.Exception_Replacement{Replacement: nhf015WireCalendarSpan(span)}
		}
		if content, ok := e.Content(); ok {
			if value, present := content.Summary().Value(); present {
				x.ReplacementSummary = &value
			}
			if value, present := content.Description().Value(); present {
				x.ReplacementDescription = &value
			}
			if value, present := content.Location().Value(); present {
				x.ReplacementLocation = &value
			}
		}
		result.Exceptions = append(result.Exceptions, x)
	}
	return result
}
func nhf015CalendarQuality(state calendars.RemoteState, remote bool) cv.Quality {
	if !remote {
		return cv.Quality_QUALITY_FRESH
	}
	quality, known := state.Freshness()
	if !known {
		return cv.Quality_QUALITY_UNAVAILABLE
	}
	switch quality {
	case calendars.RemoteFresh:
		return cv.Quality_QUALITY_FRESH
	case calendars.RemoteStale:
		return cv.Quality_QUALITY_STALE
	default:
		return cv.Quality_QUALITY_UNAVAILABLE
	}
}
func nhf015WireCalendar(value calendars.CalendarSnapshot) *cv.Calendar {
	entity, _ := value.ID().Entity()
	text, _ := entity.Value()
	state, remote := value.RemoteState()
	result := &cv.Calendar{Id: &cv.CalendarId{EntityId: text}, Origin: nhf015WireCalendarOrigin(value.ID().Origin()), Revision: nhf015CalendarRevision(value.Revision(), value.ID(), ""), Quality: nhf015CalendarQuality(state, remote)}
	if name, ok := value.Name(); ok {
		result.Name = &name
	}
	if until, ok := state.FreshUntil(); remote && ok {
		text := until.UTC().Format(time.RFC3339Nano)
		result.FreshUntil = &text
	}
	return result
}

func nhf015WireCalendarPresence(value calendars.Presence) cv.Presence {
	switch value {
	case calendars.Absent:
		return cv.Presence_PRESENCE_ABSENT
	case calendars.Present:
		return cv.Presence_PRESENCE_PRESENT
	case calendars.Removed:
		return cv.Presence_PRESENCE_REMOVED
	case calendars.PresenceInvalid:
		return cv.Presence_PRESENCE_UNSPECIFIED
	default:
		return cv.Presence_PRESENCE_UNSPECIFIED
	}
}
func nhf015WireCalendarOrigin(value calendars.Origin) cv.Origin {
	switch value {
	case calendars.Local:
		return cv.Origin_ORIGIN_LOCAL
	case calendars.RemoteICS:
		return cv.Origin_ORIGIN_REMOTE_ICS
	case calendars.Calculated:
		return cv.Origin_ORIGIN_CALCULATED
	case calendars.OriginInvalid:
		return cv.Origin_ORIGIN_UNSPECIFIED
	default:
		return cv.Origin_ORIGIN_UNSPECIFIED
	}
}
