package remotecalendar

import (
	"context"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNHA101SourceOriginalDeadlineSmallClockSkew(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		for _, late := range []bool{false, true} {
			name := map[bool]string{false: "Publication", true: "ReadOnly"}[disabled] + map[bool]string{false: "Renewal", true: "ExpiredAtAdmission"}[late]
			t.Run(name, func(t *testing.T) {
				f := nha101LiveSetup(t)
				f.disabled = disabled
				at := time.Date(2026, 10, 21, 12, 0, 0, 200000000, time.UTC)
				f.clock.wall = at
				f.clock.mono = 100 * time.Second
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if requests.Add(1) > 1 {
						<-r.Context().Done()
						return
					}
					w.Header().Set("Date", at.Add(time.Second).Format(http.TimeFormat))
					w.Header().Set("Cache-Control", "max-age=10")
					_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n"))
				}))
				defer server.Close()
				f.phase.beforeMutation = func() {
					if late {
						f.clock.advance(9800*time.Millisecond, 10300*time.Millisecond)
					} else {
						f.clock.advance(100*time.Millisecond, 600*time.Millisecond)
					}
				}
				pub := &nha101RecordingPublisher{}
				pilot, supervisor, service, fixture := f.start(t, server.URL, pub, nil)
				defer func() {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if err := supervisor.Stop(ctx); err != nil {
						t.Error(err)
					}
				}()
				ready := func() bool {
					health, ok := fixture.Health(f.instance)
					if !ok {
						return false
					}
					status, _, err := health.Components()
					return err == nil && status == integrations.HealthReady
				}
				stale := func() bool {
					head, err := service.GetCalendar(f.ctx, pilot.source.id)
					if err != nil {
						return false
					}
					state, _ := head.RemoteState()
					quality, known := state.Freshness()
					return known && quality == calendars.RemoteStale
				}
				if late {
					nha101Eventually(t, func() bool {
						pilot.mu.Lock()
						run := pilot.run
						pilot.mu.Unlock()
						if run == nil {
							return false
						}
						run.mu.Lock()
						defer run.mu.Unlock()
						return run.lastFailure != nil && run.lastFailure.Kind() == Expired
					})
					if ready() || pub.count() != 0 || !stale() {
						t.Fatal("original mono110s expired before admission but reached Ready/publication/fresh reader")
					}
				} else {
					nha101Idle(t, pilot)
					nha101Eventually(t, ready)
					// Exercise an actual local collection; it may replace the projection,
					// but must keep the HTTP observation's original deadline.
					if !disabled {
						if err := pilot.run.collect(f.ctx, false); err != nil {
							t.Fatal(err)
						}
						pub.mu.Lock()
						publication := pub.publications[len(pub.publications)-1]
						pub.mu.Unlock()
						_, _, _, untilMono, ok := publication.Deadline()
						if !ok || untilMono != 110*time.Second {
							t.Fatal("local publication renewed the original mono110s deadline")
						}
					}
					f.clock.advance(9700*time.Millisecond, 9700*time.Millisecond)
					nha101Eventually(t, func() bool { return stale() && !ready() })
					if !f.clock.Now().Before(at.Add(10*time.Second)) || f.clock.MonoNow() != 110*time.Second+300*time.Millisecond {
						t.Fatal("discriminating clock values lost")
					}
				}
			})
		}
	}
}

func TestNHA101PublicationPendingTwoSuccessesAndOldReceipt(t *testing.T) {
	for _, notModified := range []bool{false, true} {
		t.Run(map[bool]string{false: "Second200", true: "Second304"}[notModified], func(t *testing.T) {
			f := nha101LiveSetup(t)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := requests.Add(1)
				w.Header().Set("ETag", `"two-successes"`)
				w.Header().Set("Date", f.clock.Now().Add(time.Second).Format(http.TimeFormat))
				w.Header().Set("Cache-Control", "max-age=3600")
				if notModified && n > 1 {
					if r.Header.Get("If-None-Match") == "" {
						t.Error("304 lacks actual selected validators")
					}
					w.WriteHeader(http.StatusNotModified)
					return
				}
				_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n"))
			}))
			defer server.Close()
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			pub := &nha101RecordingPublisher{}
			pilot, _, _, fixture := f.start(t, server.URL, pub, nil)
			nha101Idle(t, pilot)
			ready := func() bool {
				h, ok := fixture.Health(f.instance)
				if !ok {
					return false
				}
				s, _, e := h.Components()
				return e == nil && s == integrations.HealthReady
			}
			nha101Eventually(t, ready)
			var previous knownCommit
			// The first receipt is already consumed. Keep it while the second one
			// is private/current, so stale confirmation must not affect that epoch.
			pub.mu.Lock()
			pub.afterReplace = func() { previous = *pilot.run.commit; close(entered); <-release }
			pub.mu.Unlock()
			if err := pilot.Refresh(f.ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(4 * time.Second):
				t.Fatal("second real SQL publication did not reach barrier")
			}
			if ready() {
				t.Fatal("second healthy response retained Ready during publication")
			}
			// An absent/old receipt cannot become a consumer admission.
			if err := pilot.run.http.ConfirmConditionalAdmitted(context.Background(), previous.observation, storage.RemoteCalendarCommit{}); err == nil {
				t.Fatal("invalid receipt confirmed pending publication")
			}
			if ready() {
				t.Fatal("rejected receipt restored Ready before publication")
			}
			unblock()
			nha101Idle(t, pilot)
			nha101Eventually(t, ready)
			if err := pilot.run.http.ConfirmConditionalAdmitted(f.ctx, previous.observation, previous.receipt); err == nil {
				t.Fatal("consumed old receipt reconfirmed")
			}
			third, releaseThird := make(chan struct{}), make(chan struct{})
			var thirdOnce sync.Once
			unblockThird := func() { thirdOnce.Do(func() { close(releaseThird) }) }
			defer unblockThird()
			pub.mu.Lock()
			pub.afterReplace = func() { close(third); <-releaseThird }
			pub.mu.Unlock()
			if err := pilot.Refresh(f.ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case <-third:
			case <-time.After(4 * time.Second):
				t.Fatal("third pending observation absent")
			}
			if err := pilot.run.http.ConfirmConditionalAdmitted(f.ctx, previous.observation, previous.receipt); err == nil {
				t.Fatal("old second receipt confirmed third pending observation")
			}
			if ready() {
				t.Fatal("old receipt reopened Ready while third publication pending")
			}
			unblockThird()
			nha101Idle(t, pilot)
			nha101Eventually(t, ready)
			pub.mu.Lock()
			pub.afterReplace = nil
			pub.reject = true
			pub.mu.Unlock()
			if err := pilot.Refresh(f.ctx); err != nil {
				t.Fatal(err)
			}
			nha101Eventually(t, func() bool {
				pilot.run.mu.Lock()
				defer pilot.run.mu.Unlock()
				return pilot.run.lastFailure != nil && !pilot.run.fresh
			})
			if ready() {
				t.Fatal("publication failure restored Ready")
			}
		})
	}
}
