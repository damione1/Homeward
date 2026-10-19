package calendars

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

type nha100Fixture struct {
	ctx      context.Context
	store    *storage.Store
	settings storage.Settings
	manifest registry.Manifest
	registry *registry.Repository
	repo     *Repository
	service  *Service
}

func nha100Setup(t *testing.T) nha100Fixture {
	t.Helper()
	cluster := testpg.Start(t)
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	version, err := store.Migrate(ctx)
	if err != nil || version != storage.CurrentVersion() {
		t.Fatalf("current schema migration got=%v err=%v", version, err)
	}
	bytes, err := os.ReadFile("../../scope/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := registry.ParseManifest(bytes)
	if err != nil {
		t.Fatal(err)
	}
	identities, err := registry.NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	return nha100Fixture{ctx: ctx, store: store, settings: settings, manifest: manifest, registry: identities, repo: repo, service: service}
}
func (f nha100Fixture) bind(t *testing.T, alias string) CalendarID {
	t.Helper()
	instance, err := domain.ParseIntegrationInstanceID("nha100-synthetic-local-only")
	if err != nil {
		t.Fatal(err)
	}
	source, err := integrations.NewSourceKey("nha100-synthetic-"+alias, "calendar")
	if err != nil {
		t.Fatal(err)
	}
	configured, err := f.registry.ConfigureBinding(f.ctx, instance, source, alias)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := f.registry.Resolve(f.ctx, instance, []integrations.SourceKey{source})
	if err != nil || len(resolved) != 1 || resolved[0] != configured {
		t.Fatalf("real registry resolve: %v", err)
	}
	id, err := f.service.Bind(f.ctx, configured.Entity)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func (f nha100Fixture) calendar(t *testing.T, alias string) CalendarID {
	t.Helper()
	id := f.bind(t, alias)
	absent, err := f.service.GetCalendar(f.ctx, id)
	if err != nil || absent.Revision().Presence() != Absent || !absent.Valid() {
		t.Fatalf("initial absence: %v", err)
	}
	created, err := f.service.CreateCalendar(f.ctx, id, absent.Revision(), "Synthetic "+alias)
	if err != nil || !created.Valid() {
		t.Fatalf("create calendar: %v", err)
	}
	return id
}
func nha100Key(t *testing.T, id CalendarID, uid string) EventKey {
	t.Helper()
	u, e := ParseUID(uid)
	if e != nil {
		t.Fatal(e)
	}
	k, e := EventIdentity(id, u)
	if e != nil {
		t.Fatal(e)
	}
	return k
}
func (f nha100Fixture) create(t *testing.T, id CalendarID, uid string, event Event) EventSnapshot {
	t.Helper()
	key := nha100Key(t, id, uid)
	absent, err := f.service.GetEvent(f.ctx, key)
	if err != nil || absent.Revision().Presence() != Absent || !absent.Valid() {
		t.Fatalf("event absence: %v", err)
	}
	snapshot, err := f.service.CreateEvent(f.ctx, key, absent.Revision(), event)
	if err != nil || !snapshot.Valid() {
		t.Fatalf("create event: %v", err)
	}
	return snapshot
}
func nha100SQL(t *testing.T, f nha100Fixture, sql string, args ...any) {
	t.Helper()
	if err := f.store.InTransaction(f.ctx, func(tx pgx.Tx) error { _, err := tx.Exec(f.ctx, sql, args...); return err }); err != nil {
		t.Fatalf("synthetic SQL fixture failed: %v", err)
	}
}
func nha100Window(t *testing.T, from, to string) Window {
	t.Helper()
	w, e := DateWindow(nha100Date(t, from), nha100Date(t, to))
	if e != nil {
		t.Fatal(e)
	}
	return w
}

func TestNHA100RealIdentitiesCRUDRestoreAndYAML(t *testing.T) {
	f := nha100Setup(t)
	manifestBytes, err := os.ReadFile("../../scope/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	scopeHash := sha256.Sum256(manifestBytes)
	var source struct {
		Entities []struct {
			ID     string `json:"entity_id"`
			Source string `json:"source"`
		} `json:"entities"`
	}
	if err := json.Unmarshal(manifestBytes, &source); err != nil {
		t.Fatal(err)
	}
	var aliases []string
	for _, entity := range source.Entities {
		if entity.Source == "local_calendar" {
			aliases = append(aliases, entity.ID)
		}
	}
	slices.Sort(aliases)
	want := []string{"calendar.anniversaires", "calendar.black_bin_collections", "calendar.blue_bin_collections", "calendar.branches_collections", "calendar.family", "calendar.green_bin_collections", "calendar.holidays", "calendar.sprinklers"}
	if !slices.Equal(aliases, want) {
		t.Fatalf("eight local identities expected=%v obtained=%v", want, aliases)
	}
	ids := make(map[string]CalendarID)
	entities := make(map[domain.EntityID]bool)
	for _, alias := range aliases {
		id := f.calendar(t, alias)
		if id.origin != Local || entities[id.entity] {
			t.Fatal("identity/origin collision")
		}
		ids[alias] = id
		entities[id.entity] = true
	}
	family := ids["calendar.family"]
	remote := f.bind(t, "calendar.famille")
	workday := f.bind(t, "calendar.workday_sensor_calendar")
	if family == remote || remote.origin != RemoteICS || workday.origin != Calculated {
		t.Fatal("origins conflated")
	}
	for _, id := range []CalendarID{remote, workday} {
		absent, e := f.service.GetCalendar(f.ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		_, e = f.service.CreateCalendar(f.ctx, id, absent.Revision(), "Synthetic")
		nha100Code(t, e, ReadOnly)
	}
	yamlPath := filepath.Join(t.TempDir(), "configuration.yaml")
	yaml := []byte("# synthetic fixture; business edits are database only\ncalendar:\n  - id: family\n")
	if e := os.WriteFile(yamlPath, yaml, 0600); e != nil {
		t.Fatal(e)
	}
	yamlHash := sha256.Sum256(yaml)
	moved, _ := Move(nha100Point(t, AllDay, "2026-08-24"), nha100Day(t, "2026-08-27", "2026-08-29"))
	cancel, _ := Cancel(nha100Point(t, AllDay, "2026-08-25"))
	definition := nha100Event(t, nha100Day(t, "2026-08-22", "2026-08-23"), "FREQ=DAILY;COUNT=4", func(in *EventInput) {
		in.Description = "Synthetic description"
		in.Location = "Synthetic room"
		in.Additions = []Point{nha100Point(t, AllDay, "2026-08-26")}
		in.Exclusions = []Point{nha100Point(t, AllDay, "2026-08-23")}
		in.Exceptions = []Exception{moved, cancel}
	})
	first := f.create(t, family, "same-open-uid", definition)
	other := f.create(t, ids["calendar.anniversaires"], "same-open-uid", nha100Event(t, nha100Day(t, "2026-02-02", "2026-02-03"), "", nil))
	if first.key == other.key || first.revision == other.revision {
		t.Fatal("UID merged across CalendarID")
	}
	timed := nha100Event(t, nha100Timed(t, "2026-03-01T02:30:00", "2026-03-01T08:30:00Z", "America/New_York"), "FREQ=WEEKLY;COUNT=3", func(in *EventInput) {
		moved, _ := Move(nha100Point(t, Timed, "2026-03-15T02:30:00"), nha100Timed(t, "2026-03-09T01:30:00", "2026-03-09T07:30:00Z", "America/New_York"))
		cancel, _ := Cancel(nha100Point(t, Timed, "2026-03-22T02:30:00"))
		in.Exceptions = []Exception{moved, cancel}
		in.Exclusions = []Point{nha100Point(t, Timed, "2026-03-01T02:30:00")}
		in.Additions = []Point{nha100Point(t, Timed, "2026-03-10T02:30:00")}
	})
	clock := f.create(t, family, "dst-weekly", timed)
	// Close both actual Store and application service; reopen the same isolated DB.
	f.service.Close()
	f.store.Close()
	store, e := storage.Open(f.ctx, f.settings)
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	repo, e := NewRepository(store, f.manifest)
	if e != nil {
		t.Fatal(e)
	}
	service, e := NewService(repo)
	if e != nil {
		t.Fatal(e)
	}
	defer service.Close()
	for _, before := range []EventSnapshot{first, other, clock} {
		after, e := service.GetEvent(f.ctx, before.key)
		if e != nil || !after.Valid() || before.key != after.key || before.revision != after.revision || before.updatedAt != after.updatedAt || !reflect.DeepEqual(before.definition, after.definition) {
			t.Fatalf("restore expected=%v got=%v err=%v", before.revision, after.revision, e)
		}
	}
	timedWindow, e := TimeWindow(nha100Instant(t, "2026-03-09T06:00:00Z"), nha100Instant(t, "2026-03-11T00:00:00Z"), nha100Zone(t, "America/New_York"))
	if e != nil {
		t.Fatal(e)
	}
	timedPage, e := service.Query(f.ctx, family, timedWindow, 10, Cursor{})
	if e != nil {
		t.Fatal(e)
	}
	nha100ExpectStarts(t, timedPage.Items(), "2026-03-09T05:30:00Z", "2026-03-10T06:30:00Z")
	if timedPage.items[0].id.point.String() != "2026-03-15T02:30:00" {
		t.Fatal("future original recurrence ID lost after restore")
	}
	window := nha100Window(t, "2026-08-28", "2026-08-29")
	page, e := service.Query(f.ctx, family, window, 5, Cursor{})
	if e != nil {
		t.Fatal(e)
	}
	nha100ExpectStarts(t, page.Items(), "2026-08-27")
	if page.items[0].id.point.String() != "2026-08-24" {
		t.Fatal("stored moved origin lost")
	}
	changed := nha100Event(t, nha100Day(t, "2026-08-22", "2026-08-24"), "FREQ=DAILY;COUNT=4", nil)
	updated, e := service.UpdateEvent(f.ctx, first.key, first.revision, changed)
	if e != nil || updated.revision.number != 2 {
		t.Fatal(e)
	}
	if _, e := service.UpdateEvent(f.ctx, first.key, first.revision, definition); e == nil {
		t.Fatal("stale event CAS accepted")
	} else {
		nha100Code(t, e, Conflict)
	}
	deleted, e := service.DeleteEvent(f.ctx, first.key, updated.revision)
	if e != nil || !deleted.Valid() || deleted.revision.presence != Removed {
		t.Fatal(e)
	}
	if _, e := service.CreateEvent(f.ctx, first.key, deleted.revision, definition); e == nil {
		t.Fatal("deleted UID resurrected")
	} else {
		nha100Code(t, e, Deleted)
	}
	cal, e := service.GetCalendar(f.ctx, family)
	if e != nil {
		t.Fatal(e)
	}
	renamed, e := service.UpdateCalendar(f.ctx, family, cal.revision, "Renamed synthetic")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := service.UpdateCalendar(f.ctx, family, cal.revision, "Stale synthetic"); e == nil {
		t.Fatal("stale calendar CAS accepted")
	} else {
		nha100Code(t, e, Conflict)
	}
	removed, e := service.DeleteCalendar(f.ctx, family, renamed.revision)
	if e != nil || removed.revision.presence != Removed {
		t.Fatal(e)
	}
	if _, e := service.GetEvent(f.ctx, clock.key); e == nil {
		t.Fatal("deleted calendar still active")
	} else {
		nha100Code(t, e, Deleted)
	}
	current, e := os.ReadFile(yamlPath)
	if e != nil || sha256.Sum256(current) != yamlHash {
		t.Fatal("YAML changed")
	}
	current, e = os.ReadFile("../../scope/manifest.json")
	if e != nil || sha256.Sum256(current) != scopeHash {
		t.Fatal("frozen scope changed")
	}
}
func TestNHA100ConcurrentCASAndPagination(t *testing.T) {
	f := nha100Setup(t)
	id := f.calendar(t, "calendar.family")
	// Race the ABSENT calendar's first INSERT as well: the registry identity
	// lock is exclusive from acquisition, never upgraded SHARE->UPDATE.
	pending := f.bind(t, "calendar.anniversaires")
	absentCal, err := f.repo.GetCalendar(f.ctx, pending)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	calendarResults := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			_, e := f.repo.CreateCalendar(f.ctx, pending, absentCal.revision, "Synthetic concurrent first create")
			calendarResults <- e
		}()
	}
	close(start)
	calendarWins := 0
	for i := 0; i < 2; i++ {
		if e := <-calendarResults; e == nil {
			calendarWins++
		} else {
			nha100Code(t, e, Conflict)
		}
	}
	if calendarWins != 1 {
		t.Fatal("absent calendar CAS did not have one winner")
	}
	event := nha100Event(t, nha100Day(t, "2026-01-01", "2026-01-03"), "FREQ=DAILY", nil)
	initial := f.create(t, id, "infinite", event)
	window := nha100Window(t, "2026-01-02", "2026-01-12")
	var cursor Cursor
	var all []Occurrence
	for pages := 0; pages < 10; pages++ {
		page, e := f.service.Query(f.ctx, id, window, 3, cursor)
		if e != nil {
			t.Fatal(e)
		}
		all = append(all, page.Items()...)
		next, more := page.Next()
		if !more {
			break
		}
		cursor = next
	}
	if len(all) != 11 {
		t.Fatalf("overlap/page expected11 got%d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if compareOrder(all[i-1].order, all[i].order) >= 0 {
			t.Fatal("page duplicate or disorder")
		}
	}
	page, e := f.service.Query(f.ctx, id, window, 2, Cursor{})
	if e != nil {
		t.Fatal(e)
	}
	stale, more := page.Next()
	if !more {
		t.Fatal("missing continuation")
	}
	a := nha100Event(t, nha100Day(t, "2026-01-01", "2026-01-02"), "FREQ=DAILY;COUNT=3", nil)
	b := nha100Event(t, nha100Day(t, "2026-01-01", "2026-01-02"), "FREQ=DAILY;COUNT=5", nil)
	ready := make(chan struct{})
	type answer struct {
		snapshot EventSnapshot
		err      error
	}
	answers := make(chan answer, 2)
	for _, next := range []Event{a, b} {
		go func(next Event) {
			<-ready
			s, e := f.repo.UpdateEvent(f.ctx, initial.key, initial.revision, next)
			answers <- answer{s, e}
		}(next)
	}
	close(ready)
	wins, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		got := <-answers
		if got.err == nil {
			if !got.snapshot.Valid() || got.snapshot.revision.number != 2 {
				t.Fatal("invalid CAS success")
			}
			wins++
		} else {
			nha100Code(t, got.err, Conflict)
			if got.snapshot.Valid() {
				t.Fatal("CAS failure exposed snapshot")
			}
			conflicts++
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	_, e = f.service.Query(f.ctx, id, window, 2, stale)
	nha100Code(t, e, StaleCursor)
	otherWindow := nha100Window(t, "2026-02-01", "2026-02-02")
	_, e = f.service.Query(f.ctx, id, otherWindow, 2, stale)
	nha100Code(t, e, InvalidInput)
	// Changing temporal variants removes children before the FK parent update.
	current, e := f.service.GetEvent(f.ctx, initial.key)
	if e != nil {
		t.Fatal(e)
	}
	timed := nha100Event(t, nha100Timed(t, "2026-03-07T02:30:00", "2026-03-07T08:30:00Z", "America/New_York"), "FREQ=DAILY;COUNT=3", nil)
	current, e = f.service.UpdateEvent(f.ctx, initial.key, current.revision, timed)
	if e != nil {
		t.Fatal(e)
	}
	after, e := f.service.GetEvent(f.ctx, initial.key)
	if e != nil || !reflect.DeepEqual(current, after) {
		t.Fatal("temporal variant roundtrip", e)
	}
}

func TestNHA100ImagePreflightConcurrencyAndTombstones(t *testing.T) {
	f := nha100Setup(t)
	id := f.calendar(t, "calendar.family")
	seed := f.create(t, id, "seed", nha100Event(t, nha100Day(t, "2026-01-01", "2026-01-02"), "FREQ=DAILY", nil))
	entity, _ := id.entity.Value()
	window := nha100Window(t, "2026-01-01", "2026-01-03")
	// Both real repository loads reserve before the scheduling seam. A third
	// request is refused without scanning rows or spawning a waiting worker.
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var loads atomic.Int32
	f.repo.beforeImageLoad = func() { loads.Add(1); entered <- struct{}{}; <-release }
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, err := f.service.Query(f.ctx, id, window, 1, Cursor{}); results <- err }()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-f.ctx.Done():
			t.Fatal("image requests not admitted")
		}
	}
	_, err := f.service.Query(f.ctx, id, window, 1, Cursor{})
	nha100Code(t, err, Busy)
	if loads.Load() != 2 {
		t.Fatal("third request loaded rows")
	}
	images.mu.Lock()
	active, used := images.active, images.used
	images.mu.Unlock()
	if active != 2 || used > MaxAggregateImageBytes || used <= 0 {
		t.Fatalf("ledger active=%d used=%d", active, used)
	}
	unblock()
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	f.repo.beforeImageLoad = func() { loads.Add(1) }
	loads.Store(0)
	// >1024 compact historical UIDs never participate in active image counts.
	nha100SQL(t, f, `INSERT INTO public.calendar_events(calendar_id,uid,lifecycle,revision,updated_at)
 SELECT $1,'deleted-'||g,2,7,clock_timestamp() FROM generate_series(1,1025) g`, entity)
	tomb, err := f.service.GetEvent(f.ctx, nha100Key(t, id, "deleted-1025"))
	if err != nil || !tomb.Valid() || tomb.revision.presence != Removed || tomb.revision.number != 7 || tomb.definition.Valid() {
		t.Fatal("compact tombstone decode", err)
	}
	if loads.Load() != 0 {
		t.Fatal("tombstone attempted image/span decode")
	}
	_, err = f.service.CreateEvent(f.ctx, tomb.key, tomb.revision, seed.definition)
	nha100Code(t, err, Deleted)
	fresh := f.create(t, id, "after-history", seed.definition)
	if !fresh.Valid() {
		t.Fatal("history blocked a new UID")
	}
	if _, err := f.service.Query(f.ctx, id, window, 3, Cursor{}); err != nil {
		t.Fatal("history blocked query", err)
	}
	// Populate a shape-valid active image at its1024-row boundary. Those1024
	// events each carrying256 overrides would cost >500MiB, and are refused by
	// SQL scalars before any parent or exception is loaded into Go.
	nha100SQL(t, f, `INSERT INTO public.calendar_events(calendar_id,uid,lifecycle,revision,updated_at,summary,description,location,temporal_kind,start_date,end_date,recurrence_profile,recurrence_rule)
 SELECT calendar_id,'mass-'||g,1,1,clock_timestamp(),summary,description,location,temporal_kind,start_date,end_date,recurrence_profile,recurrence_rule
 FROM public.calendar_events CROSS JOIN generate_series(1,1022) g WHERE calendar_id=$1 AND uid='seed'`, entity)
	nha100SQL(t, f, `INSERT INTO public.calendar_exceptions(calendar_id,uid,temporal_kind,point_key,date_value,action)
 SELECT calendar_id,uid,1,to_char(DATE '2026-01-01'+g,'YYYY-MM-DD'),DATE '2026-01-01'+g,1
 FROM public.calendar_events CROSS JOIN generate_series(1,256) g WHERE calendar_id=$1 AND lifecycle=1`, entity)
	loads.Store(0)
	_, err = f.service.Query(f.ctx, id, window, 1, Cursor{})
	nha100Code(t, err, CapacityExceeded)
	if loads.Load() != 0 {
		t.Fatal("oversize image loaded before refusal")
	}
	// Deletion must escape an oversized/full image rather than attempting to
	// load or validate it. Payload/children become a compact anti-ABA marker.
	deleted, err := f.service.DeleteEvent(f.ctx, seed.key, seed.revision)
	if err != nil || deleted.revision.presence != Removed {
		t.Fatal("full-image deletion blocked", err)
	}
	if loads.Load() != 0 {
		t.Fatal("delete loaded the full image")
	}
	deletedRead, err := f.service.GetEvent(f.ctx, seed.key)
	if err != nil || !deletedRead.Valid() || deletedRead.revision != deleted.revision {
		t.Fatal("removed revision lost", err)
	}
	// Drop the synthetic overrides, keeping1023 active rows. The freed slot is
	// available to a new UID and the1024-active boundary remains unchanged.
	nha100SQL(t, f, `DELETE FROM public.calendar_exceptions WHERE calendar_id=$1`, entity)
	replacement := f.create(t, id, "freed-slot", seed.definition)
	if !replacement.Valid() {
		t.Fatal("delete did not free capacity")
	}
	nextKey := nha100Key(t, id, "over-active-limit")
	absent, err := f.service.GetEvent(f.ctx, nextKey)
	if err != nil {
		t.Fatal(err)
	}
	refused, err := f.service.CreateEvent(f.ctx, nextKey, absent.revision, seed.definition)
	nha100Code(t, err, CapacityExceeded)
	if refused.Valid() {
		t.Fatal("capacity failure acknowledged")
	}
	var activeCount int
	err = f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(f.ctx, `SELECT count(*) FROM public.calendar_events WHERE calendar_id=$1 AND lifecycle=1`, entity).Scan(&activeCount)
	})
	if err != nil || activeCount != 1024 {
		t.Fatalf("active limit=%d err=%v", activeCount, err)
	}
	images.mu.Lock()
	active, used = images.active, images.used
	images.mu.Unlock()
	if active != 0 || used != 0 {
		t.Fatalf("image leak active=%d bytes=%d", active, used)
	}
}

