package server

// Explicit synthetic qualification only. This adapter is not compiled into the
// production server and never starts an integration or a device driver.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"connectrpc.com/connect"
	av "github.com/damione1/Homeward/gen/home/auth/v1"
	cv "github.com/damione1/Homeward/gen/home/calendars/v1"
	"github.com/damione1/Homeward/gen/home/calendars/v1/calendarsv1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/workday"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"slices"
	"sync/atomic"
)

type nhf015CalendarHandler struct {
	service         *calendars.Service
	entities        []domain.EntityID
	dropNextReply   atomic.Bool
	authority       *authorization.Authority
	cursors         *calendars.CursorCodec
	workday         workday.CalendarReader
	afterWatchReady func()
}

var _ calendarsv1connect.CalendarServiceHandler = (*nhf015CalendarHandler)(nil)

// The network context includes an opaque committed-rights fence; trusted
// automation composition never obtains this wrapper or a user identity.
type nhf015CalendarPermit struct {
	*authorization.Permit
	ctx context.Context
}

func (p *nhf015CalendarPermit) Context() context.Context {
	if p == nil {
		return nil
	}
	return p.ctx
}
func nhf015CalendarNetworkPermit(permit *authorization.Permit) (*nhf015CalendarPermit, error) {
	evidence, e := permit.CalendarEvidence()
	if e != nil {
		permit.Release()
		return nil, e
	}
	ctx, e := calendars.WithNetworkEvidence(permit.Context(), evidence)
	if e != nil {
		permit.Release()
		return nil, e
	}
	return &nhf015CalendarPermit{permit, ctx}, nil
}

