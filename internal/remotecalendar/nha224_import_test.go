package remotecalendar

import (
	"context"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/storage"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestNHA224ApplicationImportNoStoreFullPollRestartAnd304Refusal(t *testing.T) {
	f := nha101LiveSetup(t)
	f.importPoll, f.importFreshness = 15*time.Minute, 30*time.Minute
	var gets atomic.Int32
	var reject304 atomic.Bool
	var invalidICS atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" || r.Header.Get("Cache-Control") != "no-cache, no-store" {
			t.Error("import used HTTP cache validators or omitted bypass")
		}
		w.Header().Set("Cache-Control", "max-age=0, must-revalidate, no-cache, no-store, private")
		w.Header().Set("Expires", "Mon, 01 Jan 1990 00:00:00 GMT")
		w.Header().Set("Etag", `"must-not-persist"`)
		w.Header().Set("Last-Modified", "Mon, 01 Jan 1990 00:00:00 GMT")
		if reject304.Load() {
			w.WriteHeader(304)
			return
		}
		if invalidICS.Load() {
			_, _ = w.Write([]byte("invalid synthetic ICS"))
			return
		}
		_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:synthetic-import\r\nDTSTART:20261019T120000Z\r\nDURATION:PT30M\r\nSUMMARY:Synthetic event\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	}))
	defer server.Close()
	pub := &nha101RecordingPublisher{}
	pilot, supervisor, service, _ := f.start(t, server.URL, pub, nil)
	nha101Eventually(t, func() bool { return pub.count() == 1 })
	nha101Idle(t, pilot)
	inspect := func(p *Pilot) {
		t.Helper()
		p.run.mu.Lock()
		writer := p.run.writer
		scope := p.run.scope
		p.run.mu.Unlock()
		cache, err := writer.Read(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if cache.Selection().Metadata().Present() || cache.Validators().Kind() != storage.RemoteValidatorsNone || cache.Selection().Conditional(scope) {
			t.Fatal("application import persisted an HTTP cache")
		}
		state, _ := cache.Calendar().RemoteState()
		fresh, _ := state.Freshness()
		until, _ := state.FreshUntil()
		if fresh != calendars.RemoteFresh || !until.Equal(f.clock.Now().Add(30*time.Minute)) {
			t.Fatal("import freshness not the explicit observation deadline", until)
		}
	}
	inspect(pilot)
	from, _ := calendars.NewInstant(f.clock.Now())
	to, _ := calendars.NewInstant(f.clock.Now().Add(24 * time.Hour))
	zone, _ := calendars.ParseZone("UTC")
	window, _ := calendars.TimeWindow(from, to, zone)
	page, err := service.Query(f.ctx, pilot.source.id, window, 10, calendars.Cursor{})
	if err != nil || len(page.Items()) != 1 || page.Items()[0].Summary() != "Synthetic event" {
		t.Fatal("imported application data not queryable", err)
	}
	first := pub.last()
	f.clock.advance(15*time.Minute, 15*time.Minute)
	nha101Eventually(t, func() bool { return pub.count() == 2 })
	nha101Idle(t, pilot)
	inspect(pilot)
	if gets.Load() != 2 || pub.last().Revision() == first.Revision() {
		t.Fatal("configured poll did not commit a complete representation")
	}
	reject304.Store(true)
	if err := pilot.Refresh(f.ctx); err != nil {
		t.Fatal(err)
	}
	nha101Eventually(t, func() bool {
		snapshot, err := service.GetCalendar(f.ctx, pilot.source.id)
		if err != nil {
			return false
		}
		state, _ := snapshot.RemoteState()
		fresh, _ := state.Freshness()
		return fresh == calendars.RemoteStale
	})
	if pub.count() != 2 {
		t.Fatal("unrequested304 renewed application freshness")
	}
	stop, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := supervisor.Stop(stop); err != nil {
		t.Fatal(err)
	}
	reject304.Store(false)
	nextPub := &nha101RecordingPublisher{}
	next, _, _, _ := f.start(t, server.URL, nextPub, nil)
	nha101Eventually(t, func() bool { return nextPub.count() == 1 })
	nha101Idle(t, next)
	inspect(next)
	if gets.Load() != 4 {
		t.Fatal("restart did not fetch exactly one complete representation", gets.Load())
	}
	invalidICS.Store(true)
	if err := next.Refresh(f.ctx); err != nil {
		t.Fatal(err)
	}
	nha101Eventually(t, func() bool {
		next.run.mu.Lock()
		fresh := next.run.fresh
		next.run.mu.Unlock()
		return !fresh
	})
	if nextPub.count() != 1 {
		t.Fatal("invalid ICS renewed imported data")
	}
	invalidICS.Store(false)
	if err := next.Refresh(f.ctx); err != nil {
		t.Fatal(err)
	}
	nha101Eventually(t, func() bool { return nextPub.count() == 2 })
	nha101Idle(t, next)
	// A monotonic expiry cannot become fresh again after a backward wall clock.
	f.clock.advance(-time.Hour, 30*time.Minute)
	nha101Eventually(t, func() bool {
		next.run.mu.Lock()
		fresh := next.run.fresh
		next.run.mu.Unlock()
		return !fresh
	})
}
