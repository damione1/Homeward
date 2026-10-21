package server

import (
	"context"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/testpg"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const nha101AstraPointICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID: point uid \r\nDTSTART:20261101T041000Z\r\nSUMMARY:point\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func nha101AstraRunPilot(t *testing.T, h *nha101RunHandle) *configuredRemoteCalendarPilot {
	t.Helper()
	h.runtime.mu.Lock()
	defer h.runtime.mu.Unlock()
	for p := range h.runtime.active {
		return p
	}
	t.Fatal("real Source pilot absent")
	return nil
}
func nha101AstraNoFired(t *testing.T, h *nha101RunHandle, requireMissed bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	missed := 0
	for {
		result, err := h.results.NextResult(ctx)
		if err != nil {
			break
		}
		if result.Kind() == automationtriggers.ResultFired {
			t.Fatal("interrupted Point replayed a start/end action")
		}
		if result.Kind() == automationtriggers.ResultMissed {
			missed++
		}
	}
	if requireMissed && missed == 0 {
		t.Fatal("Point elapsed before refresh was lost without ResultMissed")
	}
}

func TestNHA101RunRefreshAfterElapsedPointReportsMissedIncludingEmpty(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "SameICS", true: "EmptyResume"}[empty], func(t *testing.T) {
			cluster := testpg.Start(t)
			var mode atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", "max-age=3600")
				w.Header().Set("Date", "Sun, 01 Nov 2026 04:00:00 GMT")
				switch mode.Load() {
				case 1:
					_, _ = w.Write([]byte("invalid calendar"))
				case 2:
					if empty {
						_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n"))
						return
					}
					fallthrough
				default:
					_, _ = w.Write([]byte(nha101AstraPointICS))
				}
			}))
			defer server.Close()
			startup := nha029RunStartupFromRaw(t, nha101RunRaw(t, cluster.Port, shortPrivateDataDir(t), server.URL))
			h := nha101RunStart(t, startup, time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC), true, nil)
			nha101RunWait(t, func() bool { return nha101RunReady(h) })
			nha101AstraNoFired(t, h, false)
			pilot := nha101AstraRunPilot(t, h)
			h.clock.advance(5 * time.Minute)
			mode.Store(1)
			if err := pilot.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			nha101RunWait(t, func() bool { return !nha101RunReady(h) })
			nha101RunCalendar(t, h, false)
			h.clock.advance(15 * time.Minute)
			// This successful HTTP/SQL refresh begins only at04:20. Point04:10 is
			// absent from both new projections, including an explicitly empty body.
			mode.Store(2)
			if err := pilot.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			nha101RunWait(t, func() bool { return nha101RunReady(h) })
			nha101AstraNoFired(t, h, true)
			if err := pilot.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			nha101RunWait(t, func() bool { return nha101RunReady(h) })
			nha101AstraNoFired(t, h, false)
			h.stop(t)
		})
	}
}

func TestNHA101RunSecond200And304ReadyOnlyAfterReal040(t *testing.T) {
	for _, notModified := range []bool{false, true} {
		t.Run(map[bool]string{false: "Second200", true: "Second304"}[notModified], func(t *testing.T) {
			cluster := testpg.Start(t)
			var gets atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := gets.Add(1)
				w.Header().Set("ETag", `"real040-two"`)
				w.Header().Set("Cache-Control", "max-age=3600")
				w.Header().Set("Date", "Sun, 01 Nov 2026 04:00:00 GMT")
				if n == 2 && notModified {
					if r.Header.Get("If-None-Match") == "" {
						t.Error("real304 did not select validators")
					}
					w.WriteHeader(http.StatusNotModified)
					return
				}
				_, _ = w.Write([]byte(nha101AstraPointICS))
			}))
			defer server.Close()
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			var replacements atomic.Int32
			var published atomic.Int32
			startup := nha029RunStartupFromRaw(t, nha101RunRaw(t, cluster.Port, shortPrivateDataDir(t), server.URL))
			h := nha101RunStart(t, startup, time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC), true, func(r *remoteCalendarRuntime) {
				r.bridge.beforeReplace = func() {
					if replacements.Add(1) == 2 {
						close(entered)
						<-release
					}
				}
				r.bridge.afterReplace = func(err error) {
					if err == nil {
						published.Add(1)
					}
				}
			})
			nha101RunWait(t, func() bool { return nha101RunReady(h) && published.Load() == 1 })
			if err := nha101AstraRunPilot(t, h).Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(8 * time.Second):
				t.Fatal("second publication did not enter real040 barrier")
			}
			if nha101RunReady(h) || published.Load() != 1 {
				t.Fatal("second healthy response retained Ready before actual Replace040")
			}
			unblock()
			nha101RunWait(t, func() bool { return published.Load() == 2 && nha101RunReady(h) })
			h.stop(t)
		})
	}
}

// The SQL and Source oracles isolate admission reanchoring. This actual Run
// additionally proves that the carried original deadline closes a Point's
// two real040 borders while wall time still precedes freshUntil.
func TestNHA101RunOriginalDeadlineGuardsPointAfterSmallSkew(t *testing.T) {
	cluster := testpg.Start(t)
	at := time.Date(2026, 11, 1, 4, 0, 0, 200000000, time.UTC)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) > 1 {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Cache-Control", "max-age=10")
		w.Header().Set("Date", at.Add(time.Second).Format(http.TimeFormat))
		_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:point-deadline\r\nDTSTART:20261101T040010Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	}))
	defer server.Close()
	startup := nha029RunStartupFromRaw(t, nha101RunRaw(t, cluster.Port, shortPrivateDataDir(t), server.URL))
	var once sync.Once
	h := nha101RunStart(t, startup, at, true, func(r *remoteCalendarRuntime) {
		clock := r.clock.(*nha105RunClock)
		clock.mu.Lock()
		clock.tick = 100 * time.Second
		clock.mu.Unlock()
		r.bridge.beforeReplace = func() {
			once.Do(func() {
				clock.mu.Lock()
				clock.wall = clock.wall.Add(100 * time.Millisecond)
				clock.tick += 600 * time.Millisecond
				clock.mu.Unlock()
			})
		}
	})
	defer h.stop(t)
	nha101RunWait(t, func() bool { return nha101RunReady(h) })
	nha101RunCalendar(t, h, true)
	h.clock.advance(9700 * time.Millisecond)
	nha101RunWait(t, func() bool { return !nha101RunReady(h) })
	nha101RunCalendar(t, h, false)
	if h.clock.MonoNow() != 110*time.Second+300*time.Millisecond || !h.clock.Now().Before(at.Add(10*time.Second)) {
		t.Fatal("original deadline counterexample values lost")
	}
	nha101AstraNoFired(t, h, false)
	h.stop(t)
}