func (h *nhf015CalendarHandler) permit(ctx context.Context, raw *cv.CalendarId, op av.ScopeOperation) (domain.EntityID, *nhf015CalendarPermit, error) {
	if h == nil || h.service == nil || len(h.entities) == 0 || h.authority == nil || raw == nil || !nhf015CalendarKnown(raw) {
		return domain.EntityID{}, nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
	}
	principal, e := authorization.PrincipalFromContext(ctx)
	if e != nil {
		return domain.EntityID{}, nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_DENIED)
	}
	actor, ok := ActorFromContext(ctx)
	admitted, e := principal.Actor()
	if !ok || e != nil || actor != admitted {
		return domain.EntityID{}, nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_DENIED)
	}
	entity, e := domain.ParseEntityID(raw.EntityId)
	if e != nil {
		return domain.EntityID{}, nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
	}
	if !slices.Contains(h.entities, entity) {
		return domain.EntityID{}, nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_DENIED)
	}
	object, e := authorization.CalendarObject(entity)
	if e != nil {
		return domain.EntityID{}, nil, nhf015CalendarRPCError(e)
	}
	permission, e := authorization.NewPermission(av.ScopeFamily_SCOPE_FAMILY_CALENDARS, op)
	if e != nil {
		return domain.EntityID{}, nil, nhf015CalendarRPCError(e)
	}
	permit, e := h.authority.Permit(ctx, principal, permission, object)
	if e != nil {
		return domain.EntityID{}, nil, nhf015CalendarRPCError(e)
	}
	network, e := nhf015CalendarNetworkPermit(permit)
	if e != nil {
		return domain.EntityID{}, nil, nhf015CalendarRPCError(e)
	}
	return entity, network, nil
}
func (h *nhf015CalendarHandler) bind(ctx context.Context, raw *cv.CalendarId, op av.ScopeOperation) (calendars.CalendarID, *nhf015CalendarPermit, error) {
	entity, permit, e := h.permit(ctx, raw, op)
	if e != nil {
		return calendars.CalendarID{}, nil, e
	}
	id, e := h.service.Bind(permit.Context(), entity)
	if e != nil {
		permit.Release()
		return calendars.CalendarID{}, nil, nhf015CalendarRPCError(e)
	}
	return id, permit, nil
}
func (h *nhf015CalendarHandler) binding(ctx context.Context) ([32]byte, error) {
	p, e := authorization.PrincipalFromContext(ctx)
	if e != nil {
		return [32]byte{}, e
	}
	current, e := h.authority.Current(p)
	if e != nil {
		return [32]byte{}, e
	}
	_, binding, e := current.Binding()
	return binding, e
}
func nhf015CalendarFinish(permit *nhf015CalendarPermit, err error, mutation bool) error {
	if err != nil {
		return nhf015CalendarRPCError(err)
	}
	if !permit.Ready() {
		if mutation {
			return nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_COMMITTED_RESPONSE_WITHHELD)
		}
		return nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_DENIED)
	}
	return nil
}
func (h *nhf015CalendarHandler) calculated() (workday.CalendarReader, uint64, error) {
	reader := h.workday
	revision := uint64(1)
	if workday.ValidateReader(reader) != nil {
		return nil, 0, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_UNAVAILABLE)
	}
	return reader, revision, nil
}
func (h *nhf015CalendarHandler) calendar(ctx context.Context, id calendars.CalendarID) (*cv.Calendar, error) {
	if id.Origin() != calendars.Calculated {
		v, e := h.service.GetCalendar(ctx, id)
		if e != nil {
			return nil, e
		}
		return nhf015WireCalendar(v), nil
	}
	reader, revision, e := h.calculated()
	if e != nil {
		return nil, e
	}
	bound, e := reader.CalendarID()
	if e != nil || bound != id {
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_UNAVAILABLE)
	}
	r, e := calendars.ExpectedRevision(id, calendars.EventUID{}, calendars.Present, revision)
	if e != nil {
		return nil, e
	}
	entity, _ := id.Entity()
	text, _ := entity.Value()
	name := "Workday"
	return &cv.Calendar{Id: &cv.CalendarId{EntityId: text}, Origin: cv.Origin_ORIGIN_CALCULATED, Name: &name, Revision: nhf015CalendarRevision(r, id, ""), Quality: cv.Quality_QUALITY_FRESH}, nil
}
func (h *nhf015CalendarHandler) ListCalendars(ctx context.Context, req *connect.Request[cv.ListCalendarsRequest]) (*connect.Response[cv.ListCalendarsResponse], error) {
	if req == nil || !nhf015CalendarKnown(req.Msg) {
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
	}
	principal, e := authorization.PrincipalFromContext(ctx)
	if e != nil {
		return nil, nhf015CalendarRPCError(e)
	}
	actor, ok := ActorFromContext(ctx)
	expectedActor, e := principal.Actor()
	if !ok || e != nil || actor != expectedActor {
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_DENIED)
	}
	initialBinding, e := h.binding(ctx)
	if e != nil {
		return nil, nhf015CalendarRPCError(e)
	}
	entities := slices.Clone(h.entities)
	result := &cv.ListCalendarsResponse{}
	for _, entity := range entities {
		object, e := authorization.CalendarObject(entity)
		if e != nil {
			return nil, nhf015CalendarRPCError(e)
		}
		permission, _ := authorization.NewPermission(av.ScopeFamily_SCOPE_FAMILY_CALENDARS, av.ScopeOperation_SCOPE_OPERATION_READ)
		permit, e := h.authority.Permit(ctx, principal, permission, object)
		if e != nil {
			if e == authorization.ErrDenied {
				continue
			}
			return nil, nhf015CalendarRPCError(e)
		}
		network, e := nhf015CalendarNetworkPermit(permit)
		if e != nil {
			return nil, nhf015CalendarRPCError(e)
		}
		id, e := h.service.Bind(network.Context(), entity)
		if e != nil {
			network.Release()
			return nil, nhf015CalendarRPCError(e)
		}
		item, e := h.calendar(network.Context(), id)
		if e == nil {
			h.decorate(ctx, item)
		}
		ready := network.Ready()
		network.Release()
		if e != nil {
			return nil, nhf015CalendarRPCError(e)
		}
		if !ready {
			return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_DENIED)
		}
		result.Calendars = append(result.Calendars, item)
		if len(result.Calendars) > 128 {
			return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_CAPACITY)
		}
	}
	// The final binding check withholds a whole list assembled across a policy change.
	current, e := h.authority.Current(principal)
	if e != nil {
		return nil, nhf015CalendarRPCError(e)
	}
	_, finalBinding, e := current.Binding()
	if e != nil || finalBinding != initialBinding {
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_DENIED)
	}
	for _, item := range result.Calendars {
		entity, _ := domain.ParseEntityID(item.Id.EntityId)
		object, _ := authorization.CalendarObject(entity)
		permission, _ := authorization.NewPermission(av.ScopeFamily_SCOPE_FAMILY_CALENDARS, av.ScopeOperation_SCOPE_OPERATION_READ)
		if current.Authorize(permission, object) != nil {
			return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_DENIED)
		}
	}
	return connect.NewResponse(result), nil
}
func (h *nhf015CalendarHandler) GetCalendar(ctx context.Context, req *connect.Request[cv.GetCalendarRequest]) (*connect.Response[cv.GetCalendarResponse], error) {
	if req == nil || !nhf015CalendarKnown(req.Msg) {
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
	}
	id, permit, e := h.bind(ctx, req.Msg.Id, av.ScopeOperation_SCOPE_OPERATION_READ)
	if e != nil {
		return nil, e
	}
	defer permit.Release()
	v, e := h.calendar(permit.Context(), id)
	if e == nil {
		h.decorate(ctx, v)
	}
	if e = nhf015CalendarFinish(permit, e, false); e != nil {
		return nil, e
	}
	return connect.NewResponse(&cv.GetCalendarResponse{Calendar: v}), nil
}
func (h *nhf015CalendarHandler) MutateCalendar(ctx context.Context, req *connect.Request[cv.MutateCalendarRequest]) (*connect.Response[cv.MutateCalendarResponse], error) {
	if req == nil || !nhf015CalendarKnown(req.Msg) {
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
	}
	id, permit, e := h.bind(ctx, req.Msg.Id, av.ScopeOperation_SCOPE_OPERATION_WRITE)
	if e != nil {
		return nil, e
	}
	defer permit.Release()
	if id.Origin() != calendars.Local {
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_READ_ONLY)
	}
	expected, e := nhf015CalendarExpected(req.Msg.ExpectedRevision, id, calendars.EventUID{})
	if e != nil {
		return nil, nhf015CalendarRPCError(e)
	}
	var v calendars.CalendarSnapshot
	switch req.Msg.Mutation {
	case cv.Mutation_MUTATION_CREATE:
		v, e = h.service.CreateCalendar(permit.Context(), id, expected, req.Msg.Name)
	case cv.Mutation_MUTATION_UPDATE:
		v, e = h.service.UpdateCalendar(permit.Context(), id, expected, req.Msg.Name)
	case cv.Mutation_MUTATION_DELETE:
		if req.Msg.Name != "" {
			return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
		}
		v, e = h.service.DeleteCalendar(permit.Context(), id, expected)
	default:
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
	}
	if e == nil && h.dropNextReply.Swap(false) {
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_COMMIT_UNKNOWN)
	}
	if e = nhf015CalendarFinish(permit, e, true); e != nil {
		return nil, e
	}
	revision := nhf015CalendarRevision(v.Revision(), id, "")
	return connect.NewResponse(&cv.MutateCalendarResponse{Receipt: &cv.MutationReceipt{Revision: revision, CalendarRevision: revision}}), nil
}
func nhf015CalendarKey(raw *cv.EventKey, id calendars.CalendarID) (calendars.EventKey, error) {
	if raw == nil {
		return calendars.EventKey{}, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
	}
	uid, e := calendars.ParseUID(raw.Uid)
	if e != nil {
		return calendars.EventKey{}, e
	}
	return calendars.EventIdentity(id, uid)
}
func (h *nhf015CalendarHandler) GetEvent(ctx context.Context, req *connect.Request[cv.GetEventRequest]) (*connect.Response[cv.GetEventResponse], error) {
	if req == nil || !nhf015CalendarKnown(req.Msg) || req.Msg.Key == nil {
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
	}
	id, permit, e := h.bind(ctx, req.Msg.Key.Calendar, av.ScopeOperation_SCOPE_OPERATION_READ)
	if e != nil {
		return nil, e
	}
	defer permit.Release()
	if id.Origin() == calendars.Calculated {
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_NOT_FOUND)
	}
	key, e := nhf015CalendarKey(req.Msg.Key, id)
	if e != nil {
		return nil, nhf015CalendarRPCError(e)
	}
	v, e := h.service.GetEvent(permit.Context(), key)
	if e = nhf015CalendarFinish(permit, e, false); e != nil {
		return nil, e
	}
	event, present := v.Event()
	result := &cv.EventSnapshot{Key: proto.Clone(req.Msg.Key).(*cv.EventKey), Revision: nhf015CalendarRevision(v.Revision(), id, req.Msg.Key.Uid), CalendarRevision: nhf015CalendarRevision(v.CalendarRevision(), id, "")}
	if present {
		result.Value = nhf015WireCalendarEvent(event)
	}
	return connect.NewResponse(&cv.GetEventResponse{Event: result}), nil
}
func (h *nhf015CalendarHandler) MutateEvent(ctx context.Context, req *connect.Request[cv.MutateEventRequest]) (*connect.Response[cv.MutateEventResponse], error) {
	if req == nil || !nhf015CalendarKnown(req.Msg) || req.Msg.Key == nil {
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
	}
	id, permit, e := h.bind(ctx, req.Msg.Key.Calendar, av.ScopeOperation_SCOPE_OPERATION_WRITE)
	if e != nil {
		return nil, e
	}
	defer permit.Release()
	if id.Origin() != calendars.Local {
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_READ_ONLY)
	}
	key, e := nhf015CalendarKey(req.Msg.Key, id)
	if e != nil {
		return nil, nhf015CalendarRPCError(e)
	}
	expected, e := nhf015CalendarExpected(req.Msg.ExpectedRevision, id, key.UID())
	if e != nil {
		return nil, nhf015CalendarRPCError(e)
	}
	var event calendars.Event
	if req.Msg.Mutation != cv.Mutation_MUTATION_DELETE {
		event, e = nhf015CalendarEvent(permit.Context(), req.Msg.Value)
		if e != nil {
			return nil, nhf015CalendarRPCError(e)
		}
	} else if req.Msg.Value != nil {
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
	}
	var v calendars.EventSnapshot
	switch req.Msg.Mutation {
	case cv.Mutation_MUTATION_CREATE:
		v, e = h.service.CreateEvent(permit.Context(), key, expected, event)
	case cv.Mutation_MUTATION_UPDATE:
		v, e = h.service.UpdateEvent(permit.Context(), key, expected, event)
	case cv.Mutation_MUTATION_DELETE:
		v, e = h.service.DeleteEvent(permit.Context(), key, expected)
	default:
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
	}
	if e == nil && h.dropNextReply.Swap(false) {
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_COMMIT_UNKNOWN)
	}
	if e = nhf015CalendarFinish(permit, e, true); e != nil {
		return nil, e
	}
	return connect.NewResponse(&cv.MutateEventResponse{Receipt: &cv.MutationReceipt{Revision: nhf015CalendarRevision(v.Revision(), id, req.Msg.Key.Uid), CalendarRevision: nhf015CalendarRevision(v.CalendarRevision(), id, "")}}), nil
}
func nhf015CalendarWindow(ctx context.Context, raw *cv.Window) (calendars.Window, calendars.Zone, error) {
	if raw == nil || !nhf015CalendarKnown(raw) {
		return calendars.Window{}, calendars.Zone{}, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
	}
	zone, e := calendars.ParseZone(raw.Zone)
	if e != nil {
		return calendars.Window{}, zone, e
	}
	switch v := raw.Bounds.(type) {
	case *cv.Window_Dates:
		if v == nil || v.Dates == nil {
			break
		}
		a, e := calendars.ParseDate(v.Dates.Start)
		if e != nil {
			return calendars.Window{}, zone, e
		}
		b, e := calendars.ParseDate(v.Dates.EndExclusive)
		if e != nil {
			return calendars.Window{}, zone, e
		}
		w, e := calendars.AgendaWindow(ctx, a, b, zone)
		return w, zone, e
	case *cv.Window_Times:
		if v == nil || v.Times == nil || v.Times.Zone != raw.Zone {
			break
		}
		a, e := calendars.ParseInstant(v.Times.StartInstant)
		if e != nil {
			return calendars.Window{}, zone, e
		}
		b, e := calendars.ParseInstant(v.Times.EndInstant)
		if e != nil {
			return calendars.Window{}, zone, e
		}
		w, e := calendars.TimeWindow(a, b, zone)
		return w, zone, e
	}
	return calendars.Window{}, zone, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
}
func (h *nhf015CalendarHandler) QueryOccurrences(ctx context.Context, req *connect.Request[cv.QueryOccurrencesRequest]) (*connect.Response[cv.QueryOccurrencesResponse], error) {
	if req == nil || !nhf015CalendarKnown(req.Msg) || req.Msg.PageSize < 1 || req.Msg.PageSize > calendars.MaxPageSize {
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
	}
	window, zone, e := nhf015CalendarWindow(ctx, req.Msg.Window)
	if e != nil {
		return nil, nhf015CalendarRPCError(e)
	}
	id, permit, e := h.bind(ctx, req.Msg.Id, av.ScopeOperation_SCOPE_OPERATION_READ)
	if e != nil {
		return nil, e
	}
	defer permit.Release()
	binding, e := h.binding(ctx)
	if e != nil {
		return nil, nhf015CalendarRPCError(e)
	}
	windowBytes, e := proto.MarshalOptions{Deterministic: true}.Marshal(req.Msg.Window)
	if e != nil {
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
	}
	requestHash := sha256.New()
	requestHash.Write([]byte("homeward.calendar-network-window/1"))
	requestHash.Write(binding[:])
	requestHash.Write(windowBytes)
	copy(binding[:], requestHash.Sum(nil))
	if id.Origin() == calendars.Calculated {
		v, e := h.queryCalculated(permit.Context(), id, window, zone, req.Msg, binding)
		if e = nhf015CalendarFinish(permit, e, false); e != nil {
			return nil, e
		}
		return connect.NewResponse(v), nil
	}
	cursor, e := h.cursors.Decode(req.Msg.PageToken, id, window, binding)
	if e != nil {
		return nil, nhf015CalendarRPCError(e)
	}
	page, e := h.service.Query(permit.Context(), id, window, int(req.Msg.PageSize), cursor)
	if e != nil {
		return nil, nhf015CalendarRPCError(e)
	}
	state, remote := page.RemoteState()
	result := &cv.QueryOccurrencesResponse{Provenance: cv.DataProvenance_DATA_PROVENANCE_SYNTHETIC, Revision: nhf015CalendarRevision(page.Revision(), id, ""), Quality: nhf015CalendarQuality(state, remote)}
	for _, item := range page.Items() {
		uid, _ := item.ID().Event().UID().Value()
		result.Occurrences = append(result.Occurrences, &cv.Occurrence{Key: &cv.EventKey{Calendar: req.Msg.Id, Uid: uid}, RecurrenceId: item.ID().Point().String(), Span: nhf015WireCalendarSpan(item.Span()), Zone: zone.Name(), Summary: item.Summary(), Description: item.Description(), Location: item.Location()})
	}
	// Read exceptions from one committed definition image and reject a concurrent
	// edit rather than combining definitions with occurrences from another revision.
	definitions, e := h.service.ListEvents(permit.Context(), id)
	if e != nil {
		return nil, nhf015CalendarRPCError(e)
	}
	for _, snapshot := range definitions {
		if snapshot.CalendarRevision() != page.Revision() {
			return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_STALE_CURSOR)
		}
		event, present := snapshot.Event()
		if !present {
			continue
		}
		if len(event.Exceptions()) == 0 && len(event.Exclusions()) == 0 {
			continue
		}
		definition := nhf015WireCalendarEvent(event)
		uid, _ := snapshot.Key().UID().Value()
		result.SeriesExceptions = append(result.SeriesExceptions, &cv.SeriesExceptions{Key: &cv.EventKey{Calendar: req.Msg.Id, Uid: uid}, Exceptions: definition.Exceptions, Exclusions: definition.Exclusions})
		if proto.Size(result) > 1<<20 {
			return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_CAPACITY)
		}
	}
	current, e := h.service.GetCalendar(permit.Context(), id)
	if e != nil {
		return nil, nhf015CalendarRPCError(e)
	}
	if current.Revision() != page.Revision() {
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_STALE_CURSOR)
	}
	finalState, isRemote := current.RemoteState()
	result.Quality = nhf015CalendarQuality(finalState, isRemote)
	if until, ok := finalState.FreshUntil(); isRemote && ok {
		text := until.UTC().Format(time.RFC3339Nano)
		result.FreshUntil = &text
	}
	if cursor, more := page.Next(); more {
		result.NextPageToken, e = h.cursors.Encode(cursor, binding)
		if e != nil {
			return nil, nhf015CalendarRPCError(e)
		}
	}
	if proto.Size(result) > 1<<20 {
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_CAPACITY)
	}
	if e = nhf015CalendarFinish(permit, nil, false); e != nil {
		return nil, e
	}
	return connect.NewResponse(result), nil
}

