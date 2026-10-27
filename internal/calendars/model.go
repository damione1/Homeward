package calendars

import (
	"context"
	"slices"
	"strings"
	"time"
)

const (
	MaxEvents                    = 1024
	MaxDates                     = 256
	MaxExceptions                = 256
	MaxImageBytes          int64 = 32 * 1024 * 1024
	MaxAggregateImageBytes int64 = 64 * 1024 * 1024
	eventBaseCharge        int64 = 16384
	pointCharge            int64 = 256
	exceptionCharge        int64 = 2048
)

type ExceptionKind uint8

const (
	ExceptionInvalid ExceptionKind = iota
	CancelOccurrence
	MoveOccurrence
	ReplaceOccurrence
)

type Exception struct {
	kind        ExceptionKind
	original    Point
	replacement Span
	content     EventContent
}

func Cancel(point Point) (Exception, error) {
	if !point.Valid() {
		return Exception{}, failure(InvalidOccurrence)
	}
	return Exception{kind: CancelOccurrence, original: point}, nil
}
func Move(point Point, replacement Span) (Exception, error) {
	if !point.Valid() || !replacement.Valid() || point.kind != replacement.kind {
		return Exception{}, failure(InvalidOccurrence)
	}
	return Exception{kind: MoveOccurrence, original: point, replacement: replacement}, nil
}

// Replace is a complete imported VEVENT override. Absent fields remain absent.
func Replace(point Point, replacement Span, content EventContent) (Exception, error) {
	if !point.Valid() || !replacement.Valid() || point.kind != replacement.kind || !content.Valid() {
		return Exception{}, failure(InvalidOccurrence)
	}
	return Exception{kind: ReplaceOccurrence, original: point, replacement: replacement, content: content}, nil
}
func (e Exception) Kind() ExceptionKind {
	if !e.valid() {
		return ExceptionInvalid
	}
	return e.kind
}
func (e Exception) Original() Point { return e.original }
func (e Exception) Replacement() (Span, bool) {
	return e.replacement, e.valid() && (e.kind == MoveOccurrence || e.kind == ReplaceOccurrence)
}
func (e Exception) Content() (EventContent, bool) {
	return e.content, e.valid() && e.kind == ReplaceOccurrence
}
func (e Exception) valid() bool {
	if !e.original.Valid() {
		return false
	}
	switch e.kind {
	case CancelOccurrence:
		return e.replacement == (Span{}) && e.content == (EventContent{})
	case MoveOccurrence:
		return e.replacement.Valid() && e.replacement.kind == e.original.kind && e.content == (EventContent{})
	case ReplaceOccurrence:
		return e.replacement.Valid() && e.replacement.kind == e.original.kind && e.content.Valid()
	default:
		return false
	}
}

// EventInput is untrusted. NewEvent checks bounds before cloning slices and
// validates all original occurrence identities under one calculation budget.
type EventInput struct {
	Summary, Description, Location string
	Span                           Span
	Rule                           Rule
	Additions, Exclusions          []Point
	Exceptions                     []Exception
}
type eventContentKind uint8

const (
	contentInvalid eventContentKind = iota
	contentLocal
	contentImported
)

type ImportedEventInput struct {
	Content               EventContent
	Span                  Span
	Rule                  Rule
	Additions, Exclusions []Point
	Exceptions            []Exception
}
type Event struct {
	admitted                       bool
	contentKind                    eventContentKind
	content                        EventContent
	summary, description, location string
	span                           Span
	rule                           Rule
	additions, exclusions          []Point
	exceptions                     []Exception
}

