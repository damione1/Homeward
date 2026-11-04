package server

import (
	"context"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/calendars"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"slices"
)

var calendarEventEditable = []calendars.EventField{calendars.EventSummary, calendars.EventDescription, calendars.EventLocation, calendars.EventSpan, calendars.EventRule, calendars.EventAdditions, calendars.EventExclusions, calendars.EventExceptions}

func calendarEventMask(raw *api.CalendarEvent, mask *fieldmaskpb.FieldMask) ([]calendars.EventField, error) {
	if raw == nil {
		return nil, taskInvalid()
	}
	if mask == nil {
		var fields []calendars.EventField
		if raw.Summary != "" {
			fields = append(fields, calendars.EventSummary)
		}
		if raw.Description != "" {
			fields = append(fields, calendars.EventDescription)
		}
		if raw.Location != "" {
			fields = append(fields, calendars.EventLocation)
		}
		if raw.Span != nil {
			fields = append(fields, calendars.EventSpan)
		}
		if raw.RecurrenceRule != nil {
			fields = append(fields, calendars.EventRule)
		}
		if len(raw.Additions) > 0 {
			fields = append(fields, calendars.EventAdditions)
		}
		if len(raw.Exclusions) > 0 {
			fields = append(fields, calendars.EventExclusions)
		}
		if len(raw.Exceptions) > 0 {
			fields = append(fields, calendars.EventExceptions)
		}
		return fields, nil
	}
	if !taskKnown(mask) || len(mask.Paths) > 64 {
		return nil, taskInvalid()
	}
	if len(mask.Paths) == 1 && mask.Paths[0] == "*" {
		return slices.Clone(calendarEventEditable), nil
	}
	var fields []calendars.EventField
	seen := map[calendars.EventField]bool{}
	for _, path := range mask.Paths {
		var field calendars.EventField
		switch path {
		case "summary":
			field = calendars.EventSummary
		case "description":
			field = calendars.EventDescription
		case "location":
			field = calendars.EventLocation
		case "span":
			field = calendars.EventSpan
		case "recurrence_rule":
			field = calendars.EventRule
		case "additions":
			field = calendars.EventAdditions
		case "exclusions":
			field = calendars.EventExclusions
		case "exceptions":
			field = calendars.EventExceptions
		case "name", "etag", "calendar_etag", "update_time":
			continue
		default:
			return nil, taskInvalid()
		}
		if !seen[field] {
			seen[field] = true
			fields = append(fields, field)
		}
	}
	return fields, nil
}
func calendarAnyPoint(raw *api.CalendarRecurrencePoint) (calendars.Point, error) {
	if raw == nil {
		return calendars.Point{}, taskInvalid()
	}
	switch raw.Value.(type) {
	case *api.CalendarRecurrencePoint_Date:
		return calendarPoint(calendars.AllDay, raw)
	case *api.CalendarRecurrencePoint_Local, *api.CalendarRecurrencePoint_Resolved:
		return calendarPoint(calendars.Timed, raw)
	default:
		return calendars.Point{}, taskInvalid()
	}
}
func calendarPatchInput(ctx context.Context, raw *api.CalendarEvent, fields []calendars.EventField) (calendars.EventPatchInput, error) {
	var input calendars.EventPatchInput
	if raw == nil || !taskKnown(raw) {
		return input, taskInvalid()
	}
	for _, field := range fields {
		switch field {
		case calendars.EventSummary:
			input.Summary = raw.Summary
		case calendars.EventDescription:
			input.Description = raw.Description
		case calendars.EventLocation:
			input.Location = raw.Location
		case calendars.EventSpan:
			value, err := calendarSpan(ctx, raw.Span)
			if err != nil {
				return input, err
			}
			input.Span = value
		case calendars.EventRule:
			input.RecurrenceRule = raw.RecurrenceRule
		case calendars.EventAdditions, calendars.EventExclusions:
			values := raw.Additions
			if field == calendars.EventExclusions {
				values = raw.Exclusions
			}
			if len(values) > calendars.MaxDates {
				return input, taskInvalid()
			}
			points := make([]calendars.Point, 0, len(values))
			for _, raw := range values {
				point, err := calendarAnyPoint(raw)
				if err != nil {
					return input, err
				}
				points = append(points, point)
			}
			if field == calendars.EventAdditions {
				input.Additions = points
			} else {
				input.Exclusions = points
			}
		case calendars.EventExceptions:
			if len(raw.Exceptions) > calendars.MaxExceptions {
				return input, taskInvalid()
			}
			for _, value := range raw.Exceptions {
				if !calendarKnown(value) || value.ReplacementSummary != nil || value.ReplacementDescription != nil || value.ReplacementLocation != nil {
					return input, taskInvalid()
				}
				point, err := calendarAnyPoint(value.Original)
				if err != nil {
					return input, err
				}
				var exception calendars.Exception
				switch change := value.Change.(type) {
				case *api.CalendarException_Cancel:
					if change == nil || change.Cancel == nil {
						return input, taskInvalid()
					}
					exception, err = calendars.Cancel(point)
				case *api.CalendarException_Replacement:
					if change == nil {
						return input, taskInvalid()
					}
					replacement, e := calendarSpan(ctx, change.Replacement)
					if e != nil {
						return input, e
					}
					exception, err = calendars.Move(point, replacement)
				default:
					return input, taskInvalid()
				}
				if err != nil {
					return input, err
				}
				input.Exceptions = append(input.Exceptions, exception)
			}
		default:
			return input, taskInvalid()
		}
	}
	return input, nil
}
func calendarNormalizedPatch(raw *api.UpdateCalendarEventRequest, fields []calendars.EventField) *api.UpdateCalendarEventRequest {
	original := raw.CalendarEvent
	out := &api.UpdateCalendarEventRequest{CalendarEvent: &api.CalendarEvent{Name: original.Name, Etag: original.Etag}, UpdateMask: &fieldmaskpb.FieldMask{}}
	// Iterate the authoritative closed field order so duplicate/mask ordering
	// differences cannot manufacture a different intention fingerprint.
	for _, field := range calendarEventEditable {
		if !slices.Contains(fields, field) {
			continue
		}
		var path string
		switch field {
		case calendars.EventSummary:
			path = "summary"
			out.CalendarEvent.Summary = original.Summary
		case calendars.EventDescription:
			path = "description"
			out.CalendarEvent.Description = original.Description
		case calendars.EventLocation:
			path = "location"
			out.CalendarEvent.Location = original.Location
		case calendars.EventSpan:
			path = "span"
			out.CalendarEvent.Span = original.Span
		case calendars.EventRule:
			path = "recurrence_rule"
			out.CalendarEvent.RecurrenceRule = original.RecurrenceRule
		case calendars.EventAdditions:
			path = "additions"
			out.CalendarEvent.Additions = original.Additions
		case calendars.EventExclusions:
			path = "exclusions"
			out.CalendarEvent.Exclusions = original.Exclusions
		case calendars.EventExceptions:
			path = "exceptions"
			out.CalendarEvent.Exceptions = original.Exceptions
		default:
			continue
		}
		out.UpdateMask.Paths = append(out.UpdateMask.Paths, path)
	}
	return out
}
