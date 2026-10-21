package calendars

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/damione1/Homeward/internal/storage"
	"strings"
	"testing"
	"time"
)

type nha101WindowClock struct {
	wall time.Time
	mono time.Duration
}

func (c *nha101WindowClock) Now() time.Time         { return c.wall }
func (c *nha101WindowClock) MonoNow() time.Duration { return c.mono }
func nha101Request(t *testing.T) AutomationWindowRequest {
	t.Helper()
	from, _ := ParseInstant("2026-10-19T00:00:00Z")
	to, _ := ParseInstant("2026-10-21T00:00:00Z")
	z, _ := ParseZone("UTC")
	q, e := NewAutomationWindowRequest(from, to, z)
	if e != nil {
		t.Fatal(e)
	}
	return q
}
func TestNHA101RemoteWindowRequestAdmissions(t *testing.T) {
	from, _ := ParseInstant("2026-10-19T00:00:00Z")
	z, _ := ParseZone("UTC")
	exact, _ := NewInstant(from.value.Add(48 * time.Hour))
	over, _ := NewInstant(from.value.Add(48*time.Hour + time.Second))
	if _, e := NewAutomationWindowRequest(from, exact, z); e != nil {
		t.Fatal(e)
	}
	for _, q := range []struct {
		a, b Instant
		z    Zone
	}{{from, over, z}, {from, from, z}, {Instant{}, exact, z}, {from, exact, Zone{}}} {
		if _, e := NewAutomationWindowRequest(q.a, q.b, q.z); e == nil {
			t.Fatal("invalid request admitted")
		}
	}
	if (AutomationWindow{}).Valid() || (AutomationOccurrence{}).Valid() {
		t.Fatal("zero projection admitted")
	}
}
func nha101DenseBody(n int) []byte {
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "BEGIN:VEVENT\r\nUID: stable uid %03d \r\nDTSTART:20261019T120000Z\r\nEND:VEVENT\r\n", i)
	}
	b.WriteString("END:VCALENDAR\r\n")
	return []byte(b.String())
}
func TestNHA101RemoteWindowSQLSingleOwnerAndGlobalCapacity(t *testing.T) {
	f, id, w, scope := nha101StoredSource(t)
	for _, n := range []int{512, 513} {
		nha101StoredReplace(t, f, w, scope, fmt.Sprint(n), nha101DenseBody(n))
		reads := 0
		f.repo.afterRemoteRead = func(o remoteDecodeObservation) {
			reads++
			if o.Admission.ReservedBytes > parseReservationLimit || o.Image.ReservedBytes > uint64(MaxImageBytes) {
				t.Error("credit exceeded")
			}
		}
		defer func() { f.repo.afterRemoteRead = nil }()
		win, e := f.service.CollectRemoteWindow(f.ctx, id, nha101Request(t))
		if reads != 1 {
			t.Fatal("collector did not own exactly one decoded image", reads)
		}
		if n == 513 {
			if e == nil || win.Valid() {
				t.Fatal("513 silently truncated")
			}
			continue
		}
		if e != nil || len(win.Items()) != 512 {
			t.Fatal("512 refused", e)
		}
		for _, item := range win.Items() {
			a, b, k, ok := item.Instants()
			if !ok || !a.Equal(b) || k != AutomationPoint {
				t.Fatal("Point changed into interval")
			}
			uid, _ := item.ID().Event().UID().Value()
			if !strings.Contains(uid, " ") {
				t.Fatal("UID stripped")
			}
		}
	}
}
func TestNHA101RemoteWindowSQLRevisionFenceAndNoRetry(t *testing.T) {
	f, id, w, scope := nha101StoredSource(t)
	nha101StoredReplace(t, f, w, scope, "before", nha101DenseBody(256))
	reads := 0
	f.repo.afterRemoteRead = func(remoteDecodeObservation) { reads++ }
	defer func() { f.repo.afterRemoteRead = nil }()
	current, e := w.Read(f.ctx)
	if e != nil {
		t.Fatal(e)
	}
	f.repo.beforeRemoteWindowPage = func(page int) error {
		if page == 1 {
			o := nha101StoredObservation(t, scope, "betweenpages")
			_, e := w.Revalidate(f.ctx, current.Calendar().Revision(), o, current.Validators(), func() [32]byte { d, _ := current.Digest(); return d }())
			return e
		}
		return nil
	}
	defer func() { f.repo.beforeRemoteWindowPage = nil }()
	// Revalidate is scalar-only and can run while the read owner is held.
	win, e := f.service.CollectRemoteWindow(f.ctx, id, nha101Request(t))
	if e == nil || win.Valid() {
		t.Fatal("changed revision published")
	}
	var ce *Error
	if x, ok := e.(*Error); ok {
		ce = x
	}
	if ce == nil || ce.Code() != StaleCursor {
		t.Fatal("wrong revision refusal", e)
	}
	if reads != 2 {
		t.Fatal("unexpected image reads (collector + explicit writer.Read)", reads)
	}
}
func TestNHA101RemoteKnownVersionGuardAndClockExpiry(t *testing.T) {
	f, id, w, scope := nha101StoredSource(t)
	now := time.Now().UTC().Truncate(time.Second)
	clock := &nha101WindowClock{wall: now}
	f.repo.clock = clock
	nha101StoredReplace(t, f, w, scope, "first", nha101DenseBody(0))
	before, e := w.Read(f.ctx)
	if e != nil {
		t.Fatal(e)
	}
	f.repo.beforeRemoteFreshAdmission = func() {
		head, e := f.repo.GetCalendar(f.ctx, id)
		if e != nil {
			t.Error(e)
			return
		}
		s, _ := head.RemoteState()
		q, _ := s.Freshness()
		if q != RemoteStale {
			t.Error("new PG revision inherited old fresh admission")
		}
	}
	defer func() { f.repo.beforeRemoteFreshAdmission = nil }()
	validators, _ := storage.NewRemoteCalendarValidators(storage.RemoteValidatorsNone, "", time.Time{})
	o, e := storage.NewRemoteCalendarObservation(scope, sha256.Sum256([]byte("clock")), now, now.Add(time.Hour), validators)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = w.Replace(f.ctx, before.Calendar().Revision(), o, nha101DenseBody(0), nil); e != nil {
		t.Fatal(e)
	}
	clock.wall = now.Add(time.Hour)
	clock.mono = time.Hour
	head, e := f.repo.GetCalendar(context.Background(), id)
	if e != nil {
		t.Fatal(e)
	}
	s, _ := head.RemoteState()
	q, _ := s.Freshness()
	if q != RemoteStale {
		t.Fatal("expiry stayed fresh")
	}
	clock.wall = now.Add(time.Minute)
	clock.mono = time.Hour + time.Second
	head, e = f.repo.GetCalendar(f.ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	s, _ = head.RemoteState()
	q, _ = s.Freshness()
	if q != RemoteStale {
		t.Fatal("expired admission resurrected on civil rollback")
	}
}

func TestNHA101RemoteWindowSQLCheckAfterLastPage(t *testing.T) {
	f, id, w, scope := nha101StoredSource(t)
	nha101StoredReplace(t, f, w, scope, "beforecheck", nha101DenseBody(1))
	win, e := f.service.CollectRemoteWindow(f.ctx, id, nha101Request(t))
	if e != nil {
		t.Fatal(e)
	}
	if e = f.service.CheckRemoteWindow(f.ctx, win); e != nil {
		t.Fatal(e)
	}
	nha101StoredReplace(t, f, w, scope, "afterlastpage", nha101DenseBody(1))
	if e = f.service.CheckRemoteWindow(f.ctx, win); e == nil {
		t.Fatal("changed durable revision admitted after final page")
	}
}

func TestNHA101RemoteConfiguredEmbeddedZoneOwnership(t *testing.T) {
	f := nha100Setup(t)
	id := f.bind(t, "calendar.famille")
	original := nha101Admit(t, id, nha101Raw(t, "10-vtimezone-dst.ics"))
	source := original.zones["Custom/NY"]
	detached, e := newRemoteResultCopies(&calendarImage{remote: original, lease: original.lease}).zone(source)
	if e != nil {
		t.Fatal(e)
	}
	original.close()
	if detached.embedded == nil {
		t.Fatal("fixture missing embedded source")
	}
	beforeYear := detached.embedded.throughYear
	beforeTransitions := len(detached.embedded.transitions)
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:floating projection\r\nDTSTART:20261019T120000\r\nDURATION:PT1H\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	for cycle := 0; cycle < 2; cycle++ {
		image, e := admitRemoteICS(f.ctx, id, raw, &detached)
		if e != nil {
			t.Fatal(e)
		}
		if image.events[0].definition.span.zone.embedded == detached.embedded {
			image.close()
			t.Fatal("configured lazy index borrowed across cycles")
		}
		if detached.embedded.throughYear != beforeYear || len(detached.embedded.transitions) != beforeTransitions {
			image.close()
			t.Fatal("source index mutated")
		}
		view, e := detachRemoteProjection(image, &detached)
		if e != nil {
			image.close()
			t.Fatal(e)
		}
		date, _ := ParseDate("2026-10-19")
		end, _ := ParseDate("2026-10-20")
		span, _ := AllDaySpan(date, end)
		uid, _ := ParseUID("date")
		key, _ := EventIdentity(id, uid)
		identity, _ := OccurrenceIdentity(key, span.point())
		o := Occurrence{id: identity, span: span, summary: "date", contentKind: contentLocal}
		projected, e := projectAutomationOccurrence(image.ctx, o, *view, RemoteFresh)
		if e != nil || projected.kind != AutomationAllDay || projected.zoneKind != AutomationEmbedded || projected.dataID != detached.DataID() {
			image.close()
			t.Fatal("DATE lost embedded provenance", e)
		}
		image.close()
	}
	image, e := newRemoteImage(f.ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	defer image.close()
	if e = image.lease.grow(MaxImageBytes - imageFixedCharge); e != nil {
		t.Fatal(e)
	}
	if z, e := detachRemoteProjection(image, &detached); e == nil || z != nil {
		t.Fatal("capacity refusal copied configured zone")
	}
	if detached.embedded.throughYear != beforeYear || len(detached.embedded.transitions) != beforeTransitions {
		t.Fatal("refused detach mutated source")
	}
}

func TestNHA101RemoteWindowProjectionPointFoldDate(t *testing.T) {
	f, id, w, scope := nha101StoredSource(t)
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:fold points\r\nDTSTART:20261101T053000Z\r\nRDATE:20261101T063000Z\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:date\r\nDTSTART;VALUE=DATE:20261101\r\nDTEND;VALUE=DATE:20261102\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	nha101StoredReplace(t, f, w, scope, "fold-date-window", raw)
	a, _ := ParseInstant("2026-11-01T00:00:00Z")
	b, _ := ParseInstant("2026-11-03T00:00:00Z")
	zone, e := ParseZone("America/Toronto")
	if e != nil {
		t.Fatal(e)
	}
	q, e := NewAutomationWindowRequest(a, b, zone)
	if e != nil {
		t.Fatal(e)
	}
	window, e := f.service.CollectRemoteWindow(f.ctx, id, q)
	if e != nil {
		t.Fatal(e)
	}
	if len(window.Items()) != 3 {
		t.Fatal("fold/date projection lost occurrences")
	}
	points := make(map[string]bool)
	starts := make(map[time.Time]bool)
	dateSeen := false
	for _, o := range window.Items() {
		start, end, kind, ok := o.Instants()
		if !ok {
			t.Fatal("invalid projection")
		}
		switch kind {
		case AutomationPoint:
			if !start.Equal(end) {
				t.Fatal("Point gained duration")
			}
			points[o.ID().Point().String()] = true
			starts[start] = true
		case AutomationAllDay:
			dateSeen = true
			if end.Sub(start) != 25*time.Hour {
				t.Fatal("DATE used fixed24h through DST")
			}
		default:
			t.Fatal("unexpected projection kind")
		}
	}
	if len(points) != 2 || !starts[time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)] || !starts[time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC)] || !dateSeen {
		t.Fatal("fold identity or DATE lost")
	}
}
