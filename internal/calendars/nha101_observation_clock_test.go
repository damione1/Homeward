package calendars

import (
	"context"
	"crypto/sha256"
	"github.com/damione1/Homeward/internal/storage"
	"testing"
	"time"
)

// The SQL postcommit barrier advances elapsed time600ms but wall only100ms.
// A later collection must not renew the original mono110s deadline.
func TestNHA101WriterOriginalObservationDeadlineAcrossSQL(t *testing.T) {
	for _, revalidate := range []bool{false, true} {
		t.Run(map[bool]string{false: "Replace200", true: "Revalidate304"}[revalidate], func(t *testing.T) {
			f, id, writer, scope := nha101StoredSource(t)
			at := time.Date(2026, 10, 21, 12, 0, 0, 200000000, time.UTC)
			clock := &nha101WindowClock{wall: at, mono: 100 * time.Second}
			f.repo.clock = clock
			validators, _ := storage.NewRemoteCalendarValidators(storage.RemoteValidatorsETag, `"original-observation-clock"`, time.Time{})
			snapshot, err := writer.Read(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if revalidate {
				seed, err := storage.NewRemoteCalendarObservation(scope, sha256.Sum256([]byte("seed-original-clock")), at, at.Add(time.Hour), validators)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := writer.Replace(f.ctx, snapshot.Calendar().Revision(), seed, nha101DenseBody(0), nil); err != nil {
					t.Fatal(err)
				}
				snapshot, err = writer.Read(f.ctx)
				if err != nil {
					t.Fatal(err)
				}
			}
			observed, err := storage.NewRemoteCalendarObservation(scope, sha256.Sum256([]byte("original-observation-clock")), at, at.Add(10*time.Second), validators)
			if err != nil {
				t.Fatal(err)
			}
			ctx, err := WithRemoteObservationClock(f.ctx, observed, at, at.Add(10*time.Second), 100*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			f.repo.beforeRemoteFreshAdmission = func() {
				clock.wall = at.Add(100 * time.Millisecond)
				clock.mono = 100*time.Second + 600*time.Millisecond
			}
			defer func() { f.repo.beforeRemoteFreshAdmission = nil }()
			if revalidate {
				digest, _ := snapshot.Digest()
				_, err = writer.Revalidate(ctx, snapshot.Calendar().Revision(), observed, validators, digest)
			} else {
				_, err = writer.Replace(ctx, snapshot.Calendar().Revision(), observed, nha101DenseBody(0), nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			current, err := f.service.GetCalendar(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			state, _ := current.RemoteState()
			quality, _ := state.Freshness()
			if quality != RemoteFresh {
				t.Fatal("live original deadline refused")
			}
			clock.wall = at.Add(9*time.Second + 400*time.Millisecond)
			clock.mono = 109*time.Second + 900*time.Millisecond
			from, _ := NewInstant(clock.wall.Truncate(time.Second))
			to, _ := NewInstant(clock.wall.Truncate(time.Second).Add(MaxAutomationHorizon))
			zone, _ := ParseZone("UTC")
			window, err := NewAutomationWindowRequest(from, to, zone)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.CollectRemoteWindow(f.ctx, id, window); err != nil {
				t.Fatal("local renewal", err)
			}
			clock.wall = at.Add(9*time.Second + 800*time.Millisecond)
			clock.mono = 110*time.Second + 300*time.Millisecond
			current, err = f.service.GetCalendar(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			state, _ = current.RemoteState()
			quality, _ = state.Freshness()
			if quality != RemoteStale {
				t.Fatal("post-SQL/read renewal extended original mono110s despite wall<freshUntil")
			}
		})
	}
}
