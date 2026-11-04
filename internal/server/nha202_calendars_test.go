package server

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/calendarnames"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/config"
	datepb "google.golang.org/genproto/googleapis/type/date"
	timepb "google.golang.org/genproto/googleapis/type/timeofday"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func nha202CalendarAPI(t *testing.T) (*nhf015CalendarRig, apiv2connect.CalendarServiceClient, apitypes.HomeName, map[calendars.Origin]calendarnames.CalendarName) {
	t.Helper()
	r := nhf015NewCalendarRig(t, 120*time.Second)
	home, err := r.lists.registry.HomeName(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	cursors, err := calendars.NewCursorCodec()
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newCalendarResourceHandler(r.handler.service, r.coordinator.authority, cursors, r.lists.registry, r.handler.entities, api.CalendarProvenance_CALENDAR_PROVENANCE_SYNTHETIC)
	if err != nil {
		t.Fatal(err)
	}
	handler.workday = r.handler.workday
	names := map[calendars.Origin]calendarnames.CalendarName{}
	for _, entity := range handler.entities {
		id, err := handler.service.Bind(r.ctx, entity)
		if err != nil {
			t.Fatal(err)
		}
		name, err := calendarnames.Calendar(home, entity)
		if err != nil {
			t.Fatal(err)
		}
		names[id.Origin()] = name
	}
	plan, err := NewPublicPlan(nil, nil, accountTransportAudit{store: r.store})
	if err != nil {
		t.Fatal(err)
	}
	if err = registerCalendarResourceRoutes(plan, handler); err != nil {
		t.Fatal(err)
	}
	plan, _, err = composeSessionsWithCoordinator(plan, r.store, r.coordinator)
	if err != nil {
		t.Fatal(err)
	}
	settings := config.DefaultPublicSettings()
	compiled, err := compilePublicPlan(plan, settings)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(compiled.handler(settings, newPublicAdmissions()))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	return r, apiv2connect.NewCalendarServiceClient(server.Client(), server.URL), home, names
}
func nha202CalendarCreate(t *testing.T, r *nhf015CalendarRig, client apiv2connect.CalendarServiceClient, home apitypes.HomeName, name calendarnames.CalendarName) *api.Calendar {
	t.Helper()
	source, err := apitypes.EntityNameForIdentity(home, name.Entity())
	if err != nil {
		t.Fatal(err)
	}
	created, err := client.CreateCalendar(r.ctx, nha162Bearer(&api.CreateCalendarRequest{Parent: home.String(), Calendar: &api.Calendar{SourceEntity: source.String(), DisplayName: "Synthetic appointments"}}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	return created.Msg
}
func nha202CalendarDayEvent() *api.CalendarEvent {
	return &api.CalendarEvent{Summary: "Synthetic appointment", Description: "Explicitly editable", Span: &api.CalendarSpan{Value: &api.CalendarSpan_Dates{Dates: &api.CalendarDateSpan{StartDate: &datepb.Date{Year: 2026, Month: 11, Day: 3}, EndDate: &datepb.Date{Year: 2026, Month: 11, Day: 4}}}}, RecurrenceRule: proto.String("FREQ=DAILY;COUNT=3")}
}
func TestNHA202CalendarResourceMaskDedupPaginationAndReadOnlyTLS(t *testing.T) {
	r, client, home, names := nha202CalendarAPI(t)
	ctx, access := r.ctx, r.adminToken
	local := names[calendars.Local]
	sources, err := client.QueryCalendarSources(ctx, nha162Bearer(&api.QueryCalendarSourcesRequest{Parent: home.String()}, access))
	if err != nil || len(sources.Msg.CalendarSources) != 3 {
		t.Fatal("configured source discovery", err)
	}
	localAbsent := false
	for _, source := range sources.Msg.CalendarSources {
		if source.Calendar == local.String() {
			localAbsent = !source.Materialized && source.MutationAccess == api.CalendarMutationAccess_CALENDAR_MUTATION_ACCESS_ALLOWED && source.SourceEntity != ""
		}
		if source.Origin != api.CalendarOrigin_CALENDAR_ORIGIN_LOCAL && source.MutationAccess != api.CalendarMutationAccess_CALENDAR_MUTATION_ACCESS_DENIED {
			t.Fatal("source mutability lost")
		}
	}
	if !localAbsent {
		t.Fatal("absent local source disappeared before CreateCalendar")
	}
	_, err = client.GetCalendar(ctx, nha162Bearer(&api.GetCalendarRequest{Name: local.String()}, access))
	nha162Code(t, err, connect.CodeNotFound)
	created := nha202CalendarCreate(t, r, client, home, local)
	if created.Name != local.String() || created.Etag == "" {
		t.Fatal("create did not return calendar resource")
	}
	sources, err = client.QueryCalendarSources(ctx, nha162Bearer(&api.QueryCalendarSourcesRequest{Parent: home.String()}, access))
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range sources.Msg.CalendarSources {
		if source.Calendar == local.String() && (!source.Materialized || source.DisplayName != created.DisplayName) {
			t.Fatal("known native calendar label lost")
		}
	}
	// Opaque admission never interprets a caller's revision codec. Well-formed
	// bounded values, including formerly emitted spellings, fail the live CAS.
	for _, stale := range []string{"different-opaque-token", created.Etag[2:]} {
		_, err := client.UpdateCalendar(ctx, nha162Bearer(&api.UpdateCalendarRequest{Calendar: &api.Calendar{Name: local.String(), Etag: stale, DisplayName: "Must not commit"}}, access))
		nha162Code(t, err, connect.CodeAborted)
	}
	for _, origin := range []calendars.Origin{calendars.RemoteICS, calendars.Calculated} {
		source, err := apitypes.EntityNameForIdentity(home, names[origin].Entity())
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.CreateCalendar(ctx, nha162Bearer(&api.CreateCalendarRequest{Parent: home.String(), Calendar: &api.Calendar{SourceEntity: source.String(), DisplayName: "Denied source mutation"}}, access))
		nha162Code(t, err, connect.CodeFailedPrecondition)
	}
	first, err := client.CreateCalendarEvent(ctx, nha162Bearer(&api.CreateCalendarEventRequest{Parent: local.String(), CalendarEvent: nha202CalendarDayEvent()}, access))
	if err != nil {
		t.Fatal(err)
	}
	for _, stale := range []string{"different-opaque-token", first.Msg.Etag[2:]} {
		_, err := client.UpdateCalendarEvent(ctx, nha162Bearer(&api.UpdateCalendarEventRequest{CalendarEvent: &api.CalendarEvent{Name: first.Msg.Name, Etag: stale, Summary: "Must not commit"}}, access))
		nha162Code(t, err, connect.CodeAborted)
	}
	noop, err := client.UpdateCalendarEvent(ctx, nha162Bearer(&api.UpdateCalendarEventRequest{CalendarEvent: &api.CalendarEvent{Name: first.Msg.Name, Etag: first.Msg.Etag, Summary: "Ignored"}, UpdateMask: &fieldmaskpb.FieldMask{}}, access))
	if err != nil || !proto.Equal(first.Msg, noop.Msg) {
		t.Fatalf("empty event mask changed resource: %v", err)
	}
	patch := &api.UpdateCalendarEventRequest{CalendarEvent: &api.CalendarEvent{Name: first.Msg.Name, Etag: first.Msg.Etag, Summary: "Updated", Description: ""}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"summary", "description"}}, RequestId: "42aee782-525a-4d39-8994-3053fdd813a0"}
	updated, err := client.UpdateCalendarEvent(ctx, nha162Bearer(patch, access))
	if err != nil || updated.Msg.Summary != "Updated" || updated.Msg.Description != "" || updated.Msg.RecurrenceRule == nil {
		t.Fatalf("partial event merge lost content: %v", err)
	}
	replay, err := client.UpdateCalendarEvent(ctx, nha162Bearer(patch, access))
	if err != nil || !proto.Equal(updated.Msg, replay.Msg) {
		t.Fatalf("event replay did not return exact original resource: %v", err)
	}
	changed := proto.Clone(patch).(*api.UpdateCalendarEventRequest)
	changed.CalendarEvent.Summary = "Different intention"
	_, err = client.UpdateCalendarEvent(ctx, nha162Bearer(changed, access))
	nha162Code(t, err, connect.CodeAlreadyExists)
	second := nha202CalendarDayEvent()
	second.Summary = "Second"
	if _, err = client.CreateCalendarEvent(ctx, nha162Bearer(&api.CreateCalendarEventRequest{Parent: local.String(), CalendarEvent: second}, access)); err != nil {
		t.Fatal(err)
	}
	page, err := client.ListCalendarEvents(ctx, nha162Bearer(&api.ListCalendarEventsRequest{Parent: local.String(), PageSize: 1}, access))
	if err != nil || len(page.Msg.CalendarEvents) != 1 || page.Msg.NextPageToken == "" {
		t.Fatalf("event pagination: %v", err)
	}
	last, err := client.ListCalendarEvents(ctx, nha162Bearer(&api.ListCalendarEventsRequest{Parent: local.String(), PageSize: 1000, PageToken: page.Msg.NextPageToken}, access))
	if err != nil || len(last.Msg.CalendarEvents) != 1 || last.Msg.NextPageToken != "" {
		t.Fatalf("event page size change: %v", err)
	}
	occurrences, err := client.QueryOccurrences(ctx, nha162Bearer(&api.QueryOccurrencesRequest{Parent: local.String() + "/events/-", Window: &api.CalendarWindow{TimeZone: "America/Toronto", Bounds: &api.CalendarWindow_Dates{Dates: &api.CalendarDateSpan{StartDate: &datepb.Date{Year: 2026, Month: 11, Day: 1}, EndDate: &datepb.Date{Year: 2026, Month: 11, Day: 10}}}}}, access))
	if err != nil || len(occurrences.Msg.Occurrences) != 6 || occurrences.Msg.CalendarEtag == "" {
		t.Fatalf("bounded recurring occurrences: %v", err)
	}
	current, err := client.GetCalendar(ctx, nha162Bearer(&api.GetCalendarRequest{Name: local.String()}, access))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.DeleteCalendar(ctx, nha162Bearer(&api.DeleteCalendarRequest{Name: local.String(), Etag: current.Msg.Etag}, access))
	nha162Code(t, err, connect.CodeFailedPrecondition)
	if _, err = client.GetCalendarEvent(ctx, nha162Bearer(&api.GetCalendarEventRequest{Name: first.Msg.Name}, access)); err != nil {
		t.Fatal("default delete damaged events", err)
	}
	_, err = client.DeleteCalendar(ctx, nha162Bearer(&api.DeleteCalendarRequest{Name: local.String(), Etag: current.Msg.Etag, Force: true}, access))
	if err != nil {
		t.Fatal("explicit event cascade", err)
	}
	_, err = client.GetCalendarEvent(ctx, nha162Bearer(&api.GetCalendarEventRequest{Name: first.Msg.Name}, access))
	nha162Code(t, err, connect.CodeNotFound)
	sources, err = client.QueryCalendarSources(ctx, nha162Bearer(&api.QueryCalendarSourcesRequest{Parent: home.String()}, access))
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range sources.Msg.CalendarSources {
		if source.Calendar == local.String() && (source.Materialized || source.DisplayName != "" || source.MutationAccess != api.CalendarMutationAccess_CALENDAR_MUTATION_ACCESS_DENIED) {
			t.Fatal("removed source advertised writable or materialized calendar")
		}
	}

}
func TestNHA202CalendarDSTResolutionAndRevocableWatchTLS(t *testing.T) {
	r, client, home, names := nha202CalendarAPI(t)
	ctx, access := r.ctx, r.adminToken
	local := names[calendars.Local]
	nha202CalendarCreate(t, r, client, home, local)
	request := &api.ResolveCalendarTimeRequest{Name: local.String(), Local: &api.CalendarCivilTime{Date: &datepb.Date{Year: 2026, Month: 11, Day: 1}, Time: &timepb.TimeOfDay{Hours: 1, Minutes: 30}}, TimeZone: "America/Toronto"}
	fold, err := client.ResolveCalendarTime(ctx, nha162Bearer(request, access))
	if err != nil || fold.Msg.Resolution != api.CalendarTimeResolution_CALENDAR_TIME_RESOLUTION_AMBIGUOUS || len(fold.Msg.Candidates) != 2 || !fold.Msg.Candidates[0].SelectedTime.AsTime().Before(fold.Msg.Candidates[1].SelectedTime.AsTime()) {
		t.Fatalf("fold was guessed or discarded: %v", err)
	}
	request.Local = &api.CalendarCivilTime{Date: &datepb.Date{Year: 2026, Month: 3, Day: 8}, Time: &timepb.TimeOfDay{Hours: 2, Minutes: 30}}
	gap, err := client.ResolveCalendarTime(ctx, nha162Bearer(request, access))
	if err != nil || gap.Msg.Resolution != api.CalendarTimeResolution_CALENDAR_TIME_RESOLUTION_NONEXISTENT || len(gap.Msg.Candidates) != 0 {
		t.Fatalf("gap was silently coerced: %v", err)
	}
	watchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stream, err := client.WatchCalendar(watchCtx, nha162Bearer(&api.WatchCalendarRequest{Name: local.String()}, access))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() || stream.Msg().GetReady() == nil || stream.Msg().Sequence != 1 {
		t.Fatal("watch ready missing", stream.Err())
	}
	nha162SetPolicy(t, r.nha162RPCRig, r.admin, "  []\n")
	if stream.Receive() {
		t.Fatal("calendar frame emitted after revocation barrier")
	}
	nha162Code(t, stream.Err(), connect.CodePermissionDenied)
}
func TestNHA202WireResolvedRecurrencePointKeepsSelectedFold(t *testing.T) {
	source, err := calendars.ParsePoint(calendars.Timed, "2026-11-01T01:30:00@2026-11-01T06:30:00Z")
	if err != nil {
		t.Fatal(err)
	}
	wire := calendarWirePoint(source)
	if wire.GetResolved() == nil || wire.GetResolved().SelectedTime.AsTime().Hour() != 6 {
		t.Fatal("explicit occurrence fold lost in resource projection")
	}
	roundtrip, err := calendarPoint(calendars.Timed, wire)
	if err != nil || roundtrip != source {
		t.Fatal("typed explicit recurrence point did not roundtrip")
	}
}
