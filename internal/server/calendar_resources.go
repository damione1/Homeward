package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"slices"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/calendarnames"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/workday"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The same resource handler can be explicitly composed with synthetic sources
// for qualification. Creating a handler never starts a provider or listener.
type calendarResourceHandler struct {
	service    *calendars.Service
	authority  *authorization.Authority
	cursors    *calendars.CursorCodec
	homes      taskHomeProvider
	entities   []domain.EntityID
	provenance api.CalendarProvenance
	workday    workday.CalendarReader
	inventory  *calendarResourceInventory
}
type calendarResourcePermit struct {
	*authorization.Permit
	ctx context.Context
}

func (p *calendarResourcePermit) Context() context.Context {
	if p == nil {
		return nil
	}
	return p.ctx
}
func newCalendarResourceHandler(service *calendars.Service, authority *authorization.Authority, cursors *calendars.CursorCodec, homes taskHomeProvider, entities []domain.EntityID, provenance api.CalendarProvenance) (*calendarResourceHandler, error) {
	if service == nil || authority == nil || cursors == nil || nilReactiveInterface(homes) || len(entities) > 128 || provenance != api.CalendarProvenance_CALENDAR_PROVENANCE_OBSERVED && provenance != api.CalendarProvenance_CALENDAR_PROVENANCE_SYNTHETIC {
		return nil, ErrInvalidPublicPlan
	}
	seen := map[domain.EntityID]bool{}
	for _, entity := range entities {
		if _, err := entity.Value(); err != nil || seen[entity] {
			return nil, ErrInvalidPublicPlan
		}
		seen[entity] = true
	}
	return &calendarResourceHandler{service: service, authority: authority, cursors: cursors, homes: homes, entities: slices.Clone(entities), provenance: provenance}, nil
}
func (h *calendarResourceHandler) calendarEntities(ctx context.Context) ([]domain.EntityID, error) {
	if h.inventory != nil {
		return h.inventory.Entities(ctx)
	}
	return slices.Clone(h.entities), nil
}
func (h *calendarResourceHandler) principal(ctx context.Context) (authorization.Principal, error) {
	if h == nil || h.authority == nil || h.service == nil || nilReactiveInterface(h.homes) {
		return authorization.Principal{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE).Connect()
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return authorization.Principal{}, taskDenied()
	}
	expected, err := principal.Actor()
	actual, ok := ActorFromContext(ctx)
	if err != nil || !ok || expected != actual {
		return authorization.Principal{}, taskDenied()
	}
	return principal, nil
}
func (h *calendarResourceHandler) permit(ctx context.Context, entity domain.EntityID, operation auth.ScopeOperation) (*calendarResourcePermit, error) {
	principal, err := h.principal(ctx)
	if err != nil {
		return nil, err
	}
	object, err := authorization.CalendarObject(entity)
	if err != nil {
		return nil, taskDenied()
	}
	permission, err := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_CALENDARS, operation)
	if err != nil {
		return nil, taskDenied()
	}
	permit, err := h.authority.Permit(ctx, principal, permission, object)
	if err != nil {
		return nil, calendarRPCError(ctx, err)
	}
	entities, err := h.calendarEntities(permit.Context())
	if err != nil {
		permit.Release()
		return nil, calendarRPCError(ctx, err)
	}
	if !slices.Contains(entities, entity) {
		permit.Release()
		return nil, taskDenied()
	}
	evidence, err := permit.CalendarEvidence()
	if err != nil {
		permit.Release()
		return nil, taskDenied()
	}
	network, err := calendars.WithNetworkEvidence(permit.Context(), evidence)
	if err != nil {
		permit.Release()
		return nil, taskDenied()
	}
	return &calendarResourcePermit{permit, network}, nil
}
func withCalendarResource[T any](h *calendarResourceHandler, ctx context.Context, raw string, operation auth.ScopeOperation, fn func(context.Context, calendarnames.CalendarName, calendars.CalendarID) (T, error)) (T, error) {
	var zero T
	name, err := calendarnames.ParseCalendar(raw)
	if err != nil {
		return zero, taskDenied()
	}
	permit, err := h.permit(ctx, name.Entity(), operation)
	if err != nil {
		return zero, err
	}
	defer permit.Release()
	home, err := h.homes.HomeName(permit.Context())
	if err != nil {
		return zero, calendarRPCError(ctx, err)
	}
	if home != name.Home() {
		return zero, taskDenied()
	}
	id, err := h.service.Bind(permit.Context(), name.Entity())
	if err != nil {
		return zero, calendarRPCError(ctx, err)
	}
	initial, bindErr := h.queryBinding(ctx, "/home.api.v2.CalendarService/Admission", raw)
	if bindErr != nil {
		return zero, bindErr
	}
	value, err := fn(permit.Context(), name, id)
	current, bindErr := h.queryBinding(ctx, "/home.api.v2.CalendarService/Admission", raw)
	if bindErr != nil || current != initial {
		return zero, taskDenied()
	}
	if err != nil && apiFault(err).Reason() == api.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN {
		return zero, err
	}
	if ctx.Err() != nil {
		return zero, apiRPCError(ctx, ctx.Err())
	}
	if !permit.Ready() {
		return zero, taskDenied()
	}
	return value, err
}
func withCalendarEvent[T any](h *calendarResourceHandler, ctx context.Context, raw string, operation auth.ScopeOperation, fn func(context.Context, calendarnames.EventName, calendars.CalendarID, calendars.EventKey) (T, error)) (T, error) {
	var zero T
	name, err := calendarnames.ParseEvent(raw)
	if err != nil {
		return zero, taskDenied()
	}
	return withCalendarResource(h, ctx, name.Parent().String(), operation, func(ctx context.Context, _ calendarnames.CalendarName, id calendars.CalendarID) (T, error) {
		key, err := calendars.EventIdentity(id, name.UID())
		if err != nil {
			return zero, calendarRPCError(ctx, err)
		}
		return fn(ctx, name, id, key)
	})
}
func calendarExpected(name, etag string, id calendars.CalendarID, uid calendars.EventUID) (calendars.Precondition, error) {
	expected, err := calendars.ExpectedResourceETag(id, uid, etag, func(current calendars.Revision) (string, error) {
		number, valid := current.Number()
		if !valid {
			return "", calendarnames.ErrInvalid
		}
		return calendarnames.Etag(name, number)
	})
	if err != nil {
		return nil, taskInvalid()
	}
	return expected, nil
}

