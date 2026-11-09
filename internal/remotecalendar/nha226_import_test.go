package remotecalendar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/calendars"
)

func TestNHA226ApplicationImportEmbeddedZoneSQLRestart(t *testing.T) {
	f := nha101LiveSetup(t)
	f.importPoll, f.importFreshness = 15*time.Minute, 30*time.Minute
	raw, err := os.ReadFile("../calendars/testdata/nha226/multi-year-zone.ics")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, private")
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	pub := &nha101RecordingPublisher{}
	pilot, supervisor, service, _ := f.start(t, server.URL, pub, nil)
	nha101Eventually(t, func() bool { return pub.count() == 1 })
	nha101Idle(t, pilot)
	verify := func(service *calendars.Service, pilot *Pilot) {
		t.Helper()
		snapshot, err := service.GetCalendar(f.ctx, pilot.source.id)
		if err != nil {
			t.Fatal(err)
		}
		state, _ := snapshot.RemoteState()
		fresh, _ := state.Freshness()
		if fresh != calendars.RemoteFresh {
			t.Fatal("import not fresh")
		}
		from, _ := calendars.ParseInstant("2026-10-19T00:00:00Z")
		to, _ := calendars.ParseInstant("2026-10-20T00:00:00Z")
		zone, _ := calendars.ParseZone("UTC")
		window, _ := calendars.TimeWindow(from, to, zone)
		page, err := service.Query(f.ctx, pilot.source.id, window, 10, calendars.Cursor{})
		if err != nil || len(page.Items()) != 1 || page.Items()[0].Summary() != "Synthetic later event" {
			t.Fatal("import not queryable", err)
		}
	}
	verify(service, pilot)
	stop, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := supervisor.Stop(stop); err != nil {
		t.Fatal(err)
	}
	nextPub := &nha101RecordingPublisher{}
	next, _, nextService, _ := f.start(t, server.URL, nextPub, nil)
	nha101Eventually(t, func() bool { return nextPub.count() == 1 })
	nha101Idle(t, next)
	verify(nextService, next)
}
