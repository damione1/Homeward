package calendars

import (
	"context"
	"slices"
	"strings"
)

// EventField is the closed editable definition field set. Time spans are atomic:
// a selected local anchor and exact instant must remain a consistent DST choice.
type EventField uint8

const (
	EventSummary EventField = iota + 1
	EventDescription
	EventLocation
	EventSpan
	EventRule
	EventAdditions
	EventExclusions
	EventExceptions
)

type EventPatchInput struct {
	Summary, Description, Location string
	Span                           Span
	RecurrenceRule                 *string
	Additions, Exclusions          []Point
	Exceptions                     []Exception
}
type EventPatch struct {
	valid  bool
	fields uint16
	input  EventPatchInput
}

// The four-MiB reservation covers bounded patch input, a merged event and result
// copies. Loading the current committed image grows that SAME ledger slot.
const eventPatchCharge int64 = 4 * 1024 * 1024

func NewEventPatch(input EventPatchInput, fields []EventField) (EventPatch, error) {
	var mask uint16
	for _, field := range fields {
		if field < EventSummary || field > EventExceptions {
			return EventPatch{}, failure(InvalidInput)
		}
		mask |= 1 << (field - 1)
	}
	selected := func(field EventField) bool { return mask&(1<<(field-1)) != 0 }
	if selected(EventSummary) && !textValid(input.Summary, 512, true) || selected(EventDescription) && !textValid(input.Description, 4096, false) || selected(EventLocation) && !textValid(input.Location, 512, false) {
		return EventPatch{}, failure(InvalidInput)
	}
	if selected(EventSpan) && !localSpanSerializable(input.Span) {
		return EventPatch{}, failure(Incompatible)
	}
	if selected(EventRule) && input.RecurrenceRule != nil && (len(*input.RecurrenceRule) == 0 || len(*input.RecurrenceRule) > 1024) {
		return EventPatch{}, failure(UnsupportedRule)
	}
	if len(input.Additions) > MaxDates || len(input.Exclusions) > MaxDates || len(input.Exceptions) > MaxExceptions {
		return EventPatch{}, failure(CapacityExceeded)
	}
	for field, points := range map[EventField][]Point{EventAdditions: input.Additions, EventExclusions: input.Exclusions} {
		if selected(field) {
			for _, point := range points {
				if !point.Valid() || point.origin != originCivil {
					return EventPatch{}, failure(InvalidOccurrence)
				}
			}
		}
	}
	if selected(EventExceptions) {
		for _, exception := range input.Exceptions {
			if !exception.valid() || exception.kind == ReplaceOccurrence || exception.original.origin != originCivil || exception.kind == MoveOccurrence && !localSpanSerializable(exception.replacement) {
				return EventPatch{}, failure(Incompatible)
			}
		}
	}
	copy := input
	copy.Summary = strings.Clone(input.Summary)
	copy.Description = strings.Clone(input.Description)
	copy.Location = strings.Clone(input.Location)
	if input.RecurrenceRule != nil {
		text := strings.Clone(*input.RecurrenceRule)
		copy.RecurrenceRule = &text
	}
	copy.Additions = slices.Clone(input.Additions)
	copy.Exclusions = slices.Clone(input.Exclusions)
	copy.Exceptions = slices.Clone(input.Exceptions)
	return EventPatch{true, mask, copy}, nil
}
func (p EventPatch) selected(field EventField) bool { return p.fields&(1<<(field-1)) != 0 }
func (p EventPatch) apply(ctx context.Context, current Event) (Event, error) {
	if !p.valid || !current.Valid() || current.contentKind != contentLocal {
		return Event{}, failure(InvalidInput)
	}
	input := EventInput{Summary: current.summary, Description: current.description, Location: current.location, Span: current.span, Rule: current.rule, Additions: current.additions, Exclusions: current.exclusions, Exceptions: current.exceptions}
	if p.selected(EventSummary) {
		input.Summary = p.input.Summary
	}
	if p.selected(EventDescription) {
		input.Description = p.input.Description
	}
	if p.selected(EventLocation) {
		input.Location = p.input.Location
	}
	if p.selected(EventSpan) {
		input.Span = p.input.Span
	}
	if p.selected(EventRule) {
		input.Rule = NoRecurrence()
		if p.input.RecurrenceRule != nil {
			var err error
			input.Rule, err = ParseRule(*p.input.RecurrenceRule, input.Span)
			if err != nil {
				return Event{}, err
			}
		}
	} else if p.selected(EventSpan) {
		if text, present := current.rule.Text(); present {
			var err error
			input.Rule, err = ParseRule(text, input.Span)
			if err != nil {
				return Event{}, err
			}
		}
	}
	if p.selected(EventAdditions) {
		input.Additions = p.input.Additions
	}
	if p.selected(EventExclusions) {
		input.Exclusions = p.input.Exclusions
	}
	if p.selected(EventExceptions) {
		input.Exceptions = p.input.Exceptions
	}
	return NewEvent(ctx, input)
}
