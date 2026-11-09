package server

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/calendarnames"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/workday"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func occurrenceParent(raw string) (calendarnames.CalendarName, *calendarnames.EventName, error) {
	if parent, all := strings.CutSuffix(raw, "/events/-"); all {
		name, err := calendarnames.ParseCalendar(parent)
		return name, nil, err
	}
	event, err := calendarnames.ParseEvent(raw)
	if err != nil {
		return calendarnames.CalendarName{}, nil, err
	}
	return event.Parent(), &event, nil
}

// Occurrence spans are a view in the request zone. Event definitions and
// recurrence identities retain their original source-local semantics.
func calendarOccurrenceSpan(ctx context.Context, value calendars.Span, zone calendars.Zone) (*api.CalendarSpan, error) {
	if !zone.Valid() {
		return nil, taskInvalid()
	}
	if value.Kind() == calendars.AllDay {
		return calendarWireSpan(ctx, value)
	}
	start, end, _, ok := value.Instants()
	if !ok {
		return nil, taskInvalid()
	}
	localStart, err := zone.LocalTime(ctx, start)
	if err != nil {
		return nil, err
	}
	localEnd, err := zone.LocalTime(ctx, end)
	if err != nil {
		return nil, err
	}
	return &api.CalendarSpan{Value: &api.CalendarSpan_Times{Times: &api.CalendarTimeSpan{
		StartLocal: calendarWireCivil(localStart), EndLocal: calendarWireCivil(localEnd),
		StartTime: calendarWireInstant(start), EndTime: calendarWireInstant(end), TimeZone: zone.Name(),
	}}}, nil
}