func calendarWireFreshness(state calendars.RemoteState, remote bool) api.CalendarFreshness {
	if !remote {
		return api.CalendarFreshness_CALENDAR_FRESHNESS_FRESH
	}
	freshness, known := state.Freshness()
	if !known {
		return api.CalendarFreshness_CALENDAR_FRESHNESS_UNAVAILABLE
	}
	switch freshness {
	case calendars.RemoteFresh:
		return api.CalendarFreshness_CALENDAR_FRESHNESS_FRESH
	case calendars.RemoteStale:
		return api.CalendarFreshness_CALENDAR_FRESHNESS_STALE
	case calendars.RemoteFreshnessInvalid:
		return api.CalendarFreshness_CALENDAR_FRESHNESS_UNAVAILABLE
	default:
		return api.CalendarFreshness_CALENDAR_FRESHNESS_UNAVAILABLE
	}
}
func (h *calendarResourceHandler) wireCalendar(home apitypes.HomeName, value calendars.CalendarSnapshot, access api.CalendarMutationAccess) (*api.Calendar, error) {
	if !value.Valid() || value.Revision().Presence() != calendars.Present {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND)
	}
	entity, ok := value.ID().Entity()
	if !ok {
		return nil, taskInvalid()
	}
	name, err := calendarnames.Calendar(home, entity)
	if err != nil {
		return nil, err
	}
	source, err := apitypes.EntityNameForIdentity(home, entity)
	if err != nil {
		return nil, err
	}
	number, ok := value.Revision().Number()
	if !ok {
		return nil, taskInvalid()
	}
	etag, err := calendarnames.Etag(name.String(), number)
	if err != nil {
		return nil, err
	}
	state, remote := value.RemoteState()
	out := &api.Calendar{Name: name.String(), SourceEntity: source.String(), Etag: etag, Provenance: h.provenance, Freshness: calendarWireFreshness(state, remote), MutationAccess: access}
	if display, present := value.Name(); present {
		out.DisplayName = display
	}
	if stamp, present := value.UpdatedAt(); present {
		out.UpdateTime = timestamppb.New(stamp)
	}
	if until, present := state.FreshUntil(); remote && present {
		out.FreshUntilTime = timestamppb.New(until)
	}
	switch value.ID().Origin() {
	case calendars.Local:
		out.Origin = api.CalendarOrigin_CALENDAR_ORIGIN_LOCAL
	case calendars.RemoteICS:
		out.Origin = api.CalendarOrigin_CALENDAR_ORIGIN_REMOTE_ICS
	case calendars.Calculated:
		out.Origin = api.CalendarOrigin_CALENDAR_ORIGIN_CALCULATED
	case calendars.OriginInvalid:
		return nil, taskInvalid()
	default:
		return nil, taskInvalid()
	}
	return out, nil
}
func (h *calendarResourceHandler) wireEvent(ctx context.Context, home apitypes.HomeName, value calendars.EventSnapshot) (*api.CalendarEvent, error) {
	event, present := value.Event()
	if !present {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND)
	}
	out, err := calendarWireEventDefinition(ctx, event)
	if err != nil {
		return nil, err
	}
	entity, _ := value.Key().Calendar().Entity()
	parent, err := calendarnames.Calendar(home, entity)
	if err != nil {
		return nil, err
	}
	name, err := calendarnames.Event(parent, value.Key().UID())
	if err != nil {
		return nil, err
	}
	out.Name = name.String()
	number, ok := value.Revision().Number()
	if !ok {
		return nil, taskInvalid()
	}
	out.Etag, err = calendarnames.Etag(out.Name, number)
	if err != nil {
		return nil, err
	}
	calendarNumber, ok := value.CalendarRevision().Number()
	if !ok {
		return nil, taskInvalid()
	}
	out.CalendarEtag, err = calendarnames.Etag(parent.String(), calendarNumber)
	if err != nil {
		return nil, err
	}
	if stamp, present := value.UpdatedAt(); present {
		out.UpdateTime = timestamppb.New(stamp)
	}
	return out, nil
}
func (h *calendarResourceHandler) mutationAccess(ctx context.Context, entity domain.EntityID, origin calendars.Origin) api.CalendarMutationAccess {
	if origin != calendars.Local {
		return api.CalendarMutationAccess_CALENDAR_MUTATION_ACCESS_DENIED
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return api.CalendarMutationAccess_CALENDAR_MUTATION_ACCESS_DENIED
	}
	current, err := h.authority.Current(principal)
	if err != nil {
		return api.CalendarMutationAccess_CALENDAR_MUTATION_ACCESS_DENIED
	}
	object, err := authorization.CalendarObject(entity)
	if err != nil {
		return api.CalendarMutationAccess_CALENDAR_MUTATION_ACCESS_DENIED
	}
	permission, err := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_CALENDARS, auth.ScopeOperation_SCOPE_OPERATION_WRITE)
	if err == nil && current.Authorize(permission, object) == nil {
		return api.CalendarMutationAccess_CALENDAR_MUTATION_ACCESS_ALLOWED
	}
	return api.CalendarMutationAccess_CALENDAR_MUTATION_ACCESS_DENIED
}
func (h *calendarResourceHandler) calendarResultEncoder(home apitypes.HomeName) func(calendars.CalendarSnapshot) (proto.Message, error) {
	return func(value calendars.CalendarSnapshot) (proto.Message, error) {
		return h.wireCalendar(home, value, api.CalendarMutationAccess_CALENDAR_MUTATION_ACCESS_ALLOWED)
	}
}
func (h *calendarResourceHandler) eventResultEncoder(ctx context.Context, home apitypes.HomeName) func(calendars.EventSnapshot) (proto.Message, error) {
	return func(value calendars.EventSnapshot) (proto.Message, error) { return h.wireEvent(ctx, home, value) }
}
func calendarEmptyEncoder[T any](T) (proto.Message, error) { return &emptypb.Empty{}, nil }
func calendarReplay(ctx context.Context, err error) (*connect.Response[api.Calendar], error) {
	value, replayed, failure := businessReplay(err, &api.Calendar{})
	if replayed {
		return connect.NewResponse(value), nil
	}
	return nil, calendarRPCError(ctx, failure)
}
func calendarEventReplay(ctx context.Context, err error) (*connect.Response[api.CalendarEvent], error) {
	value, replayed, failure := businessReplay(err, &api.CalendarEvent{})
	if replayed {
		return connect.NewResponse(value), nil
	}
	return nil, calendarRPCError(ctx, failure)
}
func calendarEmptyReplay(ctx context.Context, err error) (*connect.Response[emptypb.Empty], error) {
	value, replayed, failure := businessReplay(err, &emptypb.Empty{})
	if replayed {
		return connect.NewResponse(value), nil
	}
	return nil, calendarRPCError(ctx, failure)
}
func (h *calendarResourceHandler) GetCalendar(ctx context.Context, request *connect.Request[api.GetCalendarRequest]) (*connect.Response[api.Calendar], error) {
	raw := taskRequest(request)
	return withCalendarResource(h, ctx, raw.GetName(), auth.ScopeOperation_SCOPE_OPERATION_READ, func(ctx context.Context, name calendarnames.CalendarName, id calendars.CalendarID) (*connect.Response[api.Calendar], error) {
		if !taskKnown(raw) {
			return nil, taskInvalid()
		}
		out, err := h.readCalendar(ctx, name.Home(), id)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		return connect.NewResponse(out), nil
	})
}
func (h *calendarResourceHandler) CreateCalendar(ctx context.Context, request *connect.Request[api.CreateCalendarRequest]) (*connect.Response[api.Calendar], error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, err
	}
	raw := taskRequest(request)
	if raw == nil || raw.Calendar == nil {
		return nil, taskDenied()
	}
	home, err := h.homes.HomeName(ctx)
	if err != nil {
		return nil, calendarRPCError(ctx, err)
	}
	var selected domain.EntityID
	found := false
	entities, err := h.calendarEntities(ctx)
	if err != nil {
		return nil, calendarRPCError(ctx, err)
	}
	for _, entity := range entities {
		name, err := apitypes.EntityNameForIdentity(home, entity)
		if err == nil && name.String() == raw.Calendar.SourceEntity {
			selected = entity
			found = true
			break
		}
	}
	if !found {
		return nil, taskDenied()
	}
	target, err := calendarnames.Calendar(home, selected)
	if err != nil {
		return nil, taskDenied()
	}
	return withCalendarResource(h, ctx, target.String(), auth.ScopeOperation_SCOPE_OPERATION_WRITE, func(ctx context.Context, name calendarnames.CalendarName, id calendars.CalendarID) (*connect.Response[api.Calendar], error) {
		if id.Origin() != calendars.Local {
			return nil, calendarRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SOURCE_READ_ONLY))
		}
		if !taskKnown(raw) || !taskKnown(raw.Calendar) || raw.Parent != home.String() || raw.Calendar.Name != "" {
			return nil, taskInvalid()
		}
		expected, err := calendars.ExpectedRevision(id, calendars.EventUID{}, calendars.Absent, 0)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		normalized := businessNormalize(raw).(*api.CreateCalendarRequest)
		normalized.Calendar.Etag = ""
		ctx, err = businessMutationContext(ctx, apiv2connect.CalendarServiceCreateCalendarProcedure, raw.RequestId, normalized, h.calendarResultEncoder(home))
		if err != nil {
			return nil, err
		}
		value, err := h.service.CreateCalendar(ctx, id, expected, raw.Calendar.DisplayName)
		if err != nil {
			return calendarReplay(ctx, err)
		}
		out, err := h.wireCalendar(home, value, api.CalendarMutationAccess_CALENDAR_MUTATION_ACCESS_ALLOWED)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		return connect.NewResponse(out), nil
	})
}
func (h *calendarResourceHandler) UpdateCalendar(ctx context.Context, request *connect.Request[api.UpdateCalendarRequest]) (*connect.Response[api.Calendar], error) {
	raw := taskRequest(request)
	return withCalendarResource(h, ctx, raw.GetCalendar().GetName(), auth.ScopeOperation_SCOPE_OPERATION_WRITE, func(ctx context.Context, name calendarnames.CalendarName, id calendars.CalendarID) (*connect.Response[api.Calendar], error) {
		if id.Origin() != calendars.Local {
			return nil, calendarRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SOURCE_READ_ONLY))
		}
		if !taskKnown(raw) || !taskKnown(raw.Calendar) {
			return nil, taskInvalid()
		}
		expected, err := calendarExpected(name.String(), raw.Calendar.Etag, id, calendars.EventUID{})
		if err != nil {
			return nil, err
		}
		source, err := apitypes.EntityNameForIdentity(name.Home(), name.Entity())
		if err != nil {
			return nil, taskInvalid()
		}
		if raw.Calendar.SourceEntity != "" && raw.Calendar.SourceEntity != source.String() {
			return nil, taskViolation(raw, "calendar.source_entity", api.ValidationReason_VALIDATION_REASON_INCONSISTENT_FIELDS)
		}
		selected := raw.Calendar.DisplayName != ""
		if raw.UpdateMask != nil {
			if !taskKnown(raw.UpdateMask) {
				return nil, taskInvalid()
			}
			selected = false
			for _, path := range raw.UpdateMask.Paths {
				switch path {
				case "display_name", "*":
					selected = true
				case "source_entity":
					if raw.Calendar.SourceEntity != source.String() {
						return nil, taskInvalid()
					}
				case "name", "etag", "origin", "freshness", "fresh_until_time", "provenance", "mutation_access", "update_time":
				default:
					return nil, taskInvalid()
				}
			}
			if len(raw.UpdateMask.Paths) > 1 && slices.Contains(raw.UpdateMask.Paths, "*") {
				return nil, taskInvalid()
			}
		}
		normalized := businessNormalize(raw).(*api.UpdateCalendarRequest)
		normalized.Calendar.SourceEntity = ""
		normalized.UpdateMask = &fieldmaskpb.FieldMask{}
		if selected {
			normalized.UpdateMask.Paths = []string{"display_name"}
		} else {
			normalized.Calendar.DisplayName = ""
		}
		ctx, err = businessMutationContext(ctx, apiv2connect.CalendarServiceUpdateCalendarProcedure, raw.RequestId, normalized, h.calendarResultEncoder(name.Home()))
		if err != nil {
			return nil, err
		}
		var value calendars.CalendarSnapshot
		if selected {
			value, err = h.service.UpdateCalendar(ctx, id, expected, raw.Calendar.DisplayName)
		} else {
			value, err = h.service.CheckCalendarUpdate(ctx, id, expected)
		}
		if err != nil {
			return calendarReplay(ctx, err)
		}
		out, err := h.wireCalendar(name.Home(), value, api.CalendarMutationAccess_CALENDAR_MUTATION_ACCESS_ALLOWED)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		return connect.NewResponse(out), nil
	})
}
func (h *calendarResourceHandler) DeleteCalendar(ctx context.Context, request *connect.Request[api.DeleteCalendarRequest]) (*connect.Response[emptypb.Empty], error) {
	raw := taskRequest(request)
	return withCalendarResource(h, ctx, raw.GetName(), auth.ScopeOperation_SCOPE_OPERATION_WRITE, func(ctx context.Context, name calendarnames.CalendarName, id calendars.CalendarID) (*connect.Response[emptypb.Empty], error) {
		if id.Origin() != calendars.Local {
			return nil, calendarRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SOURCE_READ_ONLY))
		}
		if !taskKnown(raw) {
			return nil, taskInvalid()
		}
		expected, err := calendarExpected(name.String(), raw.Etag, id, calendars.EventUID{})
		if err != nil {
			return nil, err
		}
		ctx, err = businessMutationContext(ctx, apiv2connect.CalendarServiceDeleteCalendarProcedure, raw.RequestId, businessNormalize(raw), calendarEmptyEncoder[calendars.CalendarSnapshot])
		if err != nil {
			return nil, err
		}
		if _, err = h.service.DeleteCalendarResource(ctx, id, expected, raw.Force); err != nil {
			return calendarEmptyReplay(ctx, err)
		}
		return connect.NewResponse(&emptypb.Empty{}), nil
	})
}
func (h *calendarResourceHandler) GetCalendarEvent(ctx context.Context, request *connect.Request[api.GetCalendarEventRequest]) (*connect.Response[api.CalendarEvent], error) {
	raw := taskRequest(request)
	return withCalendarEvent(h, ctx, raw.GetName(), auth.ScopeOperation_SCOPE_OPERATION_READ, func(ctx context.Context, name calendarnames.EventName, id calendars.CalendarID, key calendars.EventKey) (*connect.Response[api.CalendarEvent], error) {
		if !taskKnown(raw) {
			return nil, taskInvalid()
		}
		value, err := h.service.GetEvent(ctx, key)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		out, err := h.wireEvent(ctx, name.Parent().Home(), value)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		return connect.NewResponse(out), nil
	})
}
func (h *calendarResourceHandler) CreateCalendarEvent(ctx context.Context, request *connect.Request[api.CreateCalendarEventRequest]) (*connect.Response[api.CalendarEvent], error) {
	raw := taskRequest(request)
	return withCalendarResource(h, ctx, raw.GetParent(), auth.ScopeOperation_SCOPE_OPERATION_WRITE, func(ctx context.Context, name calendarnames.CalendarName, id calendars.CalendarID) (*connect.Response[api.CalendarEvent], error) {
		if id.Origin() != calendars.Local {
			return nil, calendarRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SOURCE_READ_ONLY))
		}
		if !taskKnown(raw) || !taskKnown(raw.CalendarEvent) || raw.CalendarEvent.Name != "" {
			return nil, taskInvalid()
		}
		event, err := calendarEvent(ctx, raw.CalendarEvent)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		var bytes [16]byte
		if _, err = rand.Read(bytes[:]); err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		uid, err := calendars.ParseUID("event-" + hex.EncodeToString(bytes[:]))
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		key, err := calendars.EventIdentity(id, uid)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		expected, err := calendars.ExpectedRevision(id, uid, calendars.Absent, 0)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		normalized := businessNormalize(raw).(*api.CreateCalendarEventRequest)
		normalized.CalendarEvent, err = calendarWireEventDefinition(ctx, event)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		ctx, err = businessMutationContext(ctx, apiv2connect.CalendarServiceCreateCalendarEventProcedure, raw.RequestId, normalized, h.eventResultEncoder(ctx, name.Home()))
		if err != nil {
			return nil, err
		}
		value, err := h.service.CreateEvent(ctx, key, expected, event)
		if err != nil {
			return calendarEventReplay(ctx, err)
		}
		out, err := h.wireEvent(ctx, name.Home(), value)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		return connect.NewResponse(out), nil
	})
}
func (h *calendarResourceHandler) DeleteCalendarEvent(ctx context.Context, request *connect.Request[api.DeleteCalendarEventRequest]) (*connect.Response[emptypb.Empty], error) {
	raw := taskRequest(request)
	return withCalendarEvent(h, ctx, raw.GetName(), auth.ScopeOperation_SCOPE_OPERATION_WRITE, func(ctx context.Context, name calendarnames.EventName, id calendars.CalendarID, key calendars.EventKey) (*connect.Response[emptypb.Empty], error) {
		if id.Origin() != calendars.Local {
			return nil, calendarRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SOURCE_READ_ONLY))
		}
		if !taskKnown(raw) {
			return nil, taskInvalid()
		}
		expected, err := calendarExpected(name.String(), raw.Etag, id, key.UID())
		if err != nil {
			return nil, err
		}
		ctx, err = businessMutationContext(ctx, apiv2connect.CalendarServiceDeleteCalendarEventProcedure, raw.RequestId, businessNormalize(raw), calendarEmptyEncoder[calendars.EventSnapshot])
		if err != nil {
			return nil, err
		}
		if _, err = h.service.DeleteEvent(ctx, key, expected); err != nil {
			return calendarEmptyReplay(ctx, err)
		}
		return connect.NewResponse(&emptypb.Empty{}), nil
	})
}

