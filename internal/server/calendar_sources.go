package server

import (
	"context"
	"slices"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/calendarnames"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/workday"
)

// QueryCalendarSources retains source discovery for a caller who has calendar
// rights but no universal metadata grant. An absent local Calendar is not
// represented as an existing resource, nor selected through an alias or label.
func (h *calendarResourceHandler) QueryCalendarSources(ctx context.Context, request *connect.Request[api.QueryCalendarSourcesRequest]) (*connect.Response[api.QueryCalendarSourcesResponse], error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, err
	}
	raw := taskRequest(request)
	home, err := apitypes.ParseHomeName(raw.GetParent())
	if err != nil {
		return nil, taskDenied()
	}
	actual, err := h.homes.HomeName(ctx)
	if err != nil {
		return nil, calendarRPCError(ctx, err)
	}
	if actual != home {
		return nil, taskDenied()
	}
	if !calendarKnown(raw) {
		return nil, taskInvalid()
	}
	limit, err := calendarPageSize(raw.PageSize, 256)
	if err != nil {
		return nil, err
	}
	binding, err := h.queryBinding(ctx, apiv2connect.CalendarServiceQueryCalendarSourcesProcedure, raw.Parent)
	if err != nil {
		return nil, err
	}
	capture := &api.QueryCalendarSourcesResponse{}
	entities, err := h.calendarEntities(ctx)
	if err != nil {
		return nil, calendarRPCError(ctx, err)
	}
	for _, entity := range entities {
		permit, err := h.permit(ctx, entity, auth.ScopeOperation_SCOPE_OPERATION_READ)
		if err != nil {
			if connect.CodeOf(err) == connect.CodePermissionDenied {
				continue
			}
			return nil, err
		}
		source, sourceErr := func() (*api.CalendarSource, error) {
			id, err := h.service.Bind(permit.Context(), entity)
			if err != nil {
				return nil, calendarRPCError(ctx, err)
			}
			name, err := calendarnames.Calendar(home, entity)
			if err != nil {
				return nil, calendarRPCError(ctx, err)
			}
			canonicalEntity, err := apitypes.EntityNameForIdentity(home, entity)
			if err != nil {
				return nil, calendarRPCError(ctx, err)
			}
			out := &api.CalendarSource{Calendar: name.String(), SourceEntity: canonicalEntity.String(), Origin: calendarWireOrigin(id.Origin()), MutationAccess: api.CalendarMutationAccess_CALENDAR_MUTATION_ACCESS_DENIED, Provenance: h.provenance}
			if out.Origin == api.CalendarOrigin_CALENDAR_ORIGIN_UNSPECIFIED {
				return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
			}
			mutableLifecycle := true
			if id.Origin() == calendars.Calculated {
				if workday.ValidateReader(h.workday) == nil {
					current, err := h.workday.CalendarID()
					if err == nil && current == id {
						out.Materialized = true
					}
				}
			} else {
				snapshot, err := h.service.GetCalendar(permit.Context(), id)
				if err != nil {
					return nil, calendarRPCError(ctx, err)
				}
				out.Materialized = snapshot.Revision().Presence() == calendars.Present
				if out.Materialized {
					out.DisplayName, _ = snapshot.Name()
				}
				mutableLifecycle = snapshot.Revision().Presence() != calendars.Removed
			}
			if id.Origin() == calendars.Local && mutableLifecycle {
				write, err := h.permit(ctx, entity, auth.ScopeOperation_SCOPE_OPERATION_WRITE)
				if err == nil {
					if write.Ready() {
						out.MutationAccess = api.CalendarMutationAccess_CALENDAR_MUTATION_ACCESS_ALLOWED
					}
					write.Release()
				} else if connect.CodeOf(err) != connect.CodePermissionDenied {
					return nil, err
				}
			}
			return out, nil
		}()
		ready := permit.Ready()
		permit.Release()
		if !ready {
			return nil, taskDenied()
		}
		if connect.CodeOf(sourceErr) == connect.CodeNotFound {
			continue
		}
		if sourceErr != nil {
			return nil, sourceErr
		}
		capture.CalendarSources = append(capture.CalendarSources, source)
	}
	slices.SortFunc(capture.CalendarSources, func(a, b *api.CalendarSource) int { return stringsCompare(a.Calendar, b.Calendar) })
	digest, err := mediaCaptureHash(capture)
	if err != nil {
		return nil, calendarRPCError(ctx, err)
	}
	offset, err := h.pageOffset(raw.PageToken, binding, digest, len(capture.CalendarSources))
	if err != nil {
		return nil, calendarRPCError(ctx, err)
	}
	end := min(offset+limit, len(capture.CalendarSources))
	token, err := h.pageToken(end, len(capture.CalendarSources), binding, digest)
	if err != nil {
		return nil, calendarRPCError(ctx, err)
	}
	final, err := h.queryBinding(ctx, apiv2connect.CalendarServiceQueryCalendarSourcesProcedure, raw.Parent)
	if err != nil || final != binding {
		return nil, taskDenied()
	}
	return connect.NewResponse(&api.QueryCalendarSourcesResponse{CalendarSources: capture.CalendarSources[offset:end], NextPageToken: token}), nil
}

func calendarWireOrigin(origin calendars.Origin) api.CalendarOrigin {
	switch origin {
	case calendars.Local:
		return api.CalendarOrigin_CALENDAR_ORIGIN_LOCAL
	case calendars.RemoteICS:
		return api.CalendarOrigin_CALENDAR_ORIGIN_REMOTE_ICS
	case calendars.Calculated:
		return api.CalendarOrigin_CALENDAR_ORIGIN_CALCULATED
	case calendars.OriginInvalid:
		return api.CalendarOrigin_CALENDAR_ORIGIN_UNSPECIFIED
	default:
		return api.CalendarOrigin_CALENDAR_ORIGIN_UNSPECIFIED
	}
}