type nhf015CalculatedCursor struct {
	Identity, Profile, Window string
	Revision                  uint64
	Offset                    int
}

func (h *nhf015CalendarHandler) queryCalculated(ctx context.Context, id calendars.CalendarID, window calendars.Window, zone calendars.Zone, req *cv.QueryOccurrencesRequest, binding [32]byte) (*cv.QueryOccurrencesResponse, error) {
	reader, revision, e := h.calculated()
	if e != nil {
		return nil, e
	}
	bound, e := reader.CalendarID()
	if e != nil || bound != id {
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_UNAVAILABLE)
	}
	from, to, e := window.CivilBounds()
	if e != nil {
		return nil, e
	}
	if _, timed := req.Window.Bounds.(*cv.Window_Times); timed {
		to, e = to.AddDays(1)
		if e != nil {
			return nil, e
		}
	}
	var spans []calendars.Span
	split, splitErr := from.AddDays(366)
	if splitErr == nil && split.String() < to.String() {
		first, e := reader.Dates(ctx, from, split)
		if e != nil {
			return nil, e
		}
		second, e := reader.Dates(ctx, split, to)
		if e != nil {
			return nil, e
		}
		spans = append(first, second...)
	} else {
		spans, e = reader.Dates(ctx, from, to)
		if e != nil {
			return nil, e
		}
	}
	entity, _ := id.Entity()
	text, _ := entity.Value()
	encoded, _ := proto.MarshalOptions{Deterministic: true}.Marshal(req.Window)
	windowHash := fmt.Sprintf("%x", sha256.Sum256(encoded))
	expected := nhf015CalculatedCursor{text, workday.ProfileVersion, windowHash, revision, 0}
	if req.PageToken != "" {
		raw, e := h.cursors.Open(req.PageToken, binding)
		if e != nil {
			return nil, e
		}
		var cursor nhf015CalculatedCursor
		if json.Unmarshal(raw, &cursor) != nil || cursor.Identity != expected.Identity || cursor.Profile != expected.Profile || cursor.Window != expected.Window || cursor.Revision != revision || cursor.Offset < 1 || cursor.Offset > 366 {
			return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_STALE_CURSOR)
		}
		expected.Offset = cursor.Offset
	}
	selected := make([]calendars.Span, 0, len(spans))
	for _, span := range spans {
		if ok, e := window.ContainsSpan(ctx, span); e != nil {
			return nil, e
		} else if ok {
			selected = append(selected, span)
		}
	}
	if expected.Offset > len(selected) {
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_STALE_CURSOR)
	}
	last := expected.Offset + int(req.PageSize)
	if last > len(selected) {
		last = len(selected)
	}
	rev, e := calendars.ExpectedRevision(id, calendars.EventUID{}, calendars.Present, revision)
	if e != nil {
		return nil, e
	}
	result := &cv.QueryOccurrencesResponse{Provenance: cv.DataProvenance_DATA_PROVENANCE_SYNTHETIC, Revision: nhf015CalendarRevision(rev, id, ""), Quality: cv.Quality_QUALITY_FRESH}
	for _, span := range selected[expected.Offset:last] {
		date, _, _ := span.Dates()
		uid := "workday:" + workday.ProfileVersion + ":" + date.String()
		result.Occurrences = append(result.Occurrences, &cv.Occurrence{Key: &cv.EventKey{Calendar: req.Id, Uid: uid}, RecurrenceId: date.String(), Span: nhf015WireCalendarSpan(span), Zone: zone.Name(), Summary: "Workday"})
	}
	if last < len(selected) {
		expected.Offset = last
		raw, _ := json.Marshal(expected)
		result.NextPageToken, e = h.cursors.Seal(raw, binding)
		if e != nil {
			return nil, e
		}
	}
	_, after, e := h.calculated()
	if e != nil || after != revision {
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_STALE_CURSOR)
	}
	return result, nil
}
func (h *nhf015CalendarHandler) WatchCalendar(ctx context.Context, req *connect.Request[cv.WatchCalendarRequest], stream *connect.ServerStream[cv.WatchCalendarResponse]) error {
	if req == nil || !nhf015CalendarKnown(req.Msg) {
		return nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
	}
	id, permit, e := h.bind(ctx, req.Msg.Id, av.ScopeOperation_SCOPE_OPERATION_READ)
	if e != nil {
		return e
	}
	defer permit.Release()
	defer interruptCoreRPCWriteOnCancel(ctx, permit.Context())()
	sub, e := h.service.Subscribe()
	if e != nil {
		return nhf015CalendarRPCError(e)
	}
	defer sub.Close()
	var sequence uint64
	send := func(frame *cv.WatchCalendarResponse) error {
		if !permit.Ready() {
			return nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_DENIED)
		}
		sequence++
		frame.StreamSequence = sequence
		return stream.Send(frame)
	}
	var calculatedRevision uint64
	if id.Origin() == calendars.Calculated {
		_, calculatedRevision, e = h.calculated()
		if e != nil {
			return e
		}
	}
	// Ready always instructs reload. Reconnect has no replay promise; changes
	// carry only revision identity and every termination needs a fresh snapshot.
	if e := send(&cv.WatchCalendarResponse{Frame: &cv.WatchCalendarResponse_Ready{Ready: &emptypb.Empty{}}}); e != nil {
		return e
	}
	if e := send(&cv.WatchCalendarResponse{Frame: &cv.WatchCalendarResponse_ResyncRequired{ResyncRequired: &emptypb.Empty{}}}); e != nil {
		return e
	}
	if h.afterWatchReady != nil {
		h.afterWatchReady()
	}
	if id.Origin() == calendars.Calculated {
		for {
			_, after, e := h.calculated()
			if e != nil || after != calculatedRevision {
				if e := send(&cv.WatchCalendarResponse{Frame: &cv.WatchCalendarResponse_ResyncRequired{ResyncRequired: &emptypb.Empty{}}}); e != nil {
					return e
				}
				return nil
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-permit.Context().Done():
				timer.Stop()
				return nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_DENIED)
			case <-timer.C:
			}
		}
	}
	for {
		change, e := sub.Next(permit.Context())
		if e != nil {
			if !permit.Ready() {
				return nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_DENIED)
			}
			if x := send(&cv.WatchCalendarResponse{Frame: &cv.WatchCalendarResponse_ResyncRequired{ResyncRequired: &emptypb.Empty{}}}); x != nil {
				return x
			}
			return nhf015CalendarRPCError(e)
		}
		if change.Calendar() != id {
			continue
		}
		if e := send(&cv.WatchCalendarResponse{Frame: &cv.WatchCalendarResponse_Changed{Changed: &cv.CalendarChanged{Revision: nhf015CalendarRevision(change.Revision(), id, "")}}}); e != nil {
			return e
		}
	}
}

