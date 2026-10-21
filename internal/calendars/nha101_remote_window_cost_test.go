//go:build !race

package calendars

import (
	"testing"
)

func TestNHA101CostRemoteWindowSQLComposition(t *testing.T) {
	f, id, w, scope := nha101StoredSource(t)
	nha101StoredReplace(t, f, w, scope, "windowcost", nha101DenseBody(512))
	var observed remoteDecodeObservation
	f.repo.afterRemoteRead = func(o remoteDecodeObservation) { observed = o }
	defer func() { f.repo.afterRemoteRead = nil }()
	var win AutomationWindow
	var err error
	delta := nha101Measure(func() { win, err = f.service.CollectRemoteWindow(f.ctx, id, nha101Request(t)) })
	if err != nil || len(win.Items()) != 512 {
		t.Fatal(err)
	}
	t.Logf("collect TotalAlloc=%d credit=%d image=%d", delta, observed.Admission.ReservedBytes, observed.Image.ReservedBytes)
	if delta > observed.Admission.ReservedBytes || observed.Admission.ReservedBytes > parseReservationLimit || observed.Image.ReservedBytes > uint64(MaxImageBytes) {
		t.Fatal("collector exceeded continuous owned credit")
	}
	images.mu.Lock()
	active := images.active
	images.mu.Unlock()
	if active != 0 {
		t.Fatal("collector retained image")
	}
}

func TestNHA101CostRemoteWindowSQLCorpus(t *testing.T) {
	f, id, w, scope := nha101StoredSource(t)
	from, _ := ParseInstant("2026-01-01T00:00:00Z")
	to, _ := ParseInstant("2026-01-03T00:00:00Z")
	zone, _ := ParseZone("UTC")
	q, e := NewAutomationWindowRequest(from, to, zone)
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"20-eight-zones-conversion.ics", "21-override-content-conversion.ics", "22-sparse-observance-allocation.ics"} {
		t.Run(name, func(t *testing.T) {
			nha101StoredReplace(t, f, w, scope, "collect-"+name, costRaw(t, name))
			var observed remoteDecodeObservation
			f.repo.afterRemoteRead = func(o remoteDecodeObservation) { observed = o }
			defer func() { f.repo.afterRemoteRead = nil }()
			var win AutomationWindow
			var err error
			delta := nha101Measure(func() { win, err = f.service.CollectRemoteWindow(f.ctx, id, q) })
			t.Logf("SQL collector corpus TotalAlloc=%d reserved=%d image=%+v error=%v", delta, observed.Admission.ReservedBytes, observed.Image, err)
			if err != nil || !win.Valid() {
				t.Fatal("real collector refused cost corpus", err)
			}
			if delta > observed.Admission.ReservedBytes || observed.Admission.ReservedBytes > parseReservationLimit || observed.Image.ReservedBytes > uint64(MaxImageBytes) {
				t.Fatal("continuous reader credit exceeded")
			}
		})
	}
}
