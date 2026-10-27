package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	cv "github.com/damione1/Homeward/gen/home/calendars/v1"
	"github.com/damione1/Homeward/gen/home/calendars/v1/calendarsv1connect"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/workday"
	"github.com/damione1/Homeward/scope"
	"google.golang.org/protobuf/proto"
)

const nhf015AllCalendars = `  - scope: {family: calendars, operation: read}
    objects: {entities: {all: {capabilities: {all: {}}}}}
  - scope: {family: calendars, operation: write}
    objects: {entities: {all: {capabilities: {all: {}}}}}
`

type nhf015CalendarRig struct {
	*nha162RPCRig
	handler        *nhf015CalendarHandler
	api            calendarsv1connect.CalendarServiceClient
	ids            map[calendars.Origin]*cv.CalendarId
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
	handler := &nhf015CalendarHandler{service: service, authority: base.coordinator.authority, cursors: cursors}
	ids := make(map[calendars.Origin]*cv.CalendarId)
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
		text, _ := configured.Entity.Value()
		ids[origin] = &cv.CalendarId{EntityId: text}
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
	if err = nhf015RegisterCalendarRoutes(plan, handler); err != nil {
		t.Fatal(err)
	}
	plan, _, err = composeSessionsWithCoordinator(plan, base.store, base.coordinator)
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
	return &nhf015CalendarRig{base, handler, calendarsv1connect.NewCalendarServiceClient(client, server.URL), ids, server}
}
func (r *nhf015CalendarRig) create(t *testing.T) *cv.CalendarId {
	t.Helper()
	id := r.ids[calendars.Local]
	current, err := r.api.GetCalendar(r.ctx, nha162Bearer(&cv.GetCalendarRequest{Id: id}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.api.MutateCalendar(r.ctx, nha162Bearer(&cv.MutateCalendarRequest{Id: id, Mutation: cv.Mutation_MUTATION_CREATE, ExpectedRevision: current.Msg.Calendar.Revision, Name: "Synthetic appointments"}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func (r *nhf015CalendarRig) add(t *testing.T, id *cv.CalendarId, uid string, event *cv.Event) *cv.MutationReceipt {
	t.Helper()
	key := &cv.EventKey{Calendar: id, Uid: uid}
	response, err := r.api.MutateEvent(r.ctx, nha162Bearer(&cv.MutateEventRequest{Key: key, Mutation: cv.Mutation_MUTATION_CREATE, ExpectedRevision: &cv.Revision{Resource: key, Presence: cv.Presence_PRESENCE_ABSENT}, Value: event}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	return response.Msg.Receipt
}
func nhf015DayEvent() *cv.Event {
	return &cv.Event{Summary: "Civil appointment", Span: &cv.Span{Value: &cv.Span_Dates{Dates: &cv.DateSpan{Start: "2026-11-01", EndExclusive: "2026-11-02"}}}}
}
func nhf015Week(id *cv.CalendarId) *cv.QueryOccurrencesRequest {
	return &cv.QueryOccurrencesRequest{Id: id, PageSize: 1, Window: &cv.Window{Zone: "America/Toronto", Bounds: &cv.Window_Dates{Dates: &cv.DateSpan{Start: "2026-11-01", EndExclusive: "2026-11-08"}}}}
}
func TestNHF015CalendarTLSCRUDCASAndSourceImmutability(t *testing.T) {
	r := nhf015NewCalendarRig(t, 120*time.Second)
	id := r.create(t)
	catalog, err := r.api.ListCalendars(r.ctx, nha162Bearer(&cv.ListCalendarsRequest{}, r.adminToken))
	if err != nil || len(catalog.Msg.Calendars) != 3 {
		t.Fatalf("catalogue: %v", err)
	}
	for _, calendar := range catalog.Msg.Calendars {
		if calendar.Provenance != cv.DataProvenance_DATA_PROVENANCE_SYNTHETIC {
			t.Fatal("mock provenance missing")
		}
		if calendar.Origin != cv.Origin_ORIGIN_LOCAL && calendar.MutationAccess != cv.MutationAccess_MUTATION_ACCESS_DENIED {
			t.Fatal("read-only origin granted edits")
		}
	}
	_, err = r.api.ListCalendars(r.ctx, connect.NewRequest(&cv.ListCalendarsRequest{}))
	nha162Code(t, err, connect.CodeUnauthenticated)
	original := r.add(t, id, "stable-uid", nhf015DayEvent())
	request := &cv.MutateEventRequest{Key: original.Revision.Resource, Mutation: cv.Mutation_MUTATION_UPDATE, ExpectedRevision: original.Revision, Value: nhf015DayEvent()}
	request.Value.Summary = "Updated"
	updated, err := r.api.MutateEvent(r.ctx, nha162Bearer(request, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.api.MutateEvent(r.ctx, nha162Bearer(request, r.adminToken))
	nha162Code(t, err, connect.CodeAborted)
	stored, err := r.api.GetEvent(r.ctx, nha162Bearer(&cv.GetEventRequest{Key: original.Revision.Resource}, r.adminToken))
	if err != nil || stored.Msg.Event.Value.Summary != "Updated" {
		t.Fatalf("durable read: %v", err)
	}
	for _, origin := range []calendars.Origin{calendars.RemoteICS, calendars.Calculated} {
		other := r.ids[origin]
		_, err = r.api.MutateCalendar(r.ctx, nha162Bearer(&cv.MutateCalendarRequest{Id: other, Mutation: cv.Mutation_MUTATION_CREATE, ExpectedRevision: &cv.Revision{Resource: &cv.EventKey{Calendar: other}, Presence: cv.Presence_PRESENCE_ABSENT}, Name: "forbidden"}, r.adminToken))
		nha162Code(t, err, connect.CodeFailedPrecondition)
		copy := proto.Clone(request).(*cv.MutateEventRequest)
		copy.Key.Calendar = other
		_, err = r.api.MutateEvent(r.ctx, nha162Bearer(copy, r.adminToken))
		nha162Code(t, err, connect.CodeFailedPrecondition)
	}
	_, err = r.api.MutateEvent(r.ctx, nha162Bearer(&cv.MutateEventRequest{Key: original.Revision.Resource, Mutation: cv.Mutation_MUTATION_DELETE, ExpectedRevision: original.Revision}, r.adminToken))
	nha162Code(t, err, connect.CodeAborted)
	deleted, err := r.api.MutateEvent(r.ctx, nha162Bearer(&cv.MutateEventRequest{Key: original.Revision.Resource, Mutation: cv.Mutation_MUTATION_DELETE, ExpectedRevision: updated.Msg.Receipt.Revision}, r.adminToken))
	if err != nil || deleted.Msg.Receipt.Revision.Presence != cv.Presence_PRESENCE_REMOVED {
		t.Fatalf("delete: %v", err)
	}
}
func TestNHF015CalendarTLSExplicitFoldMixedPagesAndNoReplay(t *testing.T) {
	r := nhf015NewCalendarRig(t, 120*time.Second)
	id := r.create(t)
	resolution, err := r.api.ResolveLocalTime(r.ctx, nha162Bearer(&cv.ResolveLocalTimeRequest{Id: id, Local: "2026-11-01T01:30:00", Zone: "America/Toronto"}, r.adminToken))
	if err != nil || resolution.Msg.Resolution != cv.LocalTimeResolution_LOCAL_TIME_RESOLUTION_AMBIGUOUS || len(resolution.Msg.Candidates) != 2 {
		t.Fatalf("fold resolution: %v", err)
	}
	second := resolution.Msg.Candidates[1].Instant
	timed := &cv.Event{Summary: "Explicit second fold", Span: &cv.Span{Value: &cv.Span_Times{Times: &cv.TimeSpan{AnchorLocal: "2026-11-01T01:30:00", StartInstant: second, EndInstant: "2026-11-01T07:30:00Z", Zone: "America/Toronto"}}}}
	receipt := r.add(t, id, "fold-uid", timed)
	snapshot, err := r.api.GetEvent(r.ctx, nha162Bearer(&cv.GetEventRequest{Key: receipt.Revision.Resource}, r.adminToken))
	if err != nil || snapshot.Msg.Event.Value.Span.GetTimes().StartInstant != second {
		t.Fatalf("selected instant changed: %v", err)
	}
	r.add(t, id, "day-uid", nhf015DayEvent())
	query := nhf015Week(id)
	first, err := r.api.QueryOccurrences(r.ctx, nha162Bearer(query, r.adminToken))
	if err != nil || len(first.Msg.Occurrences) != 1 || first.Msg.NextPageToken == "" {
		t.Fatalf("bounded page: %v", err)
	}
	query.PageToken = first.Msg.NextPageToken
	next, err := r.api.QueryOccurrences(r.ctx, nha162Bearer(query, r.adminToken))
	if err != nil || len(next.Msg.Occurrences) != 1 {
		t.Fatalf("next page: %v", err)
	}
	if first.Msg.Occurrences[0].Key.Uid == next.Msg.Occurrences[0].Key.Uid {
		t.Fatal("mixed page duplicated occurrence")
	}
	altered := proto.Clone(query).(*cv.QueryOccurrencesRequest)
	altered.Window.Zone = "UTC"
	_, err = r.api.QueryOccurrences(r.ctx, nha162Bearer(altered, r.adminToken))
	nha162Code(t, err, connect.CodeFailedPrecondition)
	r.handler.dropNextReply.Store(true)
	_, err = r.api.MutateEvent(r.ctx, nha162Bearer(&cv.MutateEventRequest{Key: receipt.Revision.Resource, Mutation: cv.Mutation_MUTATION_UPDATE, ExpectedRevision: receipt.Revision, Value: timed}, r.adminToken))
	nha162Code(t, err, connect.CodeAborted)
	reread, err := r.api.GetEvent(r.ctx, nha162Bearer(&cv.GetEventRequest{Key: receipt.Revision.Resource}, r.adminToken))
	if err != nil || reread.Msg.Event.Revision.Number != receipt.Revision.Number+1 {
		t.Fatalf("lost response did not commit once: %v", err)
	}
	_, err = r.api.QueryOccurrences(r.ctx, nha162Bearer(query, r.adminToken))
	nha162Code(t, err, connect.CodeFailedPrecondition)
	gap, err := r.api.ResolveLocalTime(r.ctx, nha162Bearer(&cv.ResolveLocalTimeRequest{Id: id, Local: "2026-03-08T02:30:00", Zone: "America/Toronto"}, r.adminToken))
	if err != nil || gap.Msg.Resolution != cv.LocalTimeResolution_LOCAL_TIME_RESOLUTION_NONEXISTENT || len(gap.Msg.Candidates) != 0 {
		t.Fatalf("gap normalized: %v", err)
	}
}
func TestNHF015CalendarWatchResyncAndRevocation(t *testing.T) {
	r := nhf015NewCalendarRig(t, 120*time.Second)
	id := r.create(t)
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	stream, err := r.api.WatchCalendar(ctx, nha162Bearer(&cv.WatchCalendarRequest{Id: id}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() || stream.Msg().GetReady() == nil || stream.Msg().StreamSequence != 1 {
		t.Fatalf("first ready: %v", stream.Err())
	}
	if !stream.Receive() || stream.Msg().GetResyncRequired() == nil || stream.Msg().StreamSequence != 2 {
		t.Fatalf("initial resync: %v", stream.Err())
	}
	r.add(t, id, "watch-event", nhf015DayEvent())
	if !stream.Receive() || stream.Msg().GetChanged() == nil || stream.Msg().StreamSequence != 3 {
		t.Fatalf("change: %v", stream.Err())
	}
	nha162SetPolicy(t, r.nha162RPCRig, r.admin, "  []\n")
	if stream.Receive() || stream.Err() == nil {
		t.Fatal("revoked watch remained live")
	}
	_, err = r.api.GetCalendar(r.ctx, nha162Bearer(&cv.GetCalendarRequest{Id: id}, r.adminToken))
	nha162Code(t, err, connect.CodePermissionDenied)
}
