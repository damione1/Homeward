package server

import (
	"context"
	"errors"
	"fmt"
	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type nha101RunResolver struct{}

func (nha101RunResolver) ResolveEntity(domain.HAAlias) (domain.EntityID, error) {
	return domain.ParseEntityID("Host:nha101-unused")
}
func (nha101RunResolver) ResolveDevice(ir.HADeviceRef) (domain.DeviceID, error) {
	return domain.DeviceID{}, automationtriggers.ErrInvalidPlan
}
func (nha101RunResolver) ResolveCalendar(alias domain.HAAlias) (automationtriggers.CalendarSourceID, error) {
	raw, err := alias.Value()
	if err != nil || raw != remoteCalendarAlias {
		return automationtriggers.CalendarSourceID{}, automationtriggers.ErrInvalidPlan
	}
	return automationtriggers.ParseCalendarSourceID(raw)
}
func nha101RunTriggerPlan(t *testing.T) automationtriggers.Plan {
	t.Helper()
	calendar, err := ir.NewEntity(remoteCalendarAlias, ir.ReferencePresent)
	if err != nil {
		t.Fatal(err)
	}
	target, err := ir.NewEntity("switch.synthetic", ir.ReferencePresent, ir.SupportObserveState, ir.SupportSwitchTurnOn)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue([]ir.Entity{calendar, target}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := "id: nha101_edges\ntriggers:\n- trigger: calendar\n  entity_id: calendar.famille\n  event: start\n- trigger: calendar\n  entity_id: calendar.famille\n  event: end\nactions:\n- action: switch.turn_on\n  target: {entity_id: switch.synthetic}\n  data: {}\n"
	source, err := parser.NewSourceFile("nha101-edges.yaml", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := ir.CompileSources("automation.nha101_edges", "nha101-edges.yaml", []parser.SourceFile{source}, catalogue)
	if err != nil || len(compiled.Accepted()) != 1 {
		t.Fatalf("trigger fixture rejected: %v", err)
	}
	plan, err := automationtriggers.NewPlan(automationtriggers.PlanInput{Automations: compiled.Accepted(), Zone: "America/New_York", Resolver: nha101RunResolver{}})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}
func nha101RunWait(t *testing.T, condition func() bool) {
	t.Helper()
	until := time.Now().Add(8 * time.Second)
	for time.Now().Before(until) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("NHA101 real Run condition not reached")
}

type nha101RunHandle struct {
	cancel     context.CancelFunc
	done       chan error
	runtime    *remoteCalendarRuntime
	activation *configactivation.Owner
	sequence   *sequencer.Sequencer
	results    AutomationTriggerResults
	clock      *nha105RunClock
	joined     bool
}

func (h *nha101RunHandle) stop(t *testing.T) {
	t.Helper()
	h.cancel()
	if h.joined {
		return
	}
	select {
	case err := <-h.done:
		h.joined = true
		if err != nil {
			t.Error(err)
		}
	case <-time.After(10 * time.Second):
		t.Error("NHA101 Run did not join")
	}
	if h.runtime != nil {
		select {
		case <-h.runtime.Done():
		default:
			t.Error("NHA101 runtime reported return before its true Done")
		}
	}
}
func nha101RunStart(t *testing.T, startup config.StartupInputs, at time.Time, triggers bool, configure func(*remoteCalendarRuntime)) *nha101RunHandle {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &nha101RunHandle{cancel: cancel, done: make(chan error, 1), clock: &nha105RunClock{wall: at}}
	t.Cleanup(func() { h.stop(t) })
	owner, sequence, results := make(chan *remoteCalendarRuntime, 1), make(chan *sequencer.Sequencer, 1), make(chan AutomationTriggerResults, 1)
	var factory RuntimeFactory
	if triggers {
		plan := nha101RunTriggerPlan(t)
		// Only the040 plan and its readonly consumer are injected. Run composes
		// every identity/calendar/source/window/publisher from admitted YAML.
		factory = func(context.Context, *storage.Store) (RuntimePlan, error) {
			return RuntimePlan{AutomationTriggers: &plan, ConfigureAutomationTriggerResults: func(_ context.Context, reader AutomationTriggerResults) error { results <- reader; return nil }}, nil
		}
	}
	activation := make(chan *configactivation.Owner, 1)
	ready := make(chan struct{})
	dependencies := runDependencies{sequenceClock: h.clock, remoteCalendarClock: h.clock, maintenanceClock: h.clock,
		observeRemoteActivation: func(actual *configactivation.Owner) { activation <- actual },
		observeRemoteCalendar: func(actual *remoteCalendarRuntime) {
			if configure != nil {
				configure(actual)
			}
			owner <- actual
		},
		observe: func(_ context.Context, actual *sequencer.Sequencer) error { sequence <- actual; return nil }}
	go func() {
		h.done <- runWithDependencies(ctx, startup, (&net.ListenConfig{}).Listen, func(net.Addr) { close(ready) }, factory, dependencies)
	}()
	select {
	case <-ready:
	case err := <-h.done:
		h.joined = true
		t.Fatalf("NHA101 Run before serving: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("NHA101 Run not serving")
	}
	select {
	case h.runtime = <-owner:
	case <-time.After(time.Second):
		t.Fatal("actual YAML owner absent")
	}
	select {
	case h.sequence = <-sequence:
	case <-time.After(time.Second):
		t.Fatal("actual Run sequencer absent")
	}
	select {
	case h.activation = <-activation:
	case <-time.After(time.Second):
		t.Fatal("actual activation observation absent")
	}
	if triggers {
		select {
		case h.results = <-results:
		case <-time.After(time.Second):
			t.Fatal("readonly040 consumer absent")
		}
	}
	return h
}
func nha101RunRaw(t *testing.T, port int, data, origin string) []byte {
	t.Helper()
	return []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\nremote_calendar:\n  label: Famille\n  url: %q\n  view_timezone: America/New_York\n  network:\n    origin: %q\n    prefixes: ['127.0.0.1/32']\n", data, port, origin+"/calendar.ics", origin))
}
func nha101RunReady(h *nha101RunHandle) bool {
	snapshot, err := h.sequence.Capture()
	if err != nil {
		return false
	}
	instance, _ := domain.ParseIntegrationInstanceID("homeward-remote-calendar-v1")
	status, _, err := snapshot.Health[instance].Components()
	return err == nil && status == integrations.HealthReady
}
func nha101RunCalendar(t *testing.T, h *nha101RunHandle, fresh bool) calendars.CalendarSnapshot {
	t.Helper()
	var result calendars.CalendarSnapshot
	nha101RunWait(t, func() bool {
		list, err := h.runtime.service.ListCalendars(context.Background())
		if err != nil {
			return false
		}
		for _, item := range list {
			if item.ID().Origin() == calendars.RemoteICS {
				result = item
				state, ok := item.RemoteState()
				if !ok {
					return false
				}
				quality, known := state.Freshness()
				return known && (quality == calendars.RemoteFresh) == fresh
			}
		}
		return false
	})
	return result
}

const nha101RunPointDateICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID: point uid \r\nDTSTART:20261101T041000Z\r\nSUMMARY:point\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:date\r\nDTSTART;VALUE=DATE:20261101\r\nDTEND;VALUE=DATE:20261102\r\nSUMMARY:DST day\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func TestNHA101RunRealHTTPCommit040BeforeReadyPointDateAndStop(t *testing.T) {
	cluster := testpg.Start(t)
	var mutations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutations.Add(1)
			w.WriteHeader(405)
			return
		}
		w.Header().Set("ETag", `"nha101"`)
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Date", "Sun, 01 Nov 2026 04:00:00 GMT")
		_, _ = w.Write([]byte(nha101RunPointDateICS))
	}))
	defer server.Close()
	startup := nha029RunStartupFromRaw(t, nha101RunRaw(t, cluster.Port, shortPrivateDataDir(t), server.URL))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var successful atomic.Int32
	h := nha101RunStart(t, startup, time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC), true, func(r *remoteCalendarRuntime) {
		r.bridge.beforeReplace = func() { close(entered); <-release }
		r.bridge.afterReplace = func(err error) {
			if err == nil {
				successful.Add(1)
			}
		}
	})
	select {
	case <-entered:
	case <-time.After(8 * time.Second):
		t.Fatal("real040 publication not reached")
	}
	if nha101RunReady(h) || successful.Load() != 0 {
		t.Fatal("Ready preceded real040 replacement")
	}
	unblock()
	nha101RunWait(t, func() bool { return successful.Load() == 1 && nha101RunReady(h) })
	calendar := nha101RunCalendar(t, h, true)
	from, _ := calendars.NewInstant(h.clock.Now())
	to, _ := calendars.NewInstant(h.clock.Now().Add(48 * time.Hour))
	zone, _ := calendars.ParseZone("America/New_York")
	request, err := calendars.NewAutomationWindowRequest(from, to, zone)
	if err != nil {
		t.Fatal(err)
	}
	window, err := h.runtime.service.CollectRemoteWindow(context.Background(), calendar.ID(), request)
	if err != nil || window.Len() != 2 {
		t.Fatal("real SQL projection missing", err)
	}
	foundPoint, foundDate := false, false
	for _, item := range window.Items() {
		start, end, kind, _ := item.Instants()
		uid, _ := item.ID().Event().UID().Value()
		switch kind {
		case calendars.AutomationPoint:
			foundPoint = uid == " point uid " && start.Equal(end)
		case calendars.AutomationAllDay:
			foundDate = end.Sub(start) == 25*time.Hour
		}
	}
	if !foundPoint || !foundDate {
		t.Fatal("Point/UID spaces or DST DATE lost at real040 boundary")
	}
	h.clock.advance(10 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	fired := 0
	for fired < 2 {
		result, err := h.results.NextResult(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if result.Kind() == automationtriggers.ResultFired {
			fired++
		}
	}
	if mutations.Load() != 0 {
		t.Fatal("calendar source attempted a remote mutation")
	}
	h.stop(t)
}

func TestNHA101DefaultRunReadRestart304AndNoTriggerEngine(t *testing.T) {
	cluster := testpg.Start(t)
	var requests, conditional atomic.Int32
	var block atomic.Bool
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("ETag", `"nha101"`)
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Date", "Sun, 01 Nov 2026 04:00:00 GMT")
		if r.Header.Get("If-None-Match") == `"nha101"` {
			conditional.Add(1)
			if block.Load() {
				entered <- struct{}{}
				<-release
			}
			w.WriteHeader(304)
			return
		}
		_, _ = w.Write([]byte(nha101RunPointDateICS))
	}))
	defer server.Close()
	startup := nha029RunStartupFromRaw(t, nha101RunRaw(t, cluster.Port, shortPrivateDataDir(t), server.URL))
	at := time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)
	first := nha101RunStart(t, startup, at, false, nil)
	before := nha101RunCalendar(t, first, true)
	if first.runtime.bridge.engine != nil {
		t.Fatal("read-only Run created a trigger engine")
	}
	first.stop(t)
	block.Store(true)
	second := nha101RunStart(t, startup, at, false, nil)
	select {
	case <-entered:
	case <-time.After(8 * time.Second):
		t.Fatal("restart did not reuse durable conditional selection")
	}
	stale := nha101RunCalendar(t, second, false)
	if stale.ID() != before.ID() || nha101RunReady(second) {
		t.Fatal("restart reassigned identity or inherited Ready")
	}
	unblock()
	after := nha101RunCalendar(t, second, true)
	if after.ID() != before.ID() || conditional.Load() != 1 || requests.Load() != 2 {
		t.Fatal("304 restart changed stable source or duplicated fetch")
	}
	second.stop(t)
}

