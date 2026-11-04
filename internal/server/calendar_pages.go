package server

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/calendarnames"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/workday"
	"google.golang.org/protobuf/proto"
)

func calendarPageSize(raw int32, maximum int) (int, error) {
	if raw < 0 {
		return 0, taskInvalid()
	}
	if raw == 0 {
		return min(100, maximum), nil
	}
	return min(int(raw), maximum), nil
}
func (h *calendarResourceHandler) queryBinding(ctx context.Context, method, parent string) ([32]byte, error) {
	principal, err := h.principal(ctx)
	if err != nil {
		return [32]byte{}, err
	}
	current, err := h.authority.Current(principal)
	if err != nil {
		return [32]byte{}, taskDenied()
	}
	_, binding, err := current.Binding()
	if err != nil {
		return [32]byte{}, taskDenied()
	}
	hash := sha256.New()
	hash.Write([]byte("homeward-calendar-resources/2"))
	hash.Write(binding[:])
	var size [8]byte
	for _, value := range []string{method, parent} {
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		hash.Write(size[:])
		hash.Write([]byte(value))
	}
	var out [32]byte
	copy(out[:], hash.Sum(nil))
	return out, nil
}

type calendarCaptureCursor struct {
	Capture [32]byte
	Offset  int
}

func (h *calendarResourceHandler) pageOffset(token string, binding, capture [32]byte, total int) (int, error) {
	if token == "" {
		return 0, nil
	}
	raw, err := h.cursors.Open(token, binding)
	if err != nil {
		return 0, err
	}
	var cursor calendarCaptureCursor
	if json.Unmarshal(raw, &cursor) != nil || cursor.Capture != capture || cursor.Offset < 1 || cursor.Offset >= total {
		return 0, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED)
	}
	return cursor.Offset, nil
}
func (h *calendarResourceHandler) pageToken(offset, total int, binding, capture [32]byte) (string, error) {
	if offset >= total {
		return "", nil
	}
	raw, err := json.Marshal(calendarCaptureCursor{capture, offset})
	if err != nil {
		return "", err
	}
	return h.cursors.Seal(raw, binding)
}
func (h *calendarResourceHandler) readCalendar(ctx context.Context, home apitypes.HomeName, id calendars.CalendarID) (*api.Calendar, error) {
	entity, ok := id.Entity()
	if !ok {
		return nil, taskInvalid()
	}
	if id.Origin() != calendars.Calculated {
		value, err := h.service.GetCalendar(ctx, id)
		if err != nil {
			return nil, err
		}
		return h.wireCalendar(home, value, h.mutationAccess(ctx, entity, id.Origin()))
	}
	if workday.ValidateReader(h.workday) != nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	bound, err := h.workday.CalendarID()
	if err != nil || bound != id {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	name, err := calendarnames.Calendar(home, entity)
	if err != nil {
		return nil, err
	}
	etag, err := calendarnames.Etag(name.String(), 1)
	if err != nil {
		return nil, err
	}
	source, err := apitypes.EntityNameForIdentity(home, entity)
	if err != nil {
		return nil, err
	}
	return &api.Calendar{Name: name.String(), DisplayName: "Workday", SourceEntity: source.String(), Origin: api.CalendarOrigin_CALENDAR_ORIGIN_CALCULATED, Freshness: api.CalendarFreshness_CALENDAR_FRESHNESS_FRESH, Provenance: h.provenance, MutationAccess: api.CalendarMutationAccess_CALENDAR_MUTATION_ACCESS_DENIED, Etag: etag}, nil
}
func (h *calendarResourceHandler) ListCalendars(ctx context.Context, request *connect.Request[api.ListCalendarsRequest]) (*connect.Response[api.ListCalendarsResponse], error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, err
	}
	raw := taskRequest(request)
	if !taskKnown(raw) {
		return nil, taskInvalid()
	}
	home, err := h.homes.HomeName(ctx)
	if err != nil {
		return nil, calendarRPCError(ctx, err)
	}
	parsed, err := apitypes.ParseHomeName(raw.Parent)
	if err != nil {
		return nil, taskInvalid()
	}
	if parsed != home {
		return nil, taskDenied()
	}
	binding, err := h.queryBinding(ctx, apiv2connect.CalendarServiceListCalendarsProcedure, home.String())
	if err != nil {
		return nil, err
	}
	limit, err := calendarPageSize(raw.PageSize, 128)
	if err != nil {
		return nil, err
	}
	var values []*api.Calendar
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
		id, err := h.service.Bind(permit.Context(), entity)
		var value *api.Calendar
		if err == nil {
			value, err = h.readCalendar(permit.Context(), home, id)
		}
		ready := permit.Ready()
		permit.Release()
		if !ready {
			return nil, taskDenied()
		}
		if err != nil {
			var fault apierrors.Fault
			if errors.As(err, &fault) && fault.Reason() == api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND {
				continue
			}
			return nil, calendarRPCError(ctx, err)
		}
		values = append(values, value)
	}
	slices.SortFunc(values, func(a, b *api.Calendar) int { return strings.Compare(a.Name, b.Name) })
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(&api.ListCalendarsResponse{Calendars: values})
	if err != nil {
		return nil, calendarRPCError(ctx, err)
	}
	capture := sha256.Sum256(encoded)
	offset, err := h.pageOffset(raw.PageToken, binding, capture, len(values))
	if err != nil {
		return nil, calendarRPCError(ctx, err)
	}
	end := min(offset+limit, len(values))
	out := &api.ListCalendarsResponse{Calendars: values[offset:end]}
	out.NextPageToken, err = h.pageToken(end, len(values), binding, capture)
	if err != nil {
		return nil, calendarRPCError(ctx, err)
	}
	after, err := h.queryBinding(ctx, apiv2connect.CalendarServiceListCalendarsProcedure, home.String())
	if err != nil || after != binding {
		return nil, taskDenied()
	}
	return connect.NewResponse(out), nil
}
func (h *calendarResourceHandler) ListCalendarEvents(ctx context.Context, request *connect.Request[api.ListCalendarEventsRequest]) (*connect.Response[api.ListCalendarEventsResponse], error) {
	raw := taskRequest(request)
	return withCalendarResource(h, ctx, raw.GetParent(), auth.ScopeOperation_SCOPE_OPERATION_READ, func(ctx context.Context, name calendarnames.CalendarName, id calendars.CalendarID) (*connect.Response[api.ListCalendarEventsResponse], error) {
		if !taskKnown(raw) {
			return nil, taskInvalid()
		}
		limit, err := calendarPageSize(raw.PageSize, calendars.MaxEvents)
		if err != nil {
			return nil, err
		}
		binding, err := h.queryBinding(ctx, apiv2connect.CalendarServiceListCalendarEventsProcedure, name.String())
		if err != nil {
			return nil, err
		}
		if id.Origin() == calendars.Calculated {
			if raw.PageToken != "" {
				return nil, calendarRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED))
			}
			return connect.NewResponse(&api.ListCalendarEventsResponse{}), nil
		}
		definitions, err := h.service.ListEvents(ctx, id)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		slices.SortFunc(definitions, func(a, b calendars.EventSnapshot) int {
			aa, _ := a.Key().UID().Value()
			bb, _ := b.Key().UID().Value()
			return strings.Compare(aa, bb)
		})
		hash := sha256.New()
		for _, value := range definitions {
			uid, _ := value.Key().UID().Value()
			var uidSize [8]byte
			binary.BigEndian.PutUint64(uidSize[:], uint64(len(uid)))
			hash.Write(uidSize[:])
			hash.Write([]byte(uid))
			number, ok := value.CalendarRevision().Number()
			if !ok {
				return nil, taskInvalid()
			}
			var bytes [8]byte
			binary.BigEndian.PutUint64(bytes[:], number)
			hash.Write(bytes[:])
		}
		var capture [32]byte
		copy(capture[:], hash.Sum(nil))
		offset, err := h.pageOffset(raw.PageToken, binding, capture, len(definitions))
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		out := &api.ListCalendarEventsResponse{}
		end := offset
		for end < len(definitions) && end < offset+limit {
			value, err := h.wireEvent(ctx, name.Home(), definitions[end])
			if err != nil {
				return nil, calendarRPCError(ctx, err)
			}
			out.CalendarEvents = append(out.CalendarEvents, value)
			if proto.Size(out) > 1<<20 {
				out.CalendarEvents = out.CalendarEvents[:len(out.CalendarEvents)-1]
				if end == offset {
					return nil, calendarRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED))
				}
				break
			}
			end++
		}
		out.NextPageToken, err = h.pageToken(end, len(definitions), binding, capture)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		return connect.NewResponse(out), nil
	})
}
