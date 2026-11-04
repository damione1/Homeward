package server

import (
	"connectrpc.com/connect"
	"context"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/calendarnames"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/workday"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"math"
	"time"
)

func (h *calendarResourceHandler) ResolveCalendarTime(ctx context.Context, request *connect.Request[api.ResolveCalendarTimeRequest]) (*connect.Response[api.ResolveCalendarTimeResponse], error) {
	raw := taskRequest(request)
	return withCalendarResource(h, ctx, raw.GetName(), auth.ScopeOperation_SCOPE_OPERATION_READ, func(ctx context.Context, _ calendarnames.CalendarName, _ calendars.CalendarID) (*connect.Response[api.ResolveCalendarTimeResponse], error) {
		if !calendarKnown(raw) {
			return nil, taskInvalid()
		}
		local, err := calendarCivil(raw.Local)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		out := &api.ResolveCalendarTimeResponse{Local: proto.CloneOf(raw.Local), TimeZone: raw.TimeZone}
		zone, err := calendars.ParseZone(raw.TimeZone)
		if err != nil {
			out.Resolution = api.CalendarTimeResolution_CALENDAR_TIME_RESOLUTION_INVALID_ZONE
			return connect.NewResponse(out), nil
		}
		candidates, err := calendars.ResolveLocalTime(ctx, local, zone)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		switch len(candidates) {
		case 0:
			out.Resolution = api.CalendarTimeResolution_CALENDAR_TIME_RESOLUTION_NONEXISTENT
		case 1:
			out.Resolution = api.CalendarTimeResolution_CALENDAR_TIME_RESOLUTION_UNIQUE
		case 2:
			out.Resolution = api.CalendarTimeResolution_CALENDAR_TIME_RESOLUTION_AMBIGUOUS
		default:
			return nil, calendarRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY))
		}
		for _, candidate := range candidates {
			out.Candidates = append(out.Candidates, &api.CalendarTimeCandidate{SelectedTime: calendarWireInstant(candidate.Instant()), UtcOffset: int32(candidate.OffsetSeconds())})
		}
		return connect.NewResponse(out), nil
	})
}
func (h *calendarResourceHandler) WatchCalendar(ctx context.Context, request *connect.Request[api.WatchCalendarRequest], stream *connect.ServerStream[api.WatchCalendarResponse]) error {
	original := ctx
	raw := taskRequest(request)
	_, err := withCalendarResource(h, ctx, raw.GetName(), auth.ScopeOperation_SCOPE_OPERATION_READ, func(ctx context.Context, name calendarnames.CalendarName, id calendars.CalendarID) (struct{}, error) {
		if !taskKnown(raw) || stream == nil {
			return struct{}{}, taskInvalid()
		}
		defer interruptCoreRPCWriteOnCancel(original, ctx)()
		sub, err := h.service.Subscribe()
		if err != nil {
			return struct{}{}, calendarRPCError(original, err)
		}
		defer sub.Close()
		sequence := int64(0)
		send := func(value *api.WatchCalendarResponse) error {
			if ctx.Err() != nil {
				return apiRPCError(original, ctx.Err())
			}
			if sequence == math.MaxInt64 {
				return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED).Connect()
			}
			sequence++
			value.Sequence = sequence
			return stream.Send(value)
		}
		if err := send(&api.WatchCalendarResponse{Frame: &api.WatchCalendarResponse_Ready{Ready: &emptypb.Empty{}}}); err != nil {
			return struct{}{}, err
		}
		if id.Origin() == calendars.Calculated {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return struct{}{}, apiRPCError(original, ctx.Err())
				case <-ticker.C:
					if workday.ValidateReader(h.workday) != nil {
						if err := send(&api.WatchCalendarResponse{Frame: &api.WatchCalendarResponse_ResyncRequired{ResyncRequired: &emptypb.Empty{}}}); err != nil {
							return struct{}{}, err
						}
						return struct{}{}, nil
					}
				}
			}
		}
		for {
			change, err := sub.Next(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return struct{}{}, apiRPCError(original, ctx.Err())
				}
				if err := send(&api.WatchCalendarResponse{Frame: &api.WatchCalendarResponse_ResyncRequired{ResyncRequired: &emptypb.Empty{}}}); err != nil {
					return struct{}{}, err
				}
				return struct{}{}, nil
			}
			if change.Calendar() != id {
				continue
			}
			number, ok := change.Revision().Number()
			if !ok {
				return struct{}{}, taskInvalid()
			}
			etag, err := calendarnames.Etag(name.String(), number)
			if err != nil {
				return struct{}{}, calendarRPCError(original, err)
			}
			if err := send(&api.WatchCalendarResponse{Frame: &api.WatchCalendarResponse_Changed{Changed: &api.CalendarChanged{Etag: etag}}}); err != nil {
				return struct{}{}, err
			}
		}
	})
	return err
}
