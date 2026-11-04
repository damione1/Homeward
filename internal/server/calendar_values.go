package server

import (
	"context"
	"fmt"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/calendars"
	datepb "google.golang.org/genproto/googleapis/type/date"
	timepb "google.golang.org/genproto/googleapis/type/timeofday"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"time"
)

func calendarKnown(message proto.Message) bool {
	if !taskKnown(message) {
		return false
	}
	valid := true
	message.ProtoReflect().Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Kind() != protoreflect.MessageKind {
			return true
		}
		if field.IsList() {
			values := value.List()
			for i := 0; i < values.Len(); i++ {
				if !calendarKnown(values.Get(i).Message().Interface()) {
					valid = false
					return false
				}
			}
		} else if !calendarKnown(value.Message().Interface()) {
			valid = false
		}
		return valid
	})
	return valid
}
func calendarDate(raw *datepb.Date) (calendars.CivilDate, error) {
	if !calendarKnown(raw) || raw.Year < 1 || raw.Year > 9999 || raw.Month < 1 || raw.Month > 12 || raw.Day < 1 || raw.Day > 31 {
		return calendars.CivilDate{}, taskInvalid()
	}
	return calendars.ParseDate(fmt.Sprintf("%04d-%02d-%02d", raw.Year, raw.Month, raw.Day))
}
func calendarWireDate(value calendars.CivilDate) *datepb.Date {
	year, month, day, err := value.Components()
	if err != nil {
		return nil
	}
	return &datepb.Date{Year: int32(year), Month: int32(month), Day: int32(day)}
}
func calendarCivil(raw *api.CalendarCivilTime) (calendars.LocalDateTime, error) {
	if !calendarKnown(raw) || raw.Time == nil {
		return calendars.LocalDateTime{}, taskInvalid()
	}
	date, err := calendarDate(raw.Date)
	if err != nil {
		return calendars.LocalDateTime{}, err
	}
	clock := raw.Time
	if clock.Hours < 0 || clock.Hours > 23 || clock.Minutes < 0 || clock.Minutes > 59 || clock.Seconds < 0 || clock.Seconds > 59 || clock.Nanos != 0 {
		return calendars.LocalDateTime{}, taskInvalid()
	}
	return calendars.ParseLocalDateTime(fmt.Sprintf("%sT%02d:%02d:%02d", date.String(), clock.Hours, clock.Minutes, clock.Seconds))
}
func calendarWireCivil(value calendars.LocalDateTime) *api.CalendarCivilTime {
	if !value.Valid() {
		return nil
	}
	raw, err := time.Parse("2006-01-02T15:04:05", value.String())
	if err != nil {
		return nil
	}
	return &api.CalendarCivilTime{Date: &datepb.Date{Year: int32(raw.Year()), Month: int32(raw.Month()), Day: int32(raw.Day())}, Time: &timepb.TimeOfDay{Hours: int32(raw.Hour()), Minutes: int32(raw.Minute()), Seconds: int32(raw.Second())}}
}
func calendarInstant(raw *timestamppb.Timestamp) (calendars.Instant, error) {
	if !calendarKnown(raw) || raw.CheckValid() != nil || raw.Nanos != 0 {
		return calendars.Instant{}, taskInvalid()
	}
	return calendars.NewInstant(raw.AsTime())
}
func calendarWireInstant(value calendars.Instant) *timestamppb.Timestamp {
	at, ok := value.Time()
	if !ok {
		return nil
	}
	return timestamppb.New(at)
}
func calendarSpan(ctx context.Context, raw *api.CalendarSpan) (calendars.Span, error) {
	if !calendarKnown(raw) {
		return calendars.Span{}, taskInvalid()
	}
	switch value := raw.Value.(type) {
	case *api.CalendarSpan_Dates:
		if value == nil || value.Dates == nil {
			return calendars.Span{}, taskInvalid()
		}
		start, err := calendarDate(value.Dates.StartDate)
		if err != nil {
			return calendars.Span{}, err
		}
		end, err := calendarDate(value.Dates.EndDate)
		if err != nil {
			return calendars.Span{}, err
		}
		return calendars.AllDaySpan(start, end)
	case *api.CalendarSpan_Times:
		if value == nil || value.Times == nil {
			return calendars.Span{}, taskInvalid()
		}
		raw := value.Times
		zone, err := calendars.ParseZone(raw.TimeZone)
		if err != nil {
			return calendars.Span{}, err
		}
		anchor, err := calendarCivil(raw.StartLocal)
		if err != nil {
			return calendars.Span{}, err
		}
		selected, err := calendarInstant(raw.StartTime)
		if err != nil {
			return calendars.Span{}, err
		}
		end, err := calendarInstant(raw.EndTime)
		if err != nil {
			return calendars.Span{}, err
		}
		expectedEnd, err := calendarCivil(raw.EndLocal)
		if err != nil {
			return calendars.Span{}, err
		}
		span, err := calendars.SelectedTimedSpan(ctx, anchor, selected, end, zone)
		if err != nil {
			return calendars.Span{}, err
		}
		actualEnd, err := span.EndLocal(ctx)
		if err != nil || actualEnd != expectedEnd {
			return calendars.Span{}, taskInvalid()
		}
		return span, nil
	default:
		return calendars.Span{}, taskInvalid()
	}
}
func calendarWireSpan(ctx context.Context, value calendars.Span) (*api.CalendarSpan, error) {
	if start, end, ok := value.Dates(); ok {
		return &api.CalendarSpan{Value: &api.CalendarSpan_Dates{Dates: &api.CalendarDateSpan{StartDate: calendarWireDate(start), EndDate: calendarWireDate(end)}}}, nil
	}
	start, end, zone, ok := value.Instants()
	if !ok {
		return nil, taskInvalid()
	}
	anchor, ok := value.Anchor()
	if !ok {
		return nil, taskInvalid()
	}
	localEnd, err := value.EndLocal(ctx)
	if err != nil {
		return nil, err
	}
	return &api.CalendarSpan{Value: &api.CalendarSpan_Times{Times: &api.CalendarTimeSpan{StartLocal: calendarWireCivil(anchor), EndLocal: calendarWireCivil(localEnd), StartTime: calendarWireInstant(start), EndTime: calendarWireInstant(end), TimeZone: zone.Name()}}}, nil
}
func calendarPoint(kind calendars.TemporalKind, raw *api.CalendarRecurrencePoint) (calendars.Point, error) {
	if !calendarKnown(raw) {
		return calendars.Point{}, taskInvalid()
	}
	switch value := raw.Value.(type) {
	case *api.CalendarRecurrencePoint_Date:
		if value == nil || kind != calendars.AllDay {
			return calendars.Point{}, taskInvalid()
		}
		date, err := calendarDate(value.Date)
		if err != nil {
			return calendars.Point{}, err
		}
		return calendars.ParsePoint(kind, date.String())
	case *api.CalendarRecurrencePoint_Resolved:
		if value == nil || value.Resolved == nil || kind != calendars.Timed {
			return calendars.Point{}, taskInvalid()
		}
		local, err := calendarCivil(value.Resolved.Local)
		if err != nil {
			return calendars.Point{}, err
		}
		instant, err := calendarInstant(value.Resolved.SelectedTime)
		if err != nil {
			return calendars.Point{}, err
		}
		return calendars.ParsePoint(kind, local.String()+"@"+instant.String())
	case *api.CalendarRecurrencePoint_Local:
		if value == nil || kind != calendars.Timed {
			return calendars.Point{}, taskInvalid()
		}
		local, err := calendarCivil(value.Local)
		if err != nil {
			return calendars.Point{}, err
		}
		return calendars.ParsePoint(kind, local.String())
	default:
		return calendars.Point{}, taskInvalid()
	}
}
func calendarWirePoint(point calendars.Point) *api.CalendarRecurrencePoint {
	if !point.Valid() {
		return nil
	}
	if local, instant, resolved := point.ResolvedPoint(); resolved {
		return &api.CalendarRecurrencePoint{Value: &api.CalendarRecurrencePoint_Resolved{Resolved: &api.CalendarResolvedRecurrencePoint{Local: calendarWireCivil(local), SelectedTime: calendarWireInstant(instant)}}}
	}
	if len(point.String()) == 10 {
		date, err := calendars.ParseDate(point.String())
		if err != nil {
			return nil
		}
		return &api.CalendarRecurrencePoint{Value: &api.CalendarRecurrencePoint_Date{Date: calendarWireDate(date)}}
	}
	local, err := calendars.ParseLocalDateTime(point.String())
	if err != nil {
		return nil
	}
	return &api.CalendarRecurrencePoint{Value: &api.CalendarRecurrencePoint_Local{Local: calendarWireCivil(local)}}
}
func calendarEvent(ctx context.Context, raw *api.CalendarEvent) (calendars.Event, error) {
	if raw == nil || len(raw.Additions) > calendars.MaxDates || len(raw.Exclusions) > calendars.MaxDates || len(raw.Exceptions) > calendars.MaxExceptions {
		return calendars.Event{}, taskInvalid()
	}
	span, err := calendarSpan(ctx, raw.Span)
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
	for _, raw := range raw.Additions {
		point, err := calendarPoint(span.Kind(), raw)
		if err != nil {
			return calendars.Event{}, err
		}
		input.Additions = append(input.Additions, point)
	}
	for _, raw := range raw.Exclusions {
		point, err := calendarPoint(span.Kind(), raw)
		if err != nil {
			return calendars.Event{}, err
		}
		input.Exclusions = append(input.Exclusions, point)
	}
	for _, raw := range raw.Exceptions {
		if !calendarKnown(raw) || raw.ReplacementSummary != nil || raw.ReplacementDescription != nil || raw.ReplacementLocation != nil {
			return calendars.Event{}, taskInvalid()
		}
		point, err := calendarPoint(span.Kind(), raw.Original)
		if err != nil {
			return calendars.Event{}, err
		}
		var exception calendars.Exception
		switch change := raw.Change.(type) {
		case *api.CalendarException_Cancel:
			if change == nil || change.Cancel == nil {
				return calendars.Event{}, taskInvalid()
			}
			exception, err = calendars.Cancel(point)
		case *api.CalendarException_Replacement:
			if change == nil {
				return calendars.Event{}, taskInvalid()
			}
			replacement, e := calendarSpan(ctx, change.Replacement)
			if e != nil {
				return calendars.Event{}, e
			}
			exception, err = calendars.Move(point, replacement)
		default:
			return calendars.Event{}, taskInvalid()
		}
		if err != nil {
			return calendars.Event{}, err
		}
		input.Exceptions = append(input.Exceptions, exception)
	}
	return calendars.NewEvent(ctx, input)
}
func calendarWireEventDefinition(ctx context.Context, value calendars.Event) (*api.CalendarEvent, error) {
	span, err := calendarWireSpan(ctx, value.Span())
	if err != nil {
		return nil, err
	}
	out := &api.CalendarEvent{Summary: value.Summary(), Description: value.Description(), Location: value.Location(), Span: span}
	if text, ok := value.Rule().Text(); ok {
		out.RecurrenceRule = &text
	}
	for _, point := range value.Additions() {
		out.Additions = append(out.Additions, calendarWirePoint(point))
	}
	for _, point := range value.Exclusions() {
		out.Exclusions = append(out.Exclusions, calendarWirePoint(point))
	}
	for _, exception := range value.Exceptions() {
		raw := &api.CalendarException{Original: calendarWirePoint(exception.Original())}
		switch exception.Kind() {
		case calendars.CancelOccurrence:
			raw.Change = &api.CalendarException_Cancel{Cancel: &emptypb.Empty{}}
		case calendars.MoveOccurrence, calendars.ReplaceOccurrence:
			replacement, ok := exception.Replacement()
			if !ok {
				return nil, taskInvalid()
			}
			span, err := calendarWireSpan(ctx, replacement)
			if err != nil {
				return nil, err
			}
			raw.Change = &api.CalendarException_Replacement{Replacement: span}
		case calendars.ExceptionInvalid:
			return nil, taskInvalid()
		default:
			return nil, taskInvalid()
		}
		if content, ok := exception.Content(); ok {
			if text, present := content.Summary().Value(); present {
				raw.ReplacementSummary = &text
			}
			if text, present := content.Description().Value(); present {
				raw.ReplacementDescription = &text
			}
			if text, present := content.Location().Value(); present {
				raw.ReplacementLocation = &text
			}
		}
		out.Exceptions = append(out.Exceptions, raw)
	}
	return out, nil
}
func calendarWindow(ctx context.Context, raw *api.CalendarWindow) (calendars.Window, calendars.Zone, error) {
	if !calendarKnown(raw) {
		return calendars.Window{}, calendars.Zone{}, taskInvalid()
	}
	zone, err := calendars.ParseZone(raw.TimeZone)
	if err != nil {
		return calendars.Window{}, zone, err
	}
	switch bounds := raw.Bounds.(type) {
	case *api.CalendarWindow_Dates:
		if bounds == nil || bounds.Dates == nil {
			return calendars.Window{}, zone, taskInvalid()
		}
		start, err := calendarDate(bounds.Dates.StartDate)
		if err != nil {
			return calendars.Window{}, zone, err
		}
		end, err := calendarDate(bounds.Dates.EndDate)
		if err != nil {
			return calendars.Window{}, zone, err
		}
		window, err := calendars.AgendaWindow(ctx, start, end, zone)
		return window, zone, err
	case *api.CalendarWindow_Times:
		if bounds == nil || bounds.Times == nil {
			return calendars.Window{}, zone, taskInvalid()
		}
		start, err := calendarInstant(bounds.Times.StartTime)
		if err != nil {
			return calendars.Window{}, zone, err
		}
		end, err := calendarInstant(bounds.Times.EndTime)
		if err != nil {
			return calendars.Window{}, zone, err
		}
		window, err := calendars.TimeWindow(start, end, zone)
		return window, zone, err
	default:
		return calendars.Window{}, zone, taskInvalid()
	}
}