func pointLess(a, b Point) int {
	if a.String() < b.String() {
		return -1
	}
	if a.String() > b.String() {
		return 1
	}
	return 0
}
func canonicalPoints(input []Point, kind TemporalKind) ([]Point, error) {
	for _, p := range input {
		if !p.Valid() || p.kind != kind {
			return nil, failure(InvalidOccurrence)
		}
	}
	points := slices.Clone(input)
	slices.SortFunc(points, pointLess)
	return slices.Compact(points), nil
}
func admitEventStructure(input EventInput) (Event, error) {
	return admitEventContent(input, contentLocal, EventContent{})
}
func localSpanSerializable(s Span) bool {
	if s.mode == spanAllDay {
		return s.Valid()
	}
	return s.Valid() && s.mode == spanExact && (s.source == sourceZoned || s.source == sourceExplicit) && s.zone.embedded == nil && s.endZone == (Zone{})
}
func admitEventContent(input EventInput, kind eventContentKind, content EventContent) (Event, error) {
	if !input.Span.Valid() || !input.Rule.validFor(input.Span) {
		return Event{}, failure(InvalidInput)
	}
	summary, description, location := input.Summary, input.Description, input.Location
	switch kind {
	case contentLocal:
		// Only the local variants admitted by the versioned SQL codec may
		// be written. Imported Point/span/content must retain every field.
		if !localSpanSerializable(input.Span) {
			return Event{}, failure(Incompatible)
		}
		for _, list := range [][]Point{input.Additions, input.Exclusions} {
			for _, point := range list {
				if point.origin != originCivil {
					return Event{}, failure(Incompatible)
				}
			}
		}
		for _, exception := range input.Exceptions {
			if exception.kind == ReplaceOccurrence || exception.original.origin != originCivil || exception.kind == MoveOccurrence && !localSpanSerializable(exception.replacement) {
				return Event{}, failure(Incompatible)
			}
		}
		if content != (EventContent{}) || !textValid(summary, 512, true) || !textValid(description, 4096, false) || !textValid(location, 512, false) {
			return Event{}, failure(InvalidInput)
		}
	case contentImported:
		if input.Span.source == sourceExplicit {
			return Event{}, failure(Incompatible)
		}
		if !content.Valid() || summary != "" || description != "" || location != "" {
			return Event{}, failure(InvalidInput)
		}
	case contentInvalid:
		return Event{}, failure(InvalidInput)
	default:
		return Event{}, failure(InvalidInput)
	}
	if len(input.Additions) > MaxDates || len(input.Exclusions) > MaxDates || len(input.Exceptions) > MaxExceptions {
		return Event{}, failure(CapacityExceeded)
	}
	additions, err := canonicalPoints(input.Additions, input.Span.kind)
	if err != nil {
		return Event{}, err
	}
	exclusions, err := canonicalPoints(input.Exclusions, input.Span.kind)
	if err != nil {
		return Event{}, err
	}
	exceptions := slices.Clone(input.Exceptions)
	slices.SortFunc(exceptions, func(a, b Exception) int { return pointLess(a.original, b.original) })
	for i, e := range exceptions {
		if !e.valid() || e.original.kind != input.Span.kind || i > 0 && exceptions[i-1].original == e.original {
			return Event{}, failure(InvalidOccurrence)
		}
		if _, present := slices.BinarySearchFunc(exclusions, e.original, pointLess); present {
			return Event{}, failure(InvalidOccurrence)
		}
	}
	event := Event{admitted: true, contentKind: kind, content: content, summary: strings.Clone(summary), description: strings.Clone(description), location: strings.Clone(location), span: input.Span, rule: cloneRule(input.Rule), additions: additions, exclusions: exclusions, exceptions: exceptions}
	return event, nil
}
func NewEvent(ctx context.Context, input EventInput) (Event, error) {
	return newEvent(ctx, input, contentLocal, EventContent{})
}
func NewImportedEvent(ctx context.Context, input ImportedEventInput) (Event, error) {
	owned, release, err := admissionScope(ctx)
	if err != nil {
		return Event{}, err
	}
	defer release()
	ctx = owned
	raw := EventInput{Span: input.Span, Rule: input.Rule, Additions: input.Additions, Exclusions: input.Exclusions, Exceptions: input.Exceptions}
	return newEvent(ctx, raw, contentImported, input.Content)
}
func newEvent(ctx context.Context, input EventInput, kind eventContentKind, content EventContent) (Event, error) {
	if err := contextValid(ctx); err != nil {
		return Event{}, err
	}
	if len(input.Additions) > MaxDates || len(input.Exclusions) > MaxDates || len(input.Exceptions) > MaxExceptions || len(input.Summary) > 512 || len(input.Description) > 4096 || len(input.Location) > 512 {
		return Event{}, failure(CapacityExceeded)
	}
	contentBytes := len(input.Summary) + len(input.Description) + len(input.Location)
	if kind == contentImported {
		if !content.Valid() {
			return Event{}, failure(InvalidInput)
		}
		contentBytes = content.byteLength()
	}
	for _, exception := range input.Exceptions {
		if exception.kind == ReplaceOccurrence {
			contentBytes += exception.content.byteLength()
		}
	}
	charge := eventBaseCharge + 3*int64(contentBytes+1024+256) + int64(len(input.Additions)+len(input.Exclusions))*pointCharge + int64(len(input.Exceptions))*exceptionCharge
	lease, err := images.reserve(charge)
	if err != nil {
		return Event{}, err
	}
	defer lease.release()
	event, err := admitEventContent(input, kind, content)
	if err != nil {
		return Event{}, err
	}
	budget := zoneBudget(ctx)
	ctx = withCalendarBudget(ctx, budget)
	if err = validateEvent(ctx, event, budget); err != nil {
		return Event{}, err
	}
	return event, nil
}
func (e Event) Valid() bool {
	if !e.admitted || !e.span.Valid() || !e.rule.validFor(e.span) || len(e.additions) > MaxDates || len(e.exclusions) > MaxDates || len(e.exceptions) > MaxExceptions {
		return false
	}
	switch e.contentKind {
	case contentLocal:
		if e.content != (EventContent{}) || !textValid(e.summary, 512, true) || !textValid(e.description, 4096, false) || !textValid(e.location, 512, false) {
			return false
		}
	case contentImported:
		if !e.content.Valid() || e.summary != "" || e.description != "" || e.location != "" {
			return false
		}
	case contentInvalid:
		return false
	default:
		return false
	}
	for _, points := range [][]Point{e.additions, e.exclusions} {
		for i, p := range points {
			if !p.Valid() || p.kind != e.span.kind || i > 0 && pointLess(points[i-1], p) >= 0 {
				return false
			}
		}
	}
	for i, exception := range e.exceptions {
		if !exception.valid() || exception.original.kind != e.span.kind || i > 0 && pointLess(e.exceptions[i-1].original, exception.original) >= 0 {
			return false
		}
	}
	return true
}
func (e Event) Summary() string {
	if e.contentKind == contentImported {
		return e.content.summary.ValueOrEmpty()
	}
	return e.summary
}
func (e Event) Description() string {
	if e.contentKind == contentImported {
		return e.content.description.ValueOrEmpty()
	}
	return e.description
}
func (e Event) Location() string {
	if e.contentKind == contentImported {
		return e.content.location.ValueOrEmpty()
	}
	return e.location
}
func (e Event) Content() (EventContent, bool) {
	return e.content, e.contentKind == contentImported && e.Valid()
}
func (e Event) Span() Span              { return e.span }
func (e Event) Rule() Rule              { return cloneRule(e.rule) }
func (e Event) Additions() []Point      { return slices.Clone(e.additions) }
func (e Event) Exclusions() []Point     { return slices.Clone(e.exclusions) }
func (e Event) Exceptions() []Exception { return slices.Clone(e.exceptions) }
func cloneEvent(e Event) Event {
	e.rule = cloneRule(e.rule)
	e.additions = slices.Clone(e.additions)
	e.exclusions = slices.Clone(e.exclusions)
	e.exceptions = slices.Clone(e.exceptions)
	return e
}
func (e Event) ramCharge() int64 {
	contentBytes := len(e.summary) + len(e.description) + len(e.location)
	if e.contentKind == contentImported {
		contentBytes = e.content.byteLength()
	}
	for _, exception := range e.exceptions {
		if exception.kind == ReplaceOccurrence {
			contentBytes += exception.content.byteLength()
		}
	}
	return eventBaseCharge + 3*int64(contentBytes+len(e.rule.canonical)+len(e.span.zone.name)+len(e.span.zone.DataID())) + int64(len(e.additions)+len(e.exclusions))*pointCharge + int64(len(e.exceptions))*exceptionCharge
}