func calendarOccurrenceWire(ctx context.Context, parent calendarnames.CalendarName, value calendars.Occurrence, zone calendars.Zone) (*api.Occurrence, error) {
	event, err := calendarnames.Event(parent, value.ID().Event().UID())
	if err != nil {
		return nil, err
	}
	span, err := calendarOccurrenceSpan(ctx, value.Span(), zone)
	if err != nil {
		return nil, err
	}
	point := calendarWirePoint(value.ID().Point())
	if point == nil {
		return nil, taskInvalid()
	}
	return &api.Occurrence{CalendarEvent: event.String(), RecurrencePoint: point, Span: span, TimeZone: zone.Name(), Summary: value.Summary(), Description: value.Description(), Location: value.Location()}, nil
}
func (h *calendarResourceHandler) QueryOccurrences(ctx context.Context, request *connect.Request[api.QueryOccurrencesRequest]) (*connect.Response[api.QueryOccurrencesResponse], error) {
	raw := taskRequest(request)
	parent, selected, err := occurrenceParent(raw.GetParent())
	if err != nil {
		return nil, taskDenied()
	}
	return withCalendarResource(h, ctx, parent.String(), auth.ScopeOperation_SCOPE_OPERATION_READ, func(ctx context.Context, parent calendarnames.CalendarName, id calendars.CalendarID) (*connect.Response[api.QueryOccurrencesResponse], error) {
		if !taskKnown(raw) {
			return nil, taskInvalid()
		}
		window, zone, err := calendarWindow(ctx, raw.Window)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		limit, err := calendarPageSize(raw.PageSize, calendars.MaxPageSize)
		if err != nil {
			return nil, err
		}
		binding, err := h.queryBinding(ctx, apiv2connect.CalendarServiceQueryOccurrencesProcedure, raw.Parent)
		if err != nil {
			return nil, err
		}
		encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(raw.Window)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		hash := sha256.New()
		hash.Write(binding[:])
		hash.Write(encoded)
		copy(binding[:], hash.Sum(nil))
		if id.Origin() == calendars.Calculated {
			value, err := h.calculatedOccurrences(ctx, parent, id, window, zone, raw, binding, selected, limit)
			if err != nil {
				return nil, calendarRPCError(ctx, err)
			}
			return connect.NewResponse(value), nil
		}
		cursor, err := h.cursors.Decode(raw.PageToken, id, window, binding)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		page, err := h.service.Query(ctx, id, window, limit, cursor)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		number, ok := page.Revision().Number()
		if !ok {
			return nil, taskInvalid()
		}
		etag, err := calendarnames.Etag(parent.String(), number)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		state, remote := page.RemoteState()
		out := &api.QueryOccurrencesResponse{CalendarEtag: etag, Freshness: calendarWireFreshness(state, remote), Provenance: h.provenance}
		for _, occurrence := range page.Items() {
			if selected != nil && occurrence.ID().Event().UID() != selected.UID() {
				continue
			}
			value, err := calendarOccurrenceWire(ctx, parent, occurrence, zone)
			if err != nil {
				return nil, calendarRPCError(ctx, err)
			}
			out.Occurrences = append(out.Occurrences, value)
		}
		definitions, err := h.service.ListEvents(ctx, id)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		selectedPresent := selected == nil
		for _, snapshot := range definitions {
			if snapshot.CalendarRevision() != page.Revision() {
				return nil, calendarRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED))
			}
			if selected != nil && snapshot.Key().UID() != selected.UID() {
				continue
			}
			event, present := snapshot.Event()
			if selected != nil && snapshot.Key().UID() == selected.UID() && present {
				selectedPresent = true
			}
			if !present || len(event.Exceptions()) == 0 && len(event.Exclusions()) == 0 {
				continue
			}
			definition, err := calendarWireEventDefinition(ctx, event)
			if err != nil {
				return nil, calendarRPCError(ctx, err)
			}
			name, err := calendarnames.Event(parent, snapshot.Key().UID())
			if err != nil {
				return nil, calendarRPCError(ctx, err)
			}
			out.SeriesExceptions = append(out.SeriesExceptions, &api.CalendarSeriesExceptions{CalendarEvent: name.String(), Exceptions: definition.Exceptions, Exclusions: definition.Exclusions})
			if proto.Size(out) > 1<<20 {
				return nil, calendarRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED))
			}
		}
		current, err := h.service.GetCalendar(ctx, id)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		if current.Revision() != page.Revision() {
			return nil, calendarRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED))
		}
		if !selectedPresent {
			return nil, calendarRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND))
		}
		if until, present := state.FreshUntil(); remote && present {
			out.FreshUntilTime = timestamppb.New(until)
		}
		if next, more := page.Next(); more {
			out.NextPageToken, err = h.cursors.Encode(next, binding)
			if err != nil {
				return nil, calendarRPCError(ctx, err)
			}
		}
		if proto.Size(out) > 1<<20 {
			return nil, calendarRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED))
		}
		return connect.NewResponse(out), nil
	})
}
func (h *calendarResourceHandler) calculatedOccurrences(ctx context.Context, parent calendarnames.CalendarName, id calendars.CalendarID, window calendars.Window, zone calendars.Zone, raw *api.QueryOccurrencesRequest, binding [32]byte, selected *calendarnames.EventName, limit int) (*api.QueryOccurrencesResponse, error) {
	if workday.ValidateReader(h.workday) != nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	bound, err := h.workday.CalendarID()
	if err != nil || bound != id {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	from, to, err := window.CivilBounds()
	if err != nil {
		return nil, err
	}
	if _, timed := raw.Window.Bounds.(*api.CalendarWindow_Times); timed {
		to, err = to.AddDays(1)
		if err != nil {
			return nil, err
		}
	}
	var spans []calendars.Span
	split, splitErr := from.AddDays(366)
	if splitErr == nil && split.String() < to.String() {
		first, err := h.workday.Dates(ctx, from, split)
		if err != nil {
			return nil, err
		}
		second, err := h.workday.Dates(ctx, split, to)
		if err != nil {
			return nil, err
		}
		spans = append(first, second...)
	} else {
		spans, err = h.workday.Dates(ctx, from, to)
		if err != nil {
			return nil, err
		}
	}
	var included []calendars.Span
	for _, span := range spans {
		inside, err := window.ContainsSpan(ctx, span)
		if err != nil {
			return nil, err
		}
		if inside {
			included = append(included, span)
		}
	}
	capture := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%s:%s", parent.String(), workday.ProfileVersion, from.String(), to.String())))
	offset, err := h.pageOffset(raw.PageToken, binding, capture, len(included))
	if err != nil {
		return nil, err
	}
	end := min(offset+limit, len(included))
	etag, err := calendarnames.Etag(parent.String(), 1)
	if err != nil {
		return nil, err
	}
	out := &api.QueryOccurrencesResponse{CalendarEtag: etag, Freshness: api.CalendarFreshness_CALENDAR_FRESHNESS_FRESH, Provenance: h.provenance}
	for _, span := range included[offset:end] {
		date, _, _ := span.Dates()
		uid, err := calendars.ParseUID("workday:" + workday.ProfileVersion + ":" + date.String())
		if err != nil {
			return nil, err
		}
		if selected != nil && uid != selected.UID() {
			continue
		}
		event, err := calendarnames.Event(parent, uid)
		if err != nil {
			return nil, err
		}
		point, err := calendars.DatePoint(date)
		if err != nil {
			return nil, err
		}
		wireSpan, err := calendarWireSpan(ctx, span)
		if err != nil {
			return nil, err
		}
		out.Occurrences = append(out.Occurrences, &api.Occurrence{CalendarEvent: event.String(), RecurrencePoint: calendarWirePoint(point), Span: wireSpan, TimeZone: zone.Name(), Summary: "Workday"})
	}
	out.NextPageToken, err = h.pageToken(end, len(included), binding, capture)
	return out, err
}