func nha101RunDenseICS(count int) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\n")
	for i := 0; i < count; i++ {
		fmt.Fprintf(&b, "BEGIN:VEVENT\r\nUID:dense-%d\r\nDTSTART:20261101T041000Z\r\nSUMMARY:dense\r\nEND:VEVENT\r\n", i)
	}
	b.WriteString("END:VCALENDAR\r\n")
	return b.String()
}
func TestNHA101Run040Global512Then513RejectsWholeSource(t *testing.T) {
	cluster := testpg.Start(t)
	var count atomic.Int32
	count.Store(212)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Date", "Sun, 01 Nov 2026 04:00:00 GMT")
		_, _ = w.Write([]byte(nha101RunDenseICS(int(count.Load()))))
	}))
	defer server.Close()
	startup := nha029RunStartupFromRaw(t, nha101RunRaw(t, cluster.Port, shortPrivateDataDir(t), server.URL))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var initial sync.Once
	publications := make(chan error, 2)
	h := nha101RunStart(t, startup, time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC), true, func(r *remoteCalendarRuntime) {
		r.bridge.beforeReplace = func() { initial.Do(func() { close(entered); <-release }) }
		r.bridge.afterReplace = func(err error) { publications <- err }
	})
	select {
	case <-entered:
	case <-time.After(8 * time.Second):
		t.Fatal("real cache212 did not reach040")
	}
	sourceA, _ := automationtriggers.ParseCalendarSourceID("nha101-other-source")
	now := h.clock.Now()
	items := make([]automationtriggers.CalendarOccurrence, 300)
	for i := range items {
		var err error
		items[i], err = automationtriggers.NewCalendarOccurrence(sourceA, fmt.Sprintf("other-%d", i), "origin", now.Add(time.Hour), now.Add(2*time.Hour), "UTC", false, domain.FreshnessFresh)
		if err != nil {
			t.Fatal(err)
		}
	}
	other, err := automationtriggers.NewCalendarWindow(sourceA, 1, now, now.Add(48*time.Hour), nil, items)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.runtime.bridge.engine.ReplaceCalendarWindow(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	unblock()
	nha101RunWait(t, func() bool { return nha101RunReady(h) })
	select {
	case err := <-publications:
		if err != nil {
			t.Fatal("real 040 rejected global 512", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("real 040 global 512 result absent")
	}
	calendar := nha101RunCalendar(t, h, true)
	changes, err := h.runtime.service.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer changes.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	count.Store(213)
	h.runtime.mu.Lock()
	var pilot *configuredRemoteCalendarPilot
	for p := range h.runtime.active {
		pilot = p
	}
	h.runtime.mu.Unlock()
	if pilot == nil || pilot.Refresh(ctx) != nil {
		t.Fatal("actual supervised Source could not refresh")
	}
	select {
	case err := <-publications:
		if err != automationtriggers.ErrCapacity {
			t.Fatal("real 040 global 513 did not refuse the whole source for capacity", err)
		}
	case <-ctx.Done():
		t.Fatal("real 040 global 513 result absent", ctx.Err())
	}
	// Ready closes before collection; process freshness becomes stale before
	// persistStale finishes its admitted cache read. Wait for the post-COMMIT
	// publication reason, whose notification follows that read's release.
	for {
		change, err := changes.Next(ctx)
		if err != nil {
			t.Fatal("durable 040 refusal notification absent", err)
		}
		if change.Kind() != calendars.CalendarUpdated || change.Calendar() != calendar.ID() {
			t.Fatal("unexpected remote cache notification")
		}
		current, err := h.runtime.service.GetCalendar(ctx, calendar.ID())
		if err != nil {
			t.Fatal(err)
		}
		if current.Revision() != change.Revision() {
			continue // Await the notification for the newer durable snapshot.
		}
		state, ok := current.RemoteState()
		if !ok {
			t.Fatal("remote cache state absent")
		}
		switch state.Reason() {
		case calendars.RemoteReasonNone:
			continue // The committed 213 representation can precede its refusal.
		case calendars.RemoteReasonPublication:
			freshness, known := state.Freshness()
			if !known || freshness != calendars.RemoteStale || nha101RunReady(h) {
				t.Fatal("durable 040 refusal kept the source fresh or Ready")
			}
		default:
			t.Fatal("remote cache degraded for a reason other than 040 publication")
		}
		calendar = current
		break
	}
	zone, _ := calendars.ParseZone("UTC")
	from, _ := calendars.NewInstant(now)
	to, _ := calendars.NewInstant(now.Add(48 * time.Hour))
	window, _ := calendars.TimeWindow(from, to, zone)
	page, err := h.runtime.service.Query(context.Background(), calendar.ID(), window, 1000, calendars.Cursor{})
	if err != nil || len(page.Items()) != 213 {
		t.Fatal("040 capacity refusal erased/truncated readable durable cache", err)
	}
	h.stop(t)
}

func TestNHA101DefaultRunActivationAndProtectedRestorePreserveColdManifest(t *testing.T) {
	cluster := testpg.Start(t)
	var aGets, bGets atomic.Int32
	serve := func(counter *atomic.Int32, uid string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			w.Header().Set("Cache-Control", "max-age=3600")
			w.Header().Set("Date", "Sun, 01 Nov 2026 04:00:00 GMT")
			_, _ = w.Write([]byte(strings.ReplaceAll(nha101RunPointDateICS, " point uid ", uid)))
		}))
	}
	a, b := serve(&aGets, "source-a"), serve(&bGets, "source-b")
	defer a.Close()
	defer b.Close()
	data, workspace := shortPrivateDataDir(t), t.TempDir()
	path, launchPath := filepath.Join(workspace, "configuration.yaml"), filepath.Join(workspace, "launch.yaml")
	initial := append(nha101RunRaw(t, cluster.Port, data, a.URL), []byte("scope_revision: v1\n")...)
	if err := os.WriteFile(path, initial, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launchPath, nha137LaunchFixture(cluster.Port, data, "ecobee_mvp_v1"), 0600); err != nil {
		t.Fatal(err)
	}
	choice, err := config.ExistingBootstrap(launchPath)
	if err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(path, choice, nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	h := nha101RunStart(t, startup, time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC), false, nil)
	before := nha101RunCalendar(t, h, true)
	if h.activation == nil {
		t.Fatal("default Run did not attach real029 owner")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	old, present, err := h.runtime.store.ReadActiveConfig(ctx)
	if err != nil || !present {
		t.Fatal("missing initial SQL generation", err)
	}
	var operational storage.ConfigVersionRef
	for _, version := range old.Versions() {
		kind, ok := version.Kind()
		if ok && kind == storage.ConfigDocumentKindOperationalYAML {
			operational = version.Ref()
		}
	}
	changed := nha101RunRaw(t, cluster.Port, data, b.URL)
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	published, err := h.activation.Activate(ctx, nha137OperationalRequest(t))
	if err != nil || published.Outcome() != configactivation.Published {
		t.Fatal("real029 activation failed", err)
	}
	nha101RunWait(t, func() bool { return bGets.Load() > 0 && nha101RunReady(h) })
	after := nha101RunCalendar(t, h, true)
	if after.ID() != before.ID() {
		t.Fatal("activation reassigned CalendarID")
	}
	secrets, err := h.runtime.store.BootstrapSecrets(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := storage.NewConfigReplayRepository(h.runtime.store, secrets)
	if err != nil {
		t.Fatal(err)
	}
	protected, err := replay.ReadHistoricalProtectedSource(ctx, old.Revision(), old.ManifestSHA256(), operational)
	if err != nil {
		t.Fatal("protected history unavailable", err)
	}
	raw := protected.Bytes()
	defer clear(raw)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	gets := aGets.Load()
	restored, err := h.activation.Activate(ctx, nha137OperationalRequest(t))
	if err != nil || restored.Outcome() != configactivation.Published {
		t.Fatal("protected source restore failed", err)
	}
	nha101RunWait(t, func() bool { return aGets.Load() > gets && nha101RunReady(h) })
	manifest, err := h.runtime.identity.ScopeManifest()
	if err != nil {
		t.Fatal(err)
	}
	revision, ok := manifest.Revision()
	if !ok || revision != scope.EcobeeMVPV1() {
		t.Fatal("historical operational v1 replaced current launch137 manifest")
	}
	if restored := nha101RunCalendar(t, h, true); restored.ID() != before.ID() {
		t.Fatal("restore reassigned retained calendar")
	}
	h.stop(t)
}

func TestNHA101RunOldWindowDisarmedAtNewAdmissionAndNoReplay(t *testing.T) {
	cluster := testpg.Start(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Date", "Sun, 01 Nov 2026 04:00:00 GMT")
		_, _ = w.Write([]byte(nha101RunPointDateICS))
	}))
	defer server.Close()
	startup := nha029RunStartupFromRaw(t, nha101RunRaw(t, cluster.Port, shortPrivateDataDir(t), server.URL))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	h := nha101RunStart(t, startup, time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC), true, func(r *remoteCalendarRuntime) {
		r.bridge.beforeReplace = func() {
			if calls.Add(1) == 2 {
				close(entered)
				<-release
			}
		}
	})
	nha101RunWait(t, func() bool { return nha101RunReady(h) })
	h.runtime.mu.Lock()
	var pilot *configuredRemoteCalendarPilot
	for p := range h.runtime.active {
		pilot = p
	}
	h.runtime.mu.Unlock()
	if pilot == nil || pilot.Refresh(context.Background()) != nil {
		t.Fatal("actual Source refresh unavailable")
	}
	select {
	case <-entered:
	case <-time.After(8 * time.Second):
		t.Fatal("new admission did not reach real040 publication barrier")
	}
	if nha101RunReady(h) {
		t.Fatal("new admission inherited Ready before its real040 replacement")
	}
	h.clock.advance(10 * time.Minute)
	// A real engine timer runs while Source is still blocked before Replace.
	// Its old point start/end must both remain disarmed by the new admission.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	for {
		result, err := h.results.NextResult(ctx)
		if err != nil {
			break
		}
		if result.Kind() == automationtriggers.ResultFired {
			cancel()
			t.Fatal("old window fired after a new Source admission")
		}
	}
	cancel()
	unblock()
	nha101RunWait(t, func() bool { return nha101RunReady(h) })
	ctx, cancel = context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	for {
		result, err := h.results.NextResult(ctx)
		if err != nil {
			break
		}
		if result.Kind() == automationtriggers.ResultFired {
			t.Fatal("fresh replacement replayed the elapsed point")
		}
	}
	h.stop(t)
}