func TestNHA100CommitBarrierRollbackUnknownAndNotifications(t *testing.T) {
	f := nha100Setup(t)
	id := f.calendar(t, "calendar.family")
	original := nha100Event(t, nha100Day(t, "2026-01-01", "2026-01-02"), "", nil)
	initial := f.create(t, id, "commit-proof", original)
	sub, err := f.service.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	nha100SQL(t, f, `CREATE FUNCTION public.nha100_commit_barrier() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN PERFORM pg_advisory_xact_lock(481316,100); RETURN NEW; END $$;
 CREATE CONSTRAINT TRIGGER nha100_commit_barrier AFTER UPDATE ON public.calendar_events DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha100_commit_barrier()`)
	locked, release := make(chan struct{}), make(chan struct{})
	holder := make(chan error, 1)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	go func() {
		holder <- f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
			if _, e := tx.Exec(f.ctx, `SELECT pg_advisory_xact_lock(481316,100)`); e != nil {
				return e
			}
			close(locked)
			select {
			case <-release:
				return nil
			case <-f.ctx.Done():
				return f.ctx.Err()
			}
		})
	}()
	select {
	case <-locked:
	case <-f.ctx.Done():
		t.Fatal("holder did not acquire barrier")
	}
	changed := nha100Event(t, nha100Day(t, "2026-01-01", "2026-01-03"), "", nil)
	type ack struct {
		snapshot EventSnapshot
		err      error
	}
	acknowledged := make(chan ack, 1)
	go func() {
		s, e := f.service.UpdateEvent(f.ctx, initial.key, initial.revision, changed)
		acknowledged <- ack{s, e}
	}()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		err = f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND classid=481316 AND objid=100 AND NOT granted)`).Scan(&blocked)
		})
		if err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case result := <-acknowledged:
			t.Fatalf("ACK before SQL COMMIT barrier: %v", result.err)
		case <-ticker.C:
		case <-f.ctx.Done():
			t.Fatal("COMMIT barrier not reached")
		}
	}
	select {
	case result := <-acknowledged:
		t.Fatalf("ACK escaped barrier: %v", result.err)
	default:
	}
	if len(sub.queue) != 0 {
		t.Fatal("notification escaped uncommitted mutation")
	}
	var visibleRevision int64
	entity, _ := id.entity.Value()
	err = f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(f.ctx, `SELECT revision FROM public.calendar_events WHERE calendar_id=$1 AND uid=$2`, entity, initial.key.uid.value).Scan(&visibleRevision)
	})
	if err != nil || visibleRevision != 1 {
		t.Fatal("uncommitted data escaped", err)
	}
	unblock()
	if err := <-holder; err != nil {
		t.Fatal(err)
	}
	result := <-acknowledged
	if result.err != nil || !result.snapshot.Valid() {
		t.Fatal(result.err)
	}
	change, err := sub.Next(f.ctx)
	if err != nil || change.kind != EventUpdated || change.event != initial.key || change.revision != result.snapshot.calendarRevision {
		t.Fatal("committed change missing", err)
	}
	nha100SQL(t, f, `DROP TRIGGER nha100_commit_barrier ON public.calendar_events;
 CREATE FUNCTION public.nha100_callback_failure() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'synthetic callback failure'; END $$;
 CREATE TRIGGER nha100_callback_failure BEFORE UPDATE ON public.calendar_events FOR EACH ROW EXECUTE FUNCTION public.nha100_callback_failure()`)
	refused, err := f.service.UpdateEvent(f.ctx, initial.key, result.snapshot.revision, original)
	nha100Code(t, err, Unavailable)
	if refused.Valid() || len(sub.queue) != 0 {
		t.Fatal("rollback acknowledged/notified")
	}
	nha100SQL(t, f, `DROP TRIGGER nha100_callback_failure ON public.calendar_events;
 CREATE SEQUENCE public.nha100_commit_attempts;
 CREATE FUNCTION public.nha100_deferred_failure() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN PERFORM nextval('public.nha100_commit_attempts'); RAISE EXCEPTION 'synthetic deferred failure'; END $$;
 CREATE CONSTRAINT TRIGGER nha100_deferred_failure AFTER UPDATE ON public.calendar_events DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha100_deferred_failure()`)
	// A real deferred server error during COMMIT is conservatively unknown under
	// Store's contract. It is not presented as a simulated network loss.
	refused, err = f.service.UpdateEvent(f.ctx, initial.key, result.snapshot.revision, original)
	nha100Code(t, err, CommitUnknown)
	if refused.Valid() || len(sub.queue) != 0 {
		t.Fatal("unknown commit acknowledged/notified")
	}
	primary, e := storage.Open(f.ctx, f.settings)
	if e != nil {
		t.Fatal(e)
	}
	defer primary.Close()
	repo, e := NewRepository(primary, f.manifest)
	if e != nil {
		t.Fatal(e)
	}
	observed, e := repo.GetEvent(f.ctx, initial.key)
	if e != nil || observed.revision != result.snapshot.revision {
		t.Fatal("primary reconciliation failed", e)
	}
	var attempts int64
	err = primary.InTransaction(f.ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(f.ctx, `SELECT last_value FROM public.nha100_commit_attempts`).Scan(&attempts)
	})
	if err != nil || attempts != 1 {
		t.Fatalf("unknown commit retried attempts=%d err=%v", attempts, err)
	}
}

func TestNHA100CancellationAvailabilityAndCloseJoined(t *testing.T) {
	f := nha100Setup(t)
	id := f.calendar(t, "calendar.family")
	f.create(t, id, "rare", nha100Event(t, nha100Day(t, "2096-02-29", "2096-03-01"), "FREQ=YEARLY", nil))
	window := nha100Window(t, "2100-01-01", "2101-01-01")
	entered := make(chan struct{})
	var selected atomic.Bool
	// Schedule the first real service request after its actual SQL image was
	// admitted and released, while the second request executes expansion.
	f.repo.beforeExpansion = func(ctx context.Context) error {
		if selected.CompareAndSwap(false, true) {
			close(entered)
			<-ctx.Done()
			return canceled(ctx)
		}
		return nil
	}
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.service.Query(ctx, id, window, 1, Cursor{}); done <- err }()
	select {
	case <-entered:
	case <-f.ctx.Done():
		t.Fatal("first request not admitted")
	}
	page, err := f.service.Query(f.ctx, id, window, 1, Cursor{})
	if err != nil || len(page.Items()) != 0 {
		t.Fatal("service unavailable during admitted request", err)
	}
	started := time.Now()
	cancel()
	err = <-done
	nha100Code(t, err, Canceled)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancel cause lost")
	}
	elapsed := time.Since(started)
	if elapsed > 100*time.Millisecond {
		t.Fatalf("joined cancel too slow: %s", elapsed)
	}
	if _, err := f.service.Query(f.ctx, id, window, 1, Cursor{}); err != nil {
		t.Fatal("service unavailable after cancel", err)
	}
	entered = make(chan struct{})
	selected.Store(false)
	done = make(chan error, 1)
	go func() { _, err := f.service.Query(f.ctx, id, window, 1, Cursor{}); done <- err }()
	select {
	case <-entered:
	case <-f.ctx.Done():
		t.Fatal("close request not admitted")
	}
	f.service.Close()
	err = <-done
	nha100Code(t, err, Canceled)
	f.service.mu.Lock()
	calls := len(f.service.calls)
	f.service.mu.Unlock()
	images.mu.Lock()
	active, used := images.active, images.used
	images.mu.Unlock()
	if calls != 0 || active != 0 || used != 0 {
		t.Fatalf("Close unjoined calls=%d images=%d bytes=%d", calls, active, used)
	}
	_, err = f.service.Query(f.ctx, id, window, 1, Cursor{})
	nha100Code(t, err, Closed)
	t.Logf("joined cancellation=%s; second request completed concurrently; Close calls/images=0", elapsed)
}

func TestNHA100BoundedSubscribersAndPersistedIncompatibility(t *testing.T) {
	f := nha100Setup(t)
	id := f.calendar(t, "calendar.family")
	var subs []*Subscription
	for i := 0; i < MaxSubscribers; i++ {
		s, e := f.service.Subscribe()
		if e != nil {
			t.Fatal(e)
		}
		subs = append(subs, s)
	}
	_, err := f.service.Subscribe()
	nha100Code(t, err, CapacityExceeded)
	for _, s := range subs {
		s.Close()
	}
	slow, err := f.service.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	cal, err := f.service.GetCalendar(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 22; i++ {
		cal, err = f.service.UpdateCalendar(f.ctx, id, cal.revision, "Synthetic revision beyond config-history20")
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = slow.Next(f.ctx)
	nha100Code(t, err, ResyncRequired)
	if len(slow.queue) != NotificationCapacity {
		t.Fatal("notification queue unbounded")
	}
	if cal.revision.number != 23 {
		t.Fatal("business revision incorrectly capped at20")
	}
	event := f.create(t, id, "corruption", nha100Event(t, nha100Day(t, "2026-01-01", "2026-01-02"), "FREQ=DAILY", nil))
	entity, _ := id.entity.Value()
	nha100SQL(t, f, `UPDATE public.calendar_events SET recurrence_profile='future/profile' WHERE calendar_id=$1 AND uid=$2`, entity, event.key.uid.value)
	_, err = f.service.GetEvent(f.ctx, event.key)
	nha100Code(t, err, Incompatible)
	nha100SQL(t, f, `UPDATE public.calendar_events SET recurrence_profile=$3,recurrence_rule='FREQ=SECONDLY' WHERE calendar_id=$1 AND uid=$2`, entity, event.key.uid.value, RecurrenceProfile)
	_, err = f.service.GetEvent(f.ctx, event.key)
	nha100Code(t, err, Corrupt)
	// A fresh valid definition is never inferred from corrupt persisted text.
	refused, err := f.service.GetEvent(f.ctx, nha100Key(t, id, "absent-even-with-corrupt-neighbor"))
	if err != nil || refused.revision.presence != Absent || !refused.Valid() {
		t.Fatal("absence collapsed into unavailable", err)
	}
}

func TestNHA100SchemaAndRevisionExhaustion(t *testing.T) {
	f := nha100Setup(t)
	id := f.calendar(t, "calendar.family")
	event := f.create(t, id, "schema", nha100Event(t, nha100Day(t, "2026-01-01", "2026-01-02"), "", nil))
	entity, _ := id.entity.Value()
	for _, sql := range []string{
		`UPDATE public.calendar_events SET temporal_kind=99 WHERE calendar_id=$1`,
		`UPDATE public.calendar_events SET end_date=start_date WHERE calendar_id=$1`,
		`UPDATE public.calendar_events SET end_date=DATE 'infinity' WHERE calendar_id=$1`,
		`UPDATE public.calendar_events SET start_at=TIMESTAMPTZ '2026-01-01T00:00:00Z' WHERE calendar_id=$1`,
		`UPDATE public.calendar_events SET lifecycle=2 WHERE calendar_id=$1`,
		`UPDATE public.calendar_events SET revision=0 WHERE calendar_id=$1`,
	} {
		if err := f.store.InTransaction(f.ctx, func(tx pgx.Tx) error { _, e := tx.Exec(f.ctx, sql, entity); return e }); err == nil {
			t.Fatalf("invalid SQL variant accepted: %s", sql)
		}
	}
	nha100SQL(t, f, `UPDATE public.calendar_events SET revision=9223372036854775807 WHERE calendar_id=$1`, entity)
	maxed, err := f.service.GetEvent(f.ctx, event.key)
	if err != nil || !maxed.Valid() {
		t.Fatal(err)
	}
	refused, err := f.service.UpdateEvent(f.ctx, event.key, maxed.revision, event.definition)
	nha100Code(t, err, RevisionExhausted)
	if refused.Valid() {
		t.Fatal("exhausted event acknowledged")
	}
	nha100SQL(t, f, `UPDATE public.calendar_events SET revision=1 WHERE calendar_id=$1`, entity)
	nha100SQL(t, f, `UPDATE public.local_calendars SET revision=9223372036854775807 WHERE entity_id=$1`, entity)
	before, err := f.service.GetEvent(f.ctx, event.key)
	if err != nil {
		t.Fatal(err)
	}
	refused, err = f.service.UpdateEvent(f.ctx, event.key, before.revision, nha100Event(t, nha100Day(t, "2026-01-01", "2026-01-03"), "", nil))
	nha100Code(t, err, RevisionExhausted)
	if refused.Valid() {
		t.Fatal("exhausted calendar acknowledged event")
	}
	after, err := f.service.GetEvent(f.ctx, event.key)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("calendar exhaustion did not roll back event", err)
	}
}
