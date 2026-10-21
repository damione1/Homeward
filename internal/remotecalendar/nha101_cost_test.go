//go:build !race

package remotecalendar

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

func TestNHA101CostPilotPhaseOwnership(t *testing.T) {
	// Scalar cadence must never turn its 1s guard into SQL/decode/HTTP work.
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	var c cadence
	c.pollSucceeded(now, 0, now.Add(24*time.Hour))
	c.windowSucceeded(now, 0)
	for i := 1; i < 3600; i++ {
		if c.due(now.Add(time.Duration(i)*time.Second), time.Duration(i)*time.Second, true) != intentNone {
			t.Fatal("scalar tick scheduled work")
		}
	}
	// Also drive the real writer then SQL collector, with no measurement ledger.
	f := nha101LiveSetup(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n"))
	}))
	defer server.Close()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	pub := &nha101RecordingPublisher{}
	_, _, _, _ = f.start(t, server.URL, pub, nil)
	nha101Eventually(t, func() bool { return pub.count() == 1 })
	runtime.ReadMemStats(&after)
	if f.phase.overlap.Load() || f.phase.mutations.Load() != 1 || f.phase.reads.Load() != 1 {
		t.Fatal("writer/collector phase ownership overlapped")
	}
	t.Logf("composition HeapAlloc before=%d after=%d TotalAlloc delta=%d; per-phase owned credit asserted inside calendars", before.HeapAlloc, after.HeapAlloc, after.TotalAlloc-before.TotalAlloc)
}