func TestNHA101RunListenFailureJoinsBlockedHTTPAndService(t *testing.T) {
	cluster := testpg.Start(t)
	entered, cancelled := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(cancelled) }))
	defer server.Close()
	startup := nha029RunStartupFromRaw(t, nha101RunRaw(t, cluster.Port, shortPrivateDataDir(t), server.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	owners := make(chan *remoteCalendarRuntime, 1)
	refused := errors.New("synthetic listener preparation refusal")
	listen := func(ctx context.Context, _, _ string) (net.Listener, error) {
		select {
		case <-entered:
			return nil, refused
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	err := runWithDependencies(ctx, startup, listen, nil, nil, runDependencies{observeRemoteCalendar: func(owner *remoteCalendarRuntime) { owners <- owner }})
	if !errors.Is(err, refused) {
		t.Fatal("listener refusal was not preserved", err)
	}
	var owner *remoteCalendarRuntime
	select {
	case owner = <-owners:
	default:
		t.Fatal("real default owner absent")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("owned HTTP request did not observe cancellation")
	}
	select {
	case <-owner.Done():
	default:
		t.Fatal("Run error returned before actual Source/Service join")
	}
	// The real Service must reject later work after the drain, rather than
	// exposing a premature Done while its repository can still admit calls.
	if _, err := owner.service.ListCalendars(context.Background()); err == nil {
		t.Fatal("Service still admitted work after prepared-error drain")
	}
}
