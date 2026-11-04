package server

import (
	"connectrpc.com/connect"
	"context"
	"crypto/tls"
	"encoding/json"
	datepb "google.golang.org/genproto/googleapis/type/date"
	timepb "google.golang.org/genproto/googleapis/type/timeofday"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/calendarnames"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/workday"
	"github.com/damione1/Homeward/scope"
)

const nhf015AllCalendars = `  - scope: {family: calendars, operation: read}
    objects: {entities: {all: {capabilities: {all: {}}}}}
  - scope: {family: calendars, operation: write}
    objects: {entities: {all: {capabilities: {all: {}}}}}
`

type nhf015CalendarRig struct {
	*nha162RPCRig
	handler        *nhf015CalendarHandler
	api            apiv2connect.CalendarServiceClient
	ids            map[calendars.Origin]domain.EntityID
	calendarServer *httptest.Server
}

// Select fixture bindings from the server's authoritative manifest instead of
// copying the installation's aliases into a frontend catalogue.
func nhf015CalendarAliases(t *testing.T) map[calendars.Origin]string {
	t.Helper()
	raw, err := os.ReadFile("../../scope/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Entities []struct {
			ID     string `json:"entity_id"`
			Source string `json:"source"`
		} `json:"entities"`
	}
	if err = json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	result := make(map[calendars.Origin]string)
	for _, entity := range manifest.Entities {
		if !strings.HasPrefix(entity.ID, "calendar.") {
			continue
		}
		var origin calendars.Origin
		switch entity.Source {
		case "local_calendar":
			origin = calendars.Local
		case "remote_calendar":
			origin = calendars.RemoteICS
		case "workday":
			origin = calendars.Calculated
		default:
			continue
		}
		if result[origin] == "" {
			result[origin] = entity.ID
		}
	}
	if len(result) != 3 {
		t.Fatal("three retained calendar origins required")
	}
	return result
}
func nhf015NewCalendarRig(t *testing.T, lifetime time.Duration) *nhf015CalendarRig {
	t.Helper()
	base := nha162NewRPCRigWithLifetime(t, lifetime)
	nha162SetPolicy(t, base, base.admin, nhf015AllCalendars)
	manifest, err := registry.LoadManifest(scope.BaselineV1())
	if err != nil {
		t.Fatal(err)
	}
	identities, err := registry.NewRepository(base.store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := calendars.NewRepository(base.store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	service, err := calendars.NewService(repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	cursors, err := calendars.NewCursorCodec()
	if err != nil {
		t.Fatal(err)
	}
	resource, err := newCalendarResourceHandler(service, base.coordinator.authority, cursors, identities, nil, api.CalendarProvenance_CALENDAR_PROVENANCE_SYNTHETIC)
	if err != nil {
		t.Fatal(err)
	}
	handler := &nhf015CalendarHandler{calendarResourceHandler: resource}
	ids := make(map[calendars.Origin]domain.EntityID)
	instance, _ := domain.ParseIntegrationInstanceID("nhf015-synthetic-only")
	aliases := nhf015CalendarAliases(t)
	for _, origin := range []calendars.Origin{calendars.Local, calendars.RemoteICS, calendars.Calculated} {
		alias := aliases[origin]
		key, err := integrations.NewSourceKey("nhf015-synthetic-"+alias, "calendar")
		if err != nil {
			t.Fatal(err)
		}
		configured, err := identities.ConfigureBinding(base.ctx, instance, key, alias)
		if err != nil {
			t.Fatal(err)
		}
		bound, err := service.Bind(base.ctx, configured.Entity)
		if err != nil {
			t.Fatal(err)
		}
		if bound.Origin() != origin {
			t.Fatal("fixture origin differs from registry")
		}
		ids[origin] = configured.Entity
		handler.entities = append(handler.entities, configured.Entity)
		if origin == calendars.Calculated {
			settings, err := workday.ParseSettings(workday.Input{Timezone: "America/Toronto"})
			if err != nil {
				t.Fatal(err)
			}
			handler.workday, err = workday.NewCalendarReader(bound, settings)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	plan, err := NewPublicPlan(nil, nil, accountTransportAudit{store: base.store})
	if err != nil {
		t.Fatal(err)
	}
	if err = registerHomeDiscoveryRoutes(plan, identities, base.coordinator.authority); err != nil {
		t.Fatal(err)
	}
	if err = nhf015RegisterCalendarRoutes(plan, handler); err != nil {
		t.Fatal(err)
	}
	plan, _, err = composeSessionsWithCoordinator(plan, base.store, base.coordinator, administrationNames{home: base.lists.registry, store: base.store})
	if err != nil {
		t.Fatal(err)
	}
	settings := config.DefaultPublicSettings()
	compiled, err := compilePublicPlan(plan, settings)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(compiled.handler(settings, newPublicAdmissions()))
	data := shortPrivateDataDir(t)
	nha181CertificateFixture(t, data)
	pair, err := tls.LoadX509KeyPair(filepath.Join(data, nha181CertificateName), filepath.Join(data, nha181PrivateKeyName))
	if err != nil {
		t.Fatal(err)
	}
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(func() { service.Close(); server.CloseClientConnections(); server.Close() })
	client := server.Client()
	client.Timeout = 30 * time.Second
	return &nhf015CalendarRig{base, handler, apiv2connect.NewCalendarServiceClient(client, server.URL), ids, server}
}

func (r *nhf015CalendarRig) home(t *testing.T) apitypes.HomeName {
	t.Helper()
	home, err := r.lists.registry.HomeName(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return home
}
func (r *nhf015CalendarRig) name(t *testing.T, entity domain.EntityID) calendarnames.CalendarName {
	t.Helper()
	name, err := calendarnames.Calendar(r.home(t), entity)
	if err != nil {
		t.Fatal(err)
	}
	return name
}
func (r *nhf015CalendarRig) create(t *testing.T) domain.EntityID {
	t.Helper()
	entity := r.ids[calendars.Local]
	source, err := apitypes.EntityNameForIdentity(r.home(t), entity)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.api.CreateCalendar(r.ctx, nha162Bearer(&api.CreateCalendarRequest{Parent: r.home(t).String(), Calendar: &api.Calendar{SourceEntity: source.String(), DisplayName: "Synthetic appointments"}}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	return entity
}

// Seeds are explicit trusted fixture writes, never public client-assigned UIDs.
func (r *nhf015CalendarRig) add(t *testing.T, entity domain.EntityID, uidText string, raw *api.CalendarEvent) *api.CalendarEvent {
	t.Helper()
	id, err := r.handler.service.Bind(r.ctx, entity)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := calendars.ParseUID(uidText)
	if err != nil {
		t.Fatal(err)
	}
	key, err := calendars.EventIdentity(id, uid)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := calendars.ExpectedRevision(id, uid, calendars.Absent, 0)
	if err != nil {
		t.Fatal(err)
	}
	event, err := calendarEvent(r.ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	r.handler.mutationRequests.Add(1)
	snapshot, err := r.handler.service.CreateEvent(r.ctx, key, expected, event)
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.handler.wireEvent(r.ctx, r.home(t), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func nhf015DayEvent() *api.CalendarEvent {
	return &api.CalendarEvent{Summary: "Civil appointment", Span: &api.CalendarSpan{Value: &api.CalendarSpan_Dates{Dates: &api.CalendarDateSpan{StartDate: &datepb.Date{Year: 2026, Month: 11, Day: 1}, EndDate: &datepb.Date{Year: 2026, Month: 11, Day: 2}}}}}
}
func nhf015Week(name calendarnames.CalendarName) *api.QueryOccurrencesRequest {
	return &api.QueryOccurrencesRequest{Parent: name.String() + "/events/-", PageSize: 1, Window: &api.CalendarWindow{TimeZone: "America/Toronto", Bounds: &api.CalendarWindow_Dates{Dates: &api.CalendarDateSpan{StartDate: &datepb.Date{Year: 2026, Month: 11, Day: 1}, EndDate: &datepb.Date{Year: 2026, Month: 11, Day: 8}}}}}
}
func TestNHF015CalendarTLSCRUDCASAndSourceImmutability(t *testing.T) {
	r := nhf015NewCalendarRig(t, 120*time.Second)
	entity := r.create(t)
	sources, err := r.api.QueryCalendarSources(r.ctx, nha162Bearer(&api.QueryCalendarSourcesRequest{Parent: r.home(t).String()}, r.adminToken))
	if err != nil || len(sources.Msg.CalendarSources) != 3 {
		t.Fatal("sources", err)
	}
	for _, source := range sources.Msg.CalendarSources {
		if source.Provenance != api.CalendarProvenance_CALENDAR_PROVENANCE_SYNTHETIC {
			t.Fatal("source provenance")
		}
		if source.Origin != api.CalendarOrigin_CALENDAR_ORIGIN_LOCAL && source.MutationAccess != api.CalendarMutationAccess_CALENDAR_MUTATION_ACCESS_DENIED {
			t.Fatal("readonly grant")
		}
	}
	_, err = r.api.ListCalendars(r.ctx, connect.NewRequest(&api.ListCalendarsRequest{Parent: r.home(t).String()}))
	nha162Code(t, err, connect.CodeUnauthenticated)
	original := r.add(t, entity, "stable-uid", nhf015DayEvent())
	value := proto.CloneOf(original)
	value.Summary = "Updated"
	request := &api.UpdateCalendarEventRequest{CalendarEvent: value, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"summary"}}}
	updated, err := r.api.UpdateCalendarEvent(r.ctx, nha162Bearer(request, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.api.UpdateCalendarEvent(r.ctx, nha162Bearer(request, r.adminToken))
	nha162Code(t, err, connect.CodeAborted)
	stored, err := r.api.GetCalendarEvent(r.ctx, nha162Bearer(&api.GetCalendarEventRequest{Name: original.Name}, r.adminToken))
	if err != nil || stored.Msg.Summary != "Updated" {
		t.Fatal("durable read", err)
	}
	for _, origin := range []calendars.Origin{calendars.RemoteICS, calendars.Calculated} {
		name := r.name(t, r.ids[origin])
		source, err := apitypes.EntityNameForIdentity(r.home(t), r.ids[origin])
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.api.CreateCalendar(r.ctx, nha162Bearer(&api.CreateCalendarRequest{Parent: r.home(t).String(), Calendar: &api.Calendar{SourceEntity: source.String(), DisplayName: "forbidden"}}, r.adminToken))
		nha162Code(t, err, connect.CodeFailedPrecondition)
		_, err = r.api.CreateCalendarEvent(r.ctx, nha162Bearer(&api.CreateCalendarEventRequest{Parent: name.String(), CalendarEvent: nhf015DayEvent()}, r.adminToken))
		nha162Code(t, err, connect.CodeFailedPrecondition)
	}
	_, err = r.api.DeleteCalendarEvent(r.ctx, nha162Bearer(&api.DeleteCalendarEventRequest{Name: original.Name, Etag: original.Etag}, r.adminToken))
	nha162Code(t, err, connect.CodeAborted)
	_, err = r.api.DeleteCalendarEvent(r.ctx, nha162Bearer(&api.DeleteCalendarEventRequest{Name: original.Name, Etag: updated.Msg.Etag}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.api.GetCalendarEvent(r.ctx, nha162Bearer(&api.GetCalendarEventRequest{Name: original.Name}, r.adminToken))
	nha162Code(t, err, connect.CodeNotFound)
}
func TestNHF015CalendarTLSExplicitFoldMixedPagesAndNoReplay(t *testing.T) {
	r := nhf015NewCalendarRig(t, 120*time.Second)
	entity := r.create(t)
	name := r.name(t, entity)
	request := &api.ResolveCalendarTimeRequest{Name: name.String(), Local: &api.CalendarCivilTime{Date: &datepb.Date{Year: 2026, Month: 11, Day: 1}, Time: &timepb.TimeOfDay{Hours: 1, Minutes: 30}}, TimeZone: "America/Toronto"}
	resolution, err := r.api.ResolveCalendarTime(r.ctx, nha162Bearer(request, r.adminToken))
	if err != nil || resolution.Msg.Resolution != api.CalendarTimeResolution_CALENDAR_TIME_RESOLUTION_AMBIGUOUS || len(resolution.Msg.Candidates) != 2 {
		t.Fatal("fold", err)
	}
	second := resolution.Msg.Candidates[1].SelectedTime
	end := time.Date(2026, 11, 1, 7, 30, 0, 0, time.UTC)
	timed := &api.CalendarEvent{Summary: "Explicit second fold", Span: &api.CalendarSpan{Value: &api.CalendarSpan_Times{Times: &api.CalendarTimeSpan{StartLocal: request.Local, EndLocal: &api.CalendarCivilTime{Date: &datepb.Date{Year: 2026, Month: 11, Day: 1}, Time: &timepb.TimeOfDay{Hours: 2, Minutes: 30}}, StartTime: second, EndTime: timestamppb.New(end), TimeZone: request.TimeZone}}}}
	original := r.add(t, entity, "fold-uid", timed)
	stored, err := r.api.GetCalendarEvent(r.ctx, nha162Bearer(&api.GetCalendarEventRequest{Name: original.Name}, r.adminToken))
	if err != nil || !proto.Equal(stored.Msg.Span.GetTimes().StartTime, second) {
		t.Fatal("fold selected instant", err)
	}
	r.add(t, entity, "day-uid", nhf015DayEvent())
	query := nhf015Week(name)
	first, err := r.api.QueryOccurrences(r.ctx, nha162Bearer(query, r.adminToken))
	if err != nil || len(first.Msg.Occurrences) != 1 || first.Msg.NextPageToken == "" {
		t.Fatal("first", err)
	}
	query.PageToken = first.Msg.NextPageToken
	next, err := r.api.QueryOccurrences(r.ctx, nha162Bearer(query, r.adminToken))
	if err != nil || len(next.Msg.Occurrences) != 1 || next.Msg.Occurrences[0].CalendarEvent == first.Msg.Occurrences[0].CalendarEvent {
		t.Fatal("mixed page", err)
	}
	altered := proto.CloneOf(query)
	altered.Window.TimeZone = "UTC"
	_, err = r.api.QueryOccurrences(r.ctx, nha162Bearer(altered, r.adminToken))
	nha162Code(t, err, connect.CodeFailedPrecondition)
	value := proto.CloneOf(original)
	value.Summary = "Committed without reply"
	r.handler.dropNextReply.Store(true)
	_, err = r.api.UpdateCalendarEvent(r.ctx, nha162Bearer(&api.UpdateCalendarEventRequest{CalendarEvent: value, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"summary"}}}, r.adminToken))
	nha162Code(t, err, connect.CodeUnknown)
	reread, err := r.api.GetCalendarEvent(r.ctx, nha162Bearer(&api.GetCalendarEventRequest{Name: original.Name}, r.adminToken))
	if err != nil || reread.Msg.Etag == original.Etag || reread.Msg.Summary != value.Summary {
		t.Fatal("single commit after lost reply", err)
	}
	_, err = r.api.QueryOccurrences(r.ctx, nha162Bearer(query, r.adminToken))
	nha162Code(t, err, connect.CodeFailedPrecondition)
	request.Local = &api.CalendarCivilTime{Date: &datepb.Date{Year: 2026, Month: 3, Day: 8}, Time: &timepb.TimeOfDay{Hours: 2, Minutes: 30}}
	gap, err := r.api.ResolveCalendarTime(r.ctx, nha162Bearer(request, r.adminToken))
	if err != nil || gap.Msg.Resolution != api.CalendarTimeResolution_CALENDAR_TIME_RESOLUTION_NONEXISTENT || len(gap.Msg.Candidates) != 0 {
		t.Fatal("gap", err)
	}
}
func TestNHF015CalendarWatchReadyAndRevocation(t *testing.T) {
	r := nhf015NewCalendarRig(t, 120*time.Second)
	entity := r.create(t)
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	stream, err := r.api.WatchCalendar(ctx, nha162Bearer(&api.WatchCalendarRequest{Name: r.name(t, entity).String()}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() || stream.Msg().GetReady() == nil || stream.Msg().Sequence != 1 {
		t.Fatal("ready", stream.Err())
	}
	r.add(t, entity, "watch-event", nhf015DayEvent())
	if !stream.Receive() || stream.Msg().GetChanged() == nil || stream.Msg().Sequence != 2 {
		t.Fatal("change", stream.Err())
	}
	nha162SetPolicy(t, r.nha162RPCRig, r.admin, "  []\n")
	if stream.Receive() || stream.Err() == nil {
		t.Fatal("revoked watch remained live")
	}
	_, err = r.api.GetCalendar(ctx, nha162Bearer(&api.GetCalendarRequest{Name: r.name(t, entity).String()}, r.adminToken))
	nha162Code(t, err, connect.CodePermissionDenied)
}
