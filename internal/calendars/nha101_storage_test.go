package calendars

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
)

func nha101StoredSource(t *testing.T) (nha100Fixture, CalendarID, RemoteWriter, storage.RemoteCalendarScope) {
	t.Helper()
	f := nha100Setup(t)
	id := f.bind(t, "calendar.famille")
	scope, e := storage.NewRemoteCalendarScope(sha256.Sum256([]byte("synthetic configured ICS resource")), nil)
	if e != nil {
		t.Fatal(e)
	}
	source, e := NewRemoteSource(scope, "Synthetic family")
	if e != nil {
		t.Fatal(e)
	}
	writer, e := f.service.ConfigureRemoteSource(f.ctx, id, source)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(writer.Retire)
	return f, id, writer, scope
}

func TestNHA101StorageSameScopeResumeNoNotification(t *testing.T) {
	f, id, writer, scope := nha101StoredSource(t)
	nha101StoredReplace(t, f, writer, scope, "resume200", nha101Raw(t, "02-utc.ics"))
	before, err := writer.Read(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	writer.Retire()
	sub, err := f.service.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	source, err := NewRemoteSource(scope, "Synthetic family")
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := f.service.ConfigureRemoteSource(f.ctx, id, source)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Retire()
	after, err := resumed.Read(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !before.Validators().Equal(after.Validators()) || before.Calendar().Revision() != after.Calendar().Revision() || len(sub.queue) != 0 {
		t.Fatal("same-scope resume wrote a new revision, erased validators or notified")
	}
	state, ok := after.Calendar().RemoteState()
	freshness, present := state.Freshness()
	if !ok || !present || freshness != RemoteStale {
		t.Fatal("resumed process became fresh without real revalidation")
	}
}
func nha101StoredObservation(t *testing.T, scope storage.RemoteCalendarScope, seed string) storage.RemoteCalendarObservation {
	t.Helper()
	validators, e := storage.NewRemoteCalendarValidators(storage.RemoteValidatorsETag, `W/"synthetic-calendar"`, time.Time{})
	if e != nil {
		t.Fatal(e)
	}
	at := time.Now().UTC().Truncate(time.Second)
	o, e := storage.NewRemoteCalendarObservation(scope, sha256.Sum256([]byte(seed)), at, at.Add(time.Hour), validators)
	if e != nil {
		t.Fatal(e)
	}
	return o
}
func nha101StoredReplace(t *testing.T, f nha100Fixture, w RemoteWriter, scope storage.RemoteCalendarScope, seed string, body []byte) storage.RemoteCalendarCommit {
	t.Helper()
	current, e := w.Read(f.ctx)
	if e != nil {
		t.Fatal(e)
	}
	receipt, e := w.Replace(f.ctx, current.Calendar().Revision(), nha101StoredObservation(t, scope, seed), body, nil)
	if e != nil || !receipt.Valid() {
		t.Fatalf("stored Replace: %v", e)
	}
	return receipt
}
func nha101StoredWindow(t *testing.T, from, to string) Window {
	t.Helper()
	a, e := ParseInstant(from)
	if e != nil {
		t.Fatal(e)
	}
	b, e := ParseInstant(to)
	if e != nil {
		t.Fatal(e)
	}
	z, e := ParseZone("UTC")
	if e != nil {
		t.Fatal(e)
	}
	w, e := TimeWindow(a, b, z)
	if e != nil {
		t.Fatal(e)
	}
	return w
}
func nha101StoredState(t *testing.T, page Page, kind RemoteStateKind, freshness RemoteFreshness) {
	t.Helper()
	state, ok := page.RemoteState()
	if !ok || state.Kind() != kind {
		t.Fatal("page lost explicit remote state")
	}
	got, present := state.Freshness()
	if kind == RemoteNeverValidated {
		if present {
			t.Fatal("never validated reported a cache quality")
		}
	} else if !present || got != freshness {
		t.Fatalf("remote freshness got=%d want=%d", got, freshness)
	}
}
func TestNHA101StorageNeverValidatedAndReadOnlyLifecycle(t *testing.T) {
	f, id, w, scope := nha101StoredSource(t)
	window := nha100Window(t, "2026-10-01", "2026-10-10")
	empty, e := f.service.Query(f.ctx, id, window, 1, Cursor{})
	if e != nil {
		t.Fatal(e)
	}
	nha101StoredState(t, empty, RemoteNeverValidated, RemoteFreshnessInvalid)
	localEvent := nha100Event(t, nha100Day(t, "2026-10-01", "2026-10-02"), "", nil)
	checkReadOnly := func() {
		t.Helper()
		calendar, e := f.service.GetCalendar(f.ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		key := nha100Key(t, id, "daily")
		event, e := f.service.GetEvent(f.ctx, key)
		if e != nil {
			t.Fatal(e)
		}
		_, e = f.service.CreateCalendar(f.ctx, id, calendar.Revision(), "Mutation refused")
		nha100Code(t, e, ReadOnly)
		_, e = f.service.UpdateCalendar(f.ctx, id, calendar.Revision(), "Mutation refused")
		nha100Code(t, e, ReadOnly)
		_, e = f.service.DeleteCalendar(f.ctx, id, calendar.Revision())
		nha100Code(t, e, ReadOnly)
		_, e = f.service.CreateEvent(f.ctx, key, event.Revision(), localEvent)
		nha100Code(t, e, ReadOnly)
		_, e = f.service.UpdateEvent(f.ctx, key, event.Revision(), localEvent)
		nha100Code(t, e, ReadOnly)
		_, e = f.service.DeleteEvent(f.ctx, key, event.Revision())
		nha100Code(t, e, ReadOnly)
	}
	checkReadOnly()
	nha101StoredReplace(t, f, w, scope, "overrides200", nha101Raw(t, "07-overrides.ics"))
	checkReadOnly()
	var localRows int
	if e = f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
		entity, _ := id.entity.Value()
		return tx.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM public.local_calendars WHERE entity_id=$1)+(SELECT count(*) FROM public.calendar_events WHERE calendar_id=$1)`, entity).Scan(&localRows)
	}); e != nil || localRows != 0 {
		t.Fatal("remote import reached local CRUD storage", e)
	}
	w.Retire()
	_, e = w.Read(f.ctx)
	nha100Code(t, e, Closed)
	page, e := f.service.Query(f.ctx, id, window, 1000, Cursor{})
	if e != nil {
		t.Fatal(e)
	}
	nha101StoredState(t, page, RemoteValidated, RemoteStale)
}
func TestNHA101StorageReplaceRevalidateDegradeReopen(t *testing.T) {
	f, id, w, scope := nha101StoredSource(t)
	body := nha101Raw(t, "07-overrides.ics")
	nha101StoredReplace(t, f, w, scope, "first200", body)
	window := nha100Window(t, "2026-10-01", "2026-10-10")
	page, e := f.service.Query(f.ctx, id, window, 1, Cursor{})
	if e != nil {
		t.Fatal(e)
	}
	nha101StoredState(t, page, RemoteValidated, RemoteFresh)
	oldCursor, more := page.Next()
	if !more {
		t.Fatal("expected paginated committed snapshot")
	}
	full, e := f.service.Query(f.ctx, id, window, 1000, Cursor{})
	if e != nil || len(full.Items()) != 4 {
		t.Fatalf("UID/override occurrences got=%d err=%v", len(full.Items()), e)
	}
	var moved *Occurrence
	for _, item := range full.Items() {
		if item.ID().Point().String() == "2026-10-04" {
			value := item
			moved = &value
		}
	}
	if moved == nil {
		t.Fatal("moved exception lost its original identity")
	}
	content, ok := moved.Content()
	if !ok {
		t.Fatal("override content absent")
	}
	summary, _ := content.Summary().Value()
	location, present := content.Location().Value()
	if summary != "" || content.Summary().Present() || !present || location != "" {
		t.Fatal("replace override inherited absent SUMMARY or lost present-empty LOCATION")
	}
	before, e := w.Read(f.ctx)
	if e != nil {
		t.Fatal(e)
	}
	digest, ok := before.Digest()
	if !ok {
		t.Fatal("valid cache lacks digest")
	}
	receipt, e := w.Revalidate(f.ctx, before.Calendar().Revision(), nha101StoredObservation(t, scope, "304"), before.Validators(), digest)
	if e != nil || !receipt.Valid() {
		t.Fatalf("stored revalidate: %v", e)
	}
	_, e = f.service.Query(f.ctx, id, window, 1, oldCursor)
	nha100Code(t, e, StaleCursor)
	current, e := w.Read(f.ctx)
	if e != nil {
		t.Fatal(e)
	}
	if currentDigest, _ := current.Digest(); currentDigest != digest {
		t.Fatal("304 changed business snapshot")
	}
	if e = w.Degrade(f.ctx, current.Calendar().Revision(), time.Now(), RemoteReasonTransport); e != nil {
		t.Fatal(e)
	}
	stale, e := f.service.Query(f.ctx, id, window, 1000, Cursor{})
	if e != nil {
		t.Fatal(e)
	}
	nha101StoredState(t, stale, RemoteValidated, RemoteStale)
	if !reflect.DeepEqual(full.Items(), stale.Items()) {
		t.Fatal("degradation changed known occurrences")
	}
	reopened, e := storage.Open(f.ctx, f.settings)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	repo, e := NewRepository(reopened, f.manifest)
	if e != nil {
		t.Fatal(e)
	}
	service, e := NewService(repo)
	if e != nil {
		t.Fatal(e)
	}
	defer service.Close()
	rebound, e := service.Bind(f.ctx, id.entity)
	if e != nil || rebound != id {
		t.Fatal("reopen changed real CalendarID", e)
	}
	restored, e := service.Query(f.ctx, id, window, 1000, Cursor{})
	if e != nil {
		t.Fatal(e)
	}
	nha101StoredState(t, restored, RemoteValidated, RemoteStale)
	if !reflect.DeepEqual(stale.Items(), restored.Items()) {
		t.Fatal("reopen changed known UID/exception/content")
	}
	definition, e := service.GetEvent(f.ctx, nha100Key(t, id, "daily"))
	if e != nil || !definition.Valid() {
		t.Fatal("remote GetEvent failed after reopen", e)
	}
	definitions, e := service.ListEvents(f.ctx, id)
	if e != nil || len(definitions) != 1 {
		t.Fatal("remote ListEvents duplicated UID", e)
	}
}
func TestNHA101StorageSnapshotReplacementPaginationAndFold(t *testing.T) {
	f, id, w, scope := nha101StoredSource(t)
	var text strings.Builder
	text.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\n")
	for i := 0; i < 4; i++ {
		fmt.Fprintf(&text, "BEGIN:VEVENT\r\nUID:daily-%d\r\nDTSTART;VALUE=DATE:20260101\r\nDTEND;VALUE=DATE:20260102\r\nRRULE:FREQ=DAILY;COUNT=300\r\nEND:VEVENT\r\n", i)
	}
	text.WriteString("END:VCALENDAR\r\n")
	nha101StoredReplace(t, f, w, scope, "1200", []byte(text.String()))
	window := nha100Window(t, "2026-01-01", "2027-01-01")
	first, e := f.service.Query(f.ctx, id, window, 1000, Cursor{})
	if e != nil || len(first.Items()) != 1000 {
		t.Fatalf("first real page count=%d err=%v", len(first.Items()), e)
	}
	cursor, more := first.Next()
	if !more {
		t.Fatal("1000 became total query limit")
	}
	second, e := f.service.Query(f.ctx, id, window, 1000, cursor)
	if e != nil || len(second.Items()) != 200 {
		t.Fatalf("second page count=%d err=%v", len(second.Items()), e)
	}
	seen := make(map[RecurrenceID]bool)
	for _, page := range []Page{first, second} {
		for _, item := range page.Items() {
			if seen[item.ID()] {
				t.Fatal("duplicate occurrence across pages")
			}
			seen[item.ID()] = true
		}
	}
	const fold = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:fold\r\nDTSTART;TZID=America/New_York:20261101T013000\r\nDURATION:PT30M\r\nRDATE:20261101T063000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	nha101StoredReplace(t, f, w, scope, "fold200", []byte(fold))
	_, e = f.service.Query(f.ctx, id, window, 1000, cursor)
	nha100Code(t, e, StaleCursor)
	absent, e := f.service.GetEvent(f.ctx, nha100Key(t, id, "daily-0"))
	if e != nil || absent.Revision().Presence() != Absent {
		t.Fatal("removed UID remained in current snapshot", e)
	}
	timed := nha101StoredWindow(t, "2026-11-01T00:00:00Z", "2026-11-02T00:00:00Z")
	a, e := f.service.Query(f.ctx, id, timed, 1, Cursor{})
	if e != nil {
		t.Fatal(e)
	}
	next, more := a.Next()
	if !more {
		t.Fatal("folds collapsed at page boundary")
	}
	b, e := f.service.Query(f.ctx, id, timed, 1, next)
	if e != nil {
		t.Fatal(e)
	}
	av, bv := a.Items(), b.Items()
	if len(av) != 1 || len(bv) != 1 || av[0].ID() == bv[0].ID() {
		t.Fatal("fold origins merged")
	}
	start, _, _, _ := av[0].Span().Instants()
	later, _, _, _ := bv[0].Span().Instants()
	if start.String() != "2026-11-01T05:30:00Z" || later.String() != "2026-11-01T06:30:00Z" {
		t.Fatal("fold codec/SQL/page changed instants")
	}
}
func TestNHA101StorageEmptyValidatedAndInvalidPreserves(t *testing.T) {
	f, id, w, scope := nha101StoredSource(t)
	window := nha100Window(t, "2026-01-01", "2026-01-03")
	nha101StoredReplace(t, f, w, scope, "empty", []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n"))
	before, e := w.Read(f.ctx)
	if e != nil {
		t.Fatal(e)
	}
	receipt, e := w.Replace(f.ctx, before.Calendar().Revision(), nha101StoredObservation(t, scope, "invalid"), []byte("truncated invalid ICS"), nil)
	if e == nil || receipt.Valid() {
		t.Fatal("invalid document acknowledged")
	}
	if e = w.Degrade(f.ctx, before.Calendar().Revision(), time.Now(), RemoteReasonContent); e != nil {
		t.Fatal(e)
	}
	page, e := f.service.Query(f.ctx, id, window, 1, Cursor{})
	if e != nil || len(page.Items()) != 0 {
		t.Fatal("validated empty cache became an event or an error", e)
	}
	nha101StoredState(t, page, RemoteValidated, RemoteStale)
	after, e := w.Read(f.ctx)
	if e != nil {
		t.Fatal(e)
	}
	a, _ := after.Digest()
	b, _ := before.Digest()
	if a != b || !after.Validators().Equal(before.Validators()) {
		t.Fatal("invalid representation promoted headers or removed valid empty cache")
	}
}
func TestNHA101StoragePreflightAndCloseOwnership(t *testing.T) {
	f, id, w, scope := nha101StoredSource(t)
	nha101StoredReplace(t, f, w, scope, "UTC", nha101Raw(t, "02-utc.ics"))
	window := nha101StoredWindow(t, "2026-01-01T00:00:00Z", "2027-01-01T00:00:00Z")
	entity, _ := id.entity.Value()
	var loads atomic.Int32
	f.repo.beforeImageLoad = func() { loads.Add(1) }
	nha100SQL(t, f, `UPDATE public.remote_calendar_cache SET image_charge=2097152 WHERE entity_id=$1`, entity)
	_, e := f.service.Query(f.ctx, id, window, 1, Cursor{})
	nha100Code(t, e, Corrupt)
	if loads.Load() != 0 {
		t.Fatal("bytea scheduling point entered before false-small-charge preflight refusal")
	}
	// Restore an honest scalar by a real replacement through the writer.
	// Read() intentionally refuses corrupt payload headers, so obtain the
	// current revision using metadata only after restoring the SQL scalar.
	nha100SQL(t, f, `UPDATE public.remote_calendar_cache SET image_charge=33554432 WHERE entity_id=$1`, entity)
	current, e := f.service.GetCalendar(f.ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	_, e = w.Replace(f.ctx, current.Revision(), nha101StoredObservation(t, scope, "restored"), nha101Raw(t, "02-utc.ics"), nil)
	if e != nil {
		t.Fatal(e)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	f.repo.beforeImageLoad = func() { close(entered); <-release }
	queryDone := make(chan error, 1)
	go func() { _, err := f.service.Query(f.ctx, id, window, 1, Cursor{}); queryDone <- err }()
	select {
	case <-entered:
	case <-f.ctx.Done():
		t.Fatal("read did not reach reserved pre-scan barrier")
	}
	parses.Lock()
	parseOwned := parses.active
	parses.Unlock()
	images.mu.Lock()
	active := images.active
	images.mu.Unlock()
	if !parseOwned || active != 1 {
		t.Fatal("scope/slot not owned before bytea scan")
	}
	closed := make(chan struct{})
	go func() { f.service.Close(); close(closed) }()
	for {
		f.service.mu.Lock()
		closing := f.service.closed
		f.service.mu.Unlock()
		if closing {
			break
		}
		select {
		case <-f.ctx.Done():
			t.Fatal("Close did not close admissions")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case <-closed:
		t.Fatal("Close escaped an operation still holding its owner")
	default:
	}
	unblock()
	queryError := <-queryDone
	nha100Code(t, queryError, Canceled)
	<-closed
	parses.Lock()
	parseOwned = parses.active
	parses.Unlock()
	images.mu.Lock()
	active = images.active
	used := images.used
	images.mu.Unlock()
	if parseOwned || active != 0 || used != 0 {
		t.Fatal("Close leaked remote read owners")
	}
}

func TestNHA101StorageReconfigurationFencesOldWriter(t *testing.T) {
	f, id, old, scope := nha101StoredSource(t)
	nha101StoredReplace(t, f, old, scope, "old source", nha101Raw(t, "02-utc.ics"))
	previous, e := old.Read(f.ctx)
	if e != nil {
		t.Fatal(e)
	}
	digest, _ := previous.Digest()
	nextScope, e := storage.NewRemoteCalendarScope(sha256.Sum256([]byte("different configured source")), nil)
	if e != nil {
		t.Fatal(e)
	}
	nextSource, e := NewRemoteSource(nextScope, "Replacement source")
	if e != nil {
		t.Fatal(e)
	}
	next, e := f.service.ConfigureRemoteSource(f.ctx, id, nextSource)
	if e != nil {
		t.Fatal(e)
	}
	defer next.Retire()
	receipt, e := old.Replace(f.ctx, previous.Calendar().Revision(), nha101StoredObservation(t, scope, "late old response"), nha101Raw(t, "01-date.ics"), nil)
	nha100Code(t, e, Closed)
	if receipt.Valid() {
		t.Fatal("retired source returned committed success")
	}
	current, e := next.Read(f.ctx)
	if e != nil {
		t.Fatal(e)
	}
	got, _ := current.Digest()
	state, ok := current.Calendar().RemoteState()
	quality, present := state.Freshness()
	if got != digest || !ok || !present || quality != RemoteStale || current.Validators().Kind() != storage.RemoteValidatorsNone {
		t.Fatal("source replacement lost known image or reused prior freshness/validators")
	}
	receipt, e = next.Revalidate(f.ctx, current.Calendar().Revision(), nha101StoredObservation(t, nextScope, "unmatched304"), previous.Validators(), digest)
	nha100Code(t, e, Conflict)
	if receipt.Valid() {
		t.Fatal("new source revalidated previous source representation")
	}
}

func TestNHA101StorageReturnedEmbeddedZonesHaveNoOperationIndex(t *testing.T) {
	f, id, w, scope := nha101StoredSource(t)
	nha101StoredReplace(t, f, w, scope, "embedded200", nha101Raw(t, "10-vtimezone-dst.ics"))
	page, e := f.service.Query(f.ctx, id, nha101StoredWindow(t, "2026-01-01T00:00:00Z", "2027-01-01T00:00:00Z"), 1000, Cursor{})
	if e != nil || len(page.Items()) != 5 {
		t.Fatalf("embedded SQL occurrences=%d error=%v", len(page.Items()), e)
	}
	zones := make(map[*embeddedZone]bool)
	for _, item := range page.Items() {
		z := item.span.zone.embedded
		if z == nil || z.throughYear != 0 || len(z.transitions) != 0 || z.sourceBytes == 0 {
			t.Fatal("returned page retained operational timezone index or lost source")
		}
		zones[z] = true
	}
	if len(zones) != 1 {
		t.Fatal("page copied one embedded source per event")
	}
	definitions, e := f.service.ListEvents(f.ctx, id)
	if e != nil || len(definitions) != 3 {
		t.Fatal("remote definitions unavailable", e)
	}
	for _, definition := range definitions {
		event, ok := definition.Event()
		if !ok || event.span.zone.embedded == nil || event.span.zone.embedded.throughYear != 0 {
			t.Fatal("Get/List definition retained released lazy index")
		}
	}
	images.mu.Lock()
	active, bytes := images.active, images.used
	images.mu.Unlock()
	parses.Lock()
	parserActive := parses.active
	parses.Unlock()
	if active != 0 || bytes != 0 || parserActive {
		t.Fatal("caller-owned result retained operation leases")
	}
}
func TestNHA101StorageCommitUnknownNoNotificationAndFailedStaleGuard(t *testing.T) {
	f, id, w, scope := nha101StoredSource(t)
	nha101StoredReplace(t, f, w, scope, "initial", nha101Raw(t, "02-utc.ics"))
	before, e := w.Read(f.ctx)
	if e != nil {
		t.Fatal(e)
	}
	sub, e := f.service.Subscribe()
	if e != nil {
		t.Fatal(e)
	}
	defer sub.Close()
	nha100SQL(t, f, `CREATE SEQUENCE public.nha101_commit_attempts; CREATE FUNCTION public.nha101_deferred_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM nextval('public.nha101_commit_attempts'); RAISE EXCEPTION 'synthetic deferred failure'; END $$;
        CREATE CONSTRAINT TRIGGER nha101_deferred_failure AFTER UPDATE ON public.remote_calendar_cache DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha101_deferred_failure()`)
	receipt, e := w.Replace(f.ctx, before.Calendar().Revision(), nha101StoredObservation(t, scope, "unknown"), nha101Raw(t, "02-utc.ics"), nil)
	nha100Code(t, e, CommitUnknown)
	if receipt.Valid() || len(sub.queue) != 0 {
		t.Fatal("unknown commit acknowledged or notified")
	}
	if e = w.Degrade(f.ctx, before.Calendar().Revision(), time.Now(), RemoteReasonTransport); e == nil {
		t.Fatal("deferred failure unexpectedly allowed stale write")
	}
	page, e := f.service.Query(f.ctx, id, nha101StoredWindow(t, "2026-01-01T00:00:00Z", "2027-01-01T00:00:00Z"), 1000, Cursor{})
	if e != nil {
		t.Fatal(e)
	}
	nha101StoredState(t, page, RemoteValidated, RemoteStale)
	var attempts int64
	if e = f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(f.ctx, `SELECT last_value FROM public.nha101_commit_attempts`).Scan(&attempts)
	}); e != nil || attempts != 2 {
		t.Fatal("unknown mutation replayed or stale attempt missing", e)
	}
}
