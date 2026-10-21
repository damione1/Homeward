package remotecalendar

import (
	"context"
	"crypto/sha256"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNHA101CollectorRejectsUnadmittedWindow(t *testing.T) {
	var s Source
	if _, e := s.request(nil); e == nil {
		t.Fatal("zero source produced window")
	}
	var run sourceRun
	if run.publish(context.Background(), calendars.AutomationWindow{}, false) == nil {
		t.Fatal("zero window published")
	}
}

func TestNHA101PilotSQLChangeAfterCollectionBeforePublication(t *testing.T) {
	f := nha101LiveSetup(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Etag", `"selected"`)
		_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n"))
	}))
	defer server.Close()
	changed := make(chan struct{}, 1)
	pub := &nha101RecordingPublisher{}
	_, _, service, _ := f.start(t, server.URL, pub, func(window calendars.AutomationWindow) {
		var header storage.RemoteCalendarHeader
		var binding storage.RemoteCalendarBinding
		e := f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
			var e error
			binding, e = storage.ReadRemoteCalendarBinding(f.ctx, tx, f.entity, false)
			if e != nil {
				return e
			}
			header, e = storage.ReadRemoteCalendarHeader(f.ctx, tx, binding, false)
			return e
		})
		if e != nil {
			t.Error(e)
			return
		}
		// Only a real owner Store commit produces this competing version.
		f.phase.mu.Lock()
		scope := f.phase.scope
		f.phase.mu.Unlock()
		expected, e := storage.ExpectRemoteCalendar(header, scope)
		if e != nil {
			t.Error(e)
			return
		}
		at := f.clock.Now()
		o, e := storage.NewRemoteCalendarObservation(scope, sha256.Sum256([]byte("competing-real-commit")), at, at.Add(time.Hour), header.Validators())
		if e != nil {
			t.Error(e)
			return
		}
		receipt, _, e := f.store.RevalidateRemoteCalendar(f.ctx, expected, o, header.Validators(), header.Digest())
		if e != nil || !receipt.Valid() {
			t.Error("competing Store commit refused", e)
			return
		}
		changed <- struct{}{}
	})
	select {
	case <-changed:
	case <-time.After(4 * time.Second):
		t.Fatal("did not reach post-collection competing commit")
	}
	nha101Eventually(t, func() bool { pub.mu.Lock(); defer pub.mu.Unlock(); return pub.degraded > 1 })
	if pub.count() != 0 {
		t.Fatal("outdated collected version published")
	}
	_ = service
}
