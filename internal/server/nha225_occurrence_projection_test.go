package server

import (
	"context"
	"testing"
	"time"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/calendars"
	datepb "google.golang.org/genproto/googleapis/type/date"
	"google.golang.org/protobuf/proto"
)

func nha225Instant(t *testing.T, raw string) calendars.Instant {
	t.Helper()
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatal(err)
	}
	value, err := calendars.NewInstant(at)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestNHA225OccurrenceProjectionPreservesInstantsAndCivilDates(t *testing.T) {
	ctx := context.Background()
	utc, _ := calendars.ParseZone("UTC")
	target, _ := calendars.ParseZone("America/Toronto")
	for _, test := range []struct {
		start, end         string
		day, hour, endHour int
	}{
		{"2026-11-01T00:30:00Z", "2026-11-01T01:30:00Z", 31, 20, 21},
		{"2026-11-01T06:30:00Z", "2026-11-01T07:30:00Z", 1, 1, 2},
		{"2026-03-08T06:30:00Z", "2026-03-08T07:30:00Z", 8, 1, 3},
	} {
		start, end := nha225Instant(t, test.start), nha225Instant(t, test.end)
		span, err := calendars.TimedInstants(ctx, start, end, utc)
		if err != nil {
			t.Fatal(err)
		}
		projected, err := calendarOccurrenceSpan(ctx, span, target)
		if err != nil {
			t.Fatal(err)
		}
		value := projected.GetTimes()
		if value.TimeZone != target.Name() || value.StartLocal.Date.Day != int32(test.day) || value.StartLocal.Time.Hours != int32(test.hour) || value.EndLocal.Time.Hours != int32(test.endHour) || !proto.Equal(value.StartTime, calendarWireInstant(start)) || !proto.Equal(value.EndTime, calendarWireInstant(end)) {
			t.Fatalf("projection changed instant or local: %v", value)
		}
	}
	anchor, _ := utc.LocalTime(ctx, nha225Instant(t, "2026-11-01T06:30:00Z"))
	point, err := calendars.TimedPoint(ctx, anchor, utc)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := calendarOccurrenceSpan(ctx, point, target)
	if err != nil || !proto.Equal(projected.GetTimes().StartTime, projected.GetTimes().EndTime) || projected.GetTimes().StartLocal.Time.Hours != 1 {
		t.Fatal("point fold lost", err)
	}
	paris, _ := calendars.ParseZone("Europe/Paris")
	anchor, _ = paris.LocalTime(ctx, nha225Instant(t, "2026-03-28T11:00:00Z"))
	length, _ := calendars.NewNominalLength(1, 0)
	nominal, err := calendars.TimedNominalSpan(ctx, anchor, length, paris)
	if err != nil {
		t.Fatal(err)
	}
	start, end, _, _ := nominal.Instants()
	projected, err = calendarOccurrenceSpan(ctx, nominal, target)
	if err != nil || !proto.Equal(projected.GetTimes().StartTime, calendarWireInstant(start)) || !proto.Equal(projected.GetTimes().EndTime, calendarWireInstant(end)) {
		t.Fatal("nominal duration re-resolved", err)
	}
	day, _ := calendars.ParseDate("2026-11-01")
	next, _ := day.AddDays(1)
	allDay, _ := calendars.AllDaySpan(day, next)
	original, _ := calendarWireSpan(ctx, allDay)
	projected, err = calendarOccurrenceSpan(ctx, allDay, target)
	if err != nil || !proto.Equal(original, projected) {
		t.Fatal("civil dates shifted", err)
	}
}

func TestNHA225OccurrenceQueryTLSProjectsViewAndPreservesEventAndIdentity(t *testing.T) {
	r, client, home, names := nha202CalendarAPI(t)
	local := names[calendars.Local]
	nha202CalendarCreate(t, r, client, home, local)
	utc, _ := calendars.ParseZone("UTC")
	span, err := calendars.TimedInstants(r.ctx, nha225Instant(t, "2026-11-01T00:30:00Z"), nha225Instant(t, "2026-11-01T01:30:00Z"), utc)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := calendarWireSpan(r.ctx, span)
	if err != nil {
		t.Fatal(err)
	}
	created, err := client.CreateCalendarEvent(r.ctx, nha162Bearer(&api.CreateCalendarEventRequest{Parent: local.String(), CalendarEvent: &api.CalendarEvent{Summary: "Synthetic midnight", Span: wire, RecurrenceRule: proto.String("FREQ=DAILY;COUNT=2")}}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	query := func(zone string) *api.QueryOccurrencesResponse {
		t.Helper()
		response, err := client.QueryOccurrences(r.ctx, nha162Bearer(&api.QueryOccurrencesRequest{Parent: local.String() + "/events/-", Window: &api.CalendarWindow{TimeZone: zone, Bounds: &api.CalendarWindow_Dates{Dates: &api.CalendarDateSpan{StartDate: &datepb.Date{Year: 2026, Month: 10, Day: 31}, EndDate: &datepb.Date{Year: 2026, Month: 11, Day: 4}}}}}, r.adminToken))
		if err != nil || len(response.Msg.Occurrences) != 2 {
			t.Fatalf("occurrences: %v", err)
		}
		return response.Msg
	}
	original, projected := query("UTC"), query("America/Toronto")
	for i, item := range projected.Occurrences {
		source := original.Occurrences[i]
		if item.TimeZone != "America/Toronto" || item.Span.GetTimes().TimeZone != item.TimeZone || !proto.Equal(source.RecurrencePoint, item.RecurrencePoint) || !proto.Equal(source.Span.GetTimes().StartTime, item.Span.GetTimes().StartTime) || !proto.Equal(source.Span.GetTimes().EndTime, item.Span.GetTimes().EndTime) {
			t.Fatal("projection lost identity/instant")
		}
	}
	first := projected.Occurrences[0].Span.GetTimes().StartLocal
	if first.Date.Month != 10 || first.Date.Day != 31 || first.Time.Hours != 20 || first.Time.Minutes != 30 {
		t.Fatalf("UTC midnight not previous civil day: %v", first)
	}
	got, err := client.GetCalendarEvent(r.ctx, nha162Bearer(&api.GetCalendarEventRequest{Name: created.Msg.Name}, r.adminToken))
	if err != nil || !proto.Equal(got.Msg.Span, created.Msg.Span) || got.Msg.Span.GetTimes().TimeZone != "UTC" {
		t.Fatal("view changed original editor event", err)
	}
}