type CalendarSnapshot struct {
	id        CalendarID
	name      string
	revision  Revision
	updatedAt time.Time
	remote    RemoteState
}

func (s CalendarSnapshot) Valid() bool {
	if !s.id.Valid() || !s.revision.validFor(s.id, EventUID{}) {
		return false
	}
	if s.id.origin == RemoteICS {
		if !s.remote.Valid() || s.revision.presence != Present {
			return false
		}
	} else if s.remote != (RemoteState{}) {
		return false
	}
	if s.revision.presence == Absent {
		return s.name == "" && s.updatedAt.IsZero()
	}
	return textValid(s.name, 512, true) && !s.updatedAt.IsZero()
}
func (s CalendarSnapshot) ID() CalendarID { return s.id }
func (s CalendarSnapshot) RemoteState() (RemoteState, bool) {
	return s.remote, s.Valid() && s.id.origin == RemoteICS
}
func (s CalendarSnapshot) Name() (string, bool) {
	return s.name, s.Valid() && s.revision.presence == Present
}
func (s CalendarSnapshot) Revision() Revision { return s.revision }
func (s CalendarSnapshot) UpdatedAt() (time.Time, bool) {
	return s.updatedAt, s.Valid() && s.revision.presence != Absent
}

type EventSnapshot struct {
	key                        EventKey
	definition                 Event
	revision, calendarRevision Revision
	updatedAt                  time.Time
}

func (s EventSnapshot) Valid() bool {
	if !s.key.Valid() || !s.revision.validFor(s.key.calendar, s.key.uid) || !s.calendarRevision.validFor(s.key.calendar, EventUID{}) || s.calendarRevision.presence != Present {
		return false
	}
	switch s.revision.presence {
	case Absent:
		return !s.definition.Valid() && s.updatedAt.IsZero()
	case Present:
		return s.definition.Valid() && !s.updatedAt.IsZero()
	case Removed:
		return !s.definition.Valid() && !s.updatedAt.IsZero()
	default:
		return false
	}
}
func (s EventSnapshot) Key() EventKey { return s.key }
func (s EventSnapshot) Event() (Event, bool) {
	return cloneEvent(s.definition), s.Valid() && s.revision.presence == Present
}
func (s EventSnapshot) Revision() Revision         { return s.revision }
func (s EventSnapshot) CalendarRevision() Revision { return s.calendarRevision }
func (s EventSnapshot) UpdatedAt() (time.Time, bool) {
	return s.updatedAt, s.Valid() && s.revision.presence != Absent
}