func (h *nhf015CalendarHandler) decorate(ctx context.Context, value *cv.Calendar) {
	value.Provenance = cv.DataProvenance_DATA_PROVENANCE_SYNTHETIC
	value.MutationAccess = cv.MutationAccess_MUTATION_ACCESS_DENIED
	if value.Origin != cv.Origin_ORIGIN_LOCAL {
		return
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return
	}
	current, err := h.authority.Current(principal)
	if err != nil {
		return
	}
	entity, err := domain.ParseEntityID(value.GetId().GetEntityId())
	if err != nil {
		return
	}
	object, err := authorization.CalendarObject(entity)
	if err != nil {
		return
	}
	permission, err := authorization.NewPermission(av.ScopeFamily_SCOPE_FAMILY_CALENDARS, av.ScopeOperation_SCOPE_OPERATION_WRITE)
	if err == nil && current.Authorize(permission, object) == nil {
		value.MutationAccess = cv.MutationAccess_MUTATION_ACCESS_ALLOWED
	}
}
func (h *nhf015CalendarHandler) ResolveLocalTime(ctx context.Context, request *connect.Request[cv.ResolveLocalTimeRequest]) (*connect.Response[cv.ResolveLocalTimeResponse], error) {
	if request == nil || !nhf015CalendarKnown(request.Msg) {
		return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_INVALID)
	}
	_, permit, err := h.bind(ctx, request.Msg.Id, av.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return nil, err
	}
	defer permit.Release()
	local, err := calendars.ParseLocalDateTime(request.Msg.Local)
	if err != nil {
		return nil, nhf015CalendarRPCError(err)
	}
	value := &cv.ResolveLocalTimeResponse{Id: proto.Clone(request.Msg.Id).(*cv.CalendarId), Local: request.Msg.Local, Zone: request.Msg.Zone}
	zone, err := calendars.ParseZone(request.Msg.Zone)
	if err != nil {
		value.Resolution = cv.LocalTimeResolution_LOCAL_TIME_RESOLUTION_INVALID_ZONE
	} else {
		candidates, err := calendars.ResolveLocalTime(permit.Context(), local, zone)
		if err != nil {
			return nil, nhf015CalendarRPCError(err)
		}
		switch len(candidates) {
		case 0:
			value.Resolution = cv.LocalTimeResolution_LOCAL_TIME_RESOLUTION_NONEXISTENT
		case 1:
			value.Resolution = cv.LocalTimeResolution_LOCAL_TIME_RESOLUTION_UNIQUE
		case 2:
			value.Resolution = cv.LocalTimeResolution_LOCAL_TIME_RESOLUTION_AMBIGUOUS
		default:
			return nil, nhf015CalendarFailure(cv.ErrorCode_ERROR_CODE_UNAVAILABLE)
		}
		for _, candidate := range candidates {
			value.Candidates = append(value.Candidates, &cv.LocalTimeCandidate{Instant: candidate.Instant().String(), OffsetSeconds: int32(candidate.OffsetSeconds())})
		}
	}
	if err := nhf015CalendarFinish(permit, nil, false); err != nil {
		return nil, err
	}
	return connect.NewResponse(value), nil
}