func (h *calendarResourceHandler) UpdateCalendarEvent(ctx context.Context, request *connect.Request[api.UpdateCalendarEventRequest]) (*connect.Response[api.CalendarEvent], error) {
	raw := taskRequest(request)
	return withCalendarEvent(h, ctx, raw.GetCalendarEvent().GetName(), auth.ScopeOperation_SCOPE_OPERATION_WRITE, func(ctx context.Context, name calendarnames.EventName, id calendars.CalendarID, key calendars.EventKey) (*connect.Response[api.CalendarEvent], error) {
		if id.Origin() != calendars.Local {
			return nil, calendarRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SOURCE_READ_ONLY))
		}
		if !taskKnown(raw) || !taskKnown(raw.CalendarEvent) {
			return nil, taskInvalid()
		}
		expected, err := calendarExpected(name.String(), raw.CalendarEvent.Etag, id, key.UID())
		if err != nil {
			return nil, err
		}
		fields, err := calendarEventMask(raw.CalendarEvent, raw.UpdateMask)
		if err != nil {
			return nil, taskViolation(raw, "update_mask", api.ValidationReason_VALIDATION_REASON_INVALID_MASK)
		}
		input, err := calendarPatchInput(ctx, raw.CalendarEvent, fields)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		patch, err := calendars.NewEventPatch(input, fields)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		normalized := calendarNormalizedPatch(raw, fields)
		ctx, err = businessMutationContext(ctx, apiv2connect.CalendarServiceUpdateCalendarEventProcedure, raw.RequestId, normalized, h.eventResultEncoder(ctx, name.Parent().Home()))
		if err != nil {
			return nil, err
		}
		value, err := h.service.PatchEvent(ctx, key, expected, patch)
		if err != nil {
			return calendarEventReplay(ctx, err)
		}
		out, err := h.wireEvent(ctx, name.Parent().Home(), value)
		if err != nil {
			return nil, calendarRPCError(ctx, err)
		}
		return connect.NewResponse(out), nil
	})
}
