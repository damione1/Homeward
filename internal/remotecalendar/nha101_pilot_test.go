package remotecalendar

import (
	"context"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type nha101Clock struct {
	wall time.Time
	mono time.Duration
}

func (c *nha101Clock) Now() time.Time                                    { return c.wall }
func (c *nha101Clock) MonoNow() time.Duration                            { return c.mono }
func (c *nha101Clock) NewTimer(d time.Duration) automationtriggers.Timer { return nil }
func TestNHA101PilotControlCoalescingAndStopAdmissions(t *testing.T) {
	if _, e := NewPilot(Source{}, (*nha101Clock)(nil)); e == nil {
		t.Fatal("zero/typednil pilot admitted")
	}
	var p *Pilot
	if p.Refresh(context.Background()) == nil {
		t.Fatal("nil pilot refresh admitted")
	}
	if _, e := WithAutomationPublication((*nha101Publisher)(nil)); e == nil {
		t.Fatal("typednil publisher admitted")
	}
}

type nha101Publisher struct{}

func (*nha101Publisher) Replace(context.Context, PublicationWindow) error      { return nil }
func (*nha101Publisher) Degrade(context.Context, calendars.RemoteReason) error { return nil }
func TestNHA101PilotCadence12h24hAndCollisionPriority(t *testing.T) {
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	c := cadence{}
	c.pollSucceeded(now, 0, now.Add(24*time.Hour))
	c.windowSucceeded(now, 0)
	if got := c.due(now.Add(12*time.Hour), 12*time.Hour, true); got != intentRenewWindow {
		t.Fatal("12h must renew locally", got)
	}
	c.windowSucceeded(now.Add(12*time.Hour), 12*time.Hour)
	if got := c.due(now.Add(24*time.Hour), 24*time.Hour, true); got != intentRefresh {
		t.Fatal("24h HTTP must win collision", got)
	}
	if !c.expired(now.Add(24*time.Hour), 24*time.Hour) {
		t.Fatal("expiry must close admission before due work")
	}
	c.pollSucceeded(now, 0, now)
	if c.due(now, 0, false) != intentNone {
		t.Fatal("no-cache caused immediate spin")
	}
}

// Real SQL/CloudClient/Supervisor fixture. The publisher records composition
// only: it is deliberately not evidence for the future040 engine or Run.
type nha101LiveClock struct {
	mu     sync.Mutex
	wall   time.Time
	mono   time.Duration
	timers map[*nha101LiveTimer]bool
}
type nha101LiveTimer struct {
	clock *nha101LiveClock
	at    time.Duration
	ch    chan time.Time
}

func (c *nha101LiveClock) Now() time.Time         { c.mu.Lock(); defer c.mu.Unlock(); return c.wall }
func (c *nha101LiveClock) MonoNow() time.Duration { c.mu.Lock(); defer c.mu.Unlock(); return c.mono }
func (c *nha101LiveClock) NewTimer(d time.Duration) automationtriggers.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	x := &nha101LiveTimer{clock: c, at: c.mono + d, ch: make(chan time.Time, 1)}
	if c.timers == nil {
		c.timers = make(map[*nha101LiveTimer]bool)
	}
	c.timers[x] = true
	if d <= 0 {
		x.ch <- c.wall
		delete(c.timers, x)
	}
	return x
}
func (t *nha101LiveTimer) C() <-chan time.Time { return t.ch }
func (t *nha101LiveTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	live := t.clock.timers[t]
	delete(t.clock.timers, t)
	return live
}
func (c *nha101LiveClock) advance(wall, mono time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(wall)
	c.mono += mono
	for timer := range c.timers {
		if timer.at <= c.mono {
			timer.ch <- c.wall
			delete(c.timers, timer)
		}
	}
}

type nha101CloudTimer struct{ clock *nha101LiveClock }

func (t nha101CloudTimer) NewTimer(d time.Duration) integrations.CloudWait {
	return t.clock.NewTimer(d)
}

type nha101Phase struct {
	active         atomic.Bool
	mutations      atomic.Int32
	reads          atomic.Int32
	overlap        atomic.Bool
	mu             sync.Mutex
	scope          storage.RemoteCalendarScope
	beforeMutation func()
}
type nha101MeasuredWriter struct {
	calendars.RemoteWriter
	phase *nha101Phase
}

func (w nha101MeasuredWriter) Replace(ctx context.Context, rev calendars.Revision, o storage.RemoteCalendarObservation, body []byte, z *calendars.Zone) (storage.RemoteCalendarCommit, error) {
	w.phase.active.Store(true)
	defer w.phase.active.Store(false)
	w.phase.mutations.Add(1)
	if w.phase.beforeMutation != nil {
		w.phase.beforeMutation()
	}
	return w.RemoteWriter.Replace(ctx, rev, o, body, z)
}

type nha101WriterPort struct {
	service *calendars.Service
	id      calendars.CalendarID
	phase   *nha101Phase
}

func (w nha101WriterPort) Open(ctx context.Context, s calendars.RemoteSource) (calendars.RemoteWriter, error) {
	w.phase.mu.Lock()
	w.phase.scope = s.Scope()
	w.phase.mu.Unlock()
	writer, e := w.service.ConfigureRemoteSource(ctx, w.id, s)
	if e != nil {
		return nil, e
	}
	return nha101MeasuredWriter{writer, w.phase}, nil
}

type nha101ReaderPort struct {
	service *calendars.Service
	after   func(calendars.AutomationWindow)
	phase   *nha101Phase
}

func (r nha101ReaderPort) CollectRemoteWindow(ctx context.Context, id calendars.CalendarID, q calendars.AutomationWindowRequest) (calendars.AutomationWindow, error) {
	if r.phase.active.Load() {
		r.phase.overlap.Store(true)
	}
	r.phase.reads.Add(1)
	w, e := r.service.CollectRemoteWindow(ctx, id, q)
	if e == nil && r.after != nil {
		r.after(w)
	}
	return w, e
}
func (r nha101ReaderPort) CheckRemoteWindow(ctx context.Context, w calendars.AutomationWindow) error {
	return r.service.CheckRemoteWindow(ctx, w)
}

type nha101RecordingPublisher struct {
	mu           sync.Mutex
	windows      []calendars.AutomationWindow
	publications []PublicationWindow
	degraded     int
	reject       bool
	afterReplace func()
}

func (p *nha101RecordingPublisher) Replace(ctx context.Context, publication PublicationWindow) error {
	w := publication.Window()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reject {
		return failure(PublicationFailed)
	}
	p.windows = append(p.windows, w)
	p.publications = append(p.publications, publication)
	if p.afterReplace != nil {
		p.afterReplace()
	}
	return nil
}
func (p *nha101RecordingPublisher) Degrade(context.Context, calendars.RemoteReason) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.degraded++
	return nil
}
func (p *nha101RecordingPublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.windows)
}
func (p *nha101RecordingPublisher) last() calendars.AutomationWindow {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.windows[len(p.windows)-1]
}
func nha101Eventually(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if fn() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("real source did not reach expected boundary")
		case <-tick.C:
		}
	}
}

type nha101LiveRig struct {
	ctx      context.Context
	store    *storage.Store
	manifest registry.Manifest
	instance domain.IntegrationInstanceID
	key      integrations.SourceKey
	entity   domain.EntityID
	clock    *nha101LiveClock
	private  string
	phase    *nha101Phase
	resource *integrations.ConditionalResource
	auth     *integrations.ConditionalAuth
	disabled bool
}

func nha101LiveSetup(t *testing.T) nha101LiveRig {
	t.Helper()
	cluster := testpg.Start(t)
	settings, e := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(n string) (string, bool) { return testpg.Password(), n == "HOMEWARD_TEST_PASSWORD" })
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	store, e := storage.Open(ctx, settings)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(store.Close)
	version, e := store.Migrate(ctx)
	if e != nil || version.Number() < 23 {
		t.Fatal("requires real owner migration23, no ad hoc schema", e)
	}
	raw, e := os.ReadFile("../../scope/manifest.json")
	if e != nil {
		t.Fatal(e)
	}
	manifest, e := registry.ParseManifest(raw)
	if e != nil {
		t.Fatal(e)
	}
	repo, e := registry.NewRepository(store, manifest)
	if e != nil {
		t.Fatal(e)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha101-t2-real-source")
	key, _ := integrations.NewSourceKey("remote-family", "calendar")
	bound, e := repo.ConfigureBinding(ctx, instance, key, "calendar.famille")
	if e != nil {
		t.Fatal(e)
	}
	return nha101LiveRig{ctx: ctx, store: store, manifest: manifest, instance: instance, key: key, entity: bound.Entity, clock: &nha101LiveClock{wall: time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)}, private: filepath.Join(t.TempDir(), "private"), phase: &nha101Phase{}}
}
func (f nha101LiveRig) start(t *testing.T, url string, pub *nha101RecordingPublisher, after func(calendars.AutomationWindow)) (*Pilot, *integrations.Supervisor, *calendars.Service, *fake.Fixture) {
	t.Helper()
	repo, e := calendars.NewRepositoryWithClock(f.store, f.manifest, f.clock)
	if e != nil {
		t.Fatal(e)
	}
	service, e := calendars.NewService(repo)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(service.Close)
	id, e := service.Bind(f.ctx, f.entity)
	if e != nil {
		t.Fatal(e)
	}
	host, fixture, e := fake.NewCommandFixture()
	if e != nil {
		t.Fatal(e)
	}
	secrets, e := f.store.BootstrapSecrets(f.ctx, f.private)
	if e != nil {
		t.Fatal(e)
	}
	if e = host.AttachSecrets(secrets); e != nil {
		t.Fatal(e)
	}
	resource, e := integrations.PublicConditionalResource(url + "/calendar?exact=1&exact=2")
	if e != nil {
		t.Fatal(e)
	}
	destination, e := integrations.ExplicitConditionalDestination(url, []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})
	if e != nil {
		t.Fatal(e)
	}
	zone, _ := calendars.ParseZone("UTC")
	if f.resource != nil {
		resource = *f.resource
	}
	auth := integrations.NoConditionalAuth()
	if f.auth != nil {
		auth = *f.auth
	}
	settings, e := NewSettings(f.instance, f.key, "Synthetic family", resource, auth, destination, NoFloatingProjection(), zone)
	if e != nil {
		t.Fatal(e)
	}
	policy, e := integrations.NewCloudPolicy(integrations.DefaultCloudLimits(), nil, f.clock, nha101CloudTimer{f.clock}, integrations.RandomJitter{})
	if e != nil {
		t.Fatal(e)
	}
	publication, e := WithAutomationPublication(pub)
	if e != nil {
		t.Fatal(e)
	}
	if f.disabled {
		publication = WithoutAutomationPublication()
	}
	source, e := NewSource(settings, id, nha101WriterPort{service, id, f.phase}, nha101ReaderPort{service, after, f.phase}, publication, policy)
	if e != nil {
		t.Fatal(e)
	}
	pilot, e := NewPilot(source, f.clock)
	if e != nil {
		t.Fatal(e)
	}
	spec, e := integrations.NewPilotSpec(f.instance, func() integrations.Pilot { return pilot })
	if e != nil {
		t.Fatal(e)
	}
	retry, _ := integrations.NewRetryPolicy(0, time.Millisecond, time.Second, 3*time.Second)
	supervisor, e := integrations.NewSupervisor(host, []integrations.PilotSpec{spec}, retry, integrations.RealTimer{}, integrations.RandomJitter{})
	if e != nil {
		t.Fatal(e)
	}
	if e = supervisor.Start(f.ctx); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if e := supervisor.Stop(ctx); e != nil {
			t.Error(e)
		}
		select {
		case <-supervisor.Done():
		default:
			t.Error("Supervisor tasks not joined")
		}
	})
	return pilot, supervisor, service, fixture
}
func nha101Idle(t *testing.T, p *Pilot) {
	t.Helper()
	nha101Eventually(t, func() bool {
		p.mu.Lock()
		run := p.run
		p.mu.Unlock()
		if run == nil {
			return false
		}
		run.mu.Lock()
		defer run.mu.Unlock()
		return run.workerCancel == nil && run.pending == intentNone && run.commit == nil && run.fresh
	})
}
func TestNHA101PilotSQLConditionalRestartAndInvalidSource(t *testing.T) {
	f := nha101LiveSetup(t)
	var gets atomic.Int32
	var mode atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		if r.Method != "GET" {
			t.Error("readonly source sent mutation")
		}
		w.Header().Set("Etag", `"stable"`)
		if mode.Load() == 1 {
			_, _ = w.Write([]byte("invalid ICS"))
			return
		}
		if r.Header.Get("If-None-Match") == `"stable"` {
			w.WriteHeader(304)
			return
		}
		_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID: stable uid \r\nDTSTART:20261019T120000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	}))
	defer server.Close()
	pub := &nha101RecordingPublisher{}
	pilot, supervisor, service, fixture := f.start(t, server.URL, pub, nil)
	nha101Eventually(t, func() bool { return pub.count() == 1 })
	nha101Idle(t, pilot)
	nha101Eventually(t, func() bool {
		h, ok := fixture.Health(f.instance)
		if !ok {
			return false
		}
		status, _, e := h.Components()
		return e == nil && status == integrations.HealthReady
	})
	first := pub.last()
	before := gets.Load()
	f.clock.advance(12*time.Hour, 12*time.Hour)
	nha101Eventually(t, func() bool { return pub.count() == 2 })
	nha101Idle(t, pilot)
	second := pub.last()
	if gets.Load() != before || versionOf(first) != versionOf(second) {
		t.Fatal("12h renewal changed representation or performed GET")
	}
	a, _, _ := first.Bounds()
	b, _, _ := second.Bounds()
	if !b.After(a) {
		t.Fatal("12h did not renew horizon")
	}
	f.clock.advance(12*time.Hour, 12*time.Hour)
	nha101Eventually(t, func() bool { return gets.Load() > before && pub.count() >= 3 })
	if pub.last().Revision() == second.Revision() {
		t.Fatal("24h did not perform durable revalidation")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if e := supervisor.Stop(stopCtx); e != nil {
		t.Fatal(e)
	}
	if pilot.Refresh(f.ctx) == nil {
		t.Fatal("refresh admitted after stop")
	}
	id := first.Calendar()
	snapshot, e := service.GetCalendar(f.ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	state, _ := snapshot.RemoteState()
	fresh, _ := state.Freshness()
	if fresh != calendars.RemoteStale {
		t.Fatal("stopped process remains fresh")
	}
	pub2 := &nha101RecordingPublisher{}
	next, _, service2, _ := f.start(t, server.URL, pub2, nil)
	nha101Eventually(t, func() bool { return pub2.count() == 1 })
	if versionOf(pub2.last()).digest != versionOf(first).digest {
		t.Fatal("restart changed durable representation")
	}
	mode.Store(1)
	if e = next.Refresh(f.ctx); e != nil {
		t.Fatal(e)
	}
	nha101Eventually(t, func() bool {
		snap, e := service2.GetCalendar(f.ctx, id)
		if e != nil {
			return false
		}
		s, _ := snap.RemoteState()
		q, _ := s.Freshness()
		return q == calendars.RemoteStale
	})
	if pub2.count() != 1 {
		t.Fatal("invalid source replaced admitted window")
	}
}

func TestNHA101PilotSQLExpiryAndJoinAtBoundaries(t *testing.T) {
	f := nha101LiveSetup(t)
	var gets atomic.Int32
	blocked := make(chan struct{}, 8)
	joined := make(chan struct{}, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gets.Add(1) > 1 {
			blocked <- struct{}{}
			<-r.Context().Done()
			joined <- struct{}{}
			return
		}
		w.Header().Set("Cache-Control", "max-age=3600")
		_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n"))
	}))
	defer server.Close()
	pub := &nha101RecordingPublisher{}
	pilot, supervisor, service, fixture := f.start(t, server.URL, pub, nil)
	nha101Eventually(t, func() bool { return pub.count() == 1 })
	first := pub.last()
	if e := pilot.Refresh(f.ctx); e != nil {
		t.Fatal(e)
	}
	select {
	case <-blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("GET did not block")
	}
	f.clock.advance(2*time.Hour, 2*time.Hour)
	nha101Eventually(t, func() bool {
		snapshot, e := service.GetCalendar(f.ctx, first.Calendar())
		if e != nil {
			return false
		}
		s, _ := snapshot.RemoteState()
		q, _ := s.Freshness()
		return q == calendars.RemoteStale
	})
	select {
	case <-joined:
	case <-time.After(3 * time.Second):
		t.Fatal("expired HTTP not canceled")
	}
	pilot.run.mu.Lock()
	pilot.run.mu.Unlock()
	h, ok := fixture.Health(f.instance)
	if ok {
		status, _, e := h.Components()
		if e != nil || status == integrations.HealthReady {
			t.Fatal("expiry left Ready")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if e := supervisor.Stop(ctx); e != nil {
		t.Fatal(e)
	}
	select {
	case <-pilot.run.done:
	default:
		t.Fatal("pilot Tasks not really joined")
	}
	if pub.count() != 1 {
		t.Fatal("late expired window published")
	}
}
func TestNHA101PilotSQLNoCacheDoesNotSpin(t *testing.T) {
	f := nha101LiveSetup(t)
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n"))
	}))
	defer server.Close()
	pub := &nha101RecordingPublisher{}
	pilot, _, service, fixture := f.start(t, server.URL, pub, nil)
	nha101Eventually(t, func() bool {
		snapshot, e := service.GetCalendar(f.ctx, pilot.source.id)
		if e != nil {
			return false
		}
		s, _ := snapshot.RemoteState()
		return s.Kind() == calendars.RemoteValidated && s.Reason() == calendars.RemoteReasonExpired
	})
	if pub.count() != 0 {
		t.Fatal("no-cache published fresh window")
	}
	for i := 0; i < 3; i++ {
		f.clock.advance(time.Second, time.Second)
	}
	if gets.Load() != 1 {
		t.Fatal("no-cache spun HTTP requests")
	}
	if h, ok := fixture.Health(f.instance); ok {
		status, _, _ := h.Components()
		if status == integrations.HealthReady {
			t.Fatal("no-cache became Ready")
		}
	}
}

func TestNHA101PilotSQLSecretRotationCASAndPublicationFence(t *testing.T) {
	f := nha101LiveSetup(t)
	var gets atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := gets.Add(1)
		if n == 2 && r.Header.Get("If-None-Match") != "" {
			t.Error("rotation reused old validators")
		}
		if n == 2 && (r.Header.Get("Authorization") != "Bearer NHA101_AUTH_TWO" || r.URL.RawQuery != "token=NHA101_URL_TWO") {
			t.Error("rotated resource/auth not used")
		}
		if n == 3 {
			entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Etag", `"rotation"`)
		_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n"))
	}))
	defer server.Close()
	secrets, e := f.store.BootstrapSecrets(f.ctx, f.private)
	if e != nil {
		t.Fatal(e)
	}
	name, _ := storage.ParseSecretName("calendar-url")
	handle, _ := storage.NewSecretHandle(f.instance, name)
	tokenName, _ := storage.ParseSecretName("calendar-token")
	token, _ := storage.NewSecretHandle(f.instance, tokenName)
	rotate := func(suffix string) {
		t.Helper()
		value, e := storage.NewSecretValue([]byte(server.URL + "/secret?token=NHA101_URL_" + suffix))
		if e != nil {
			t.Fatal(e)
		}
		if e = secrets.RotateCredential(f.ctx, handle, value); e != nil {
			t.Fatal(e)
		}
		value, e = storage.NewSecretValue([]byte("NHA101_AUTH_" + suffix))
		if e != nil {
			t.Fatal(e)
		}
		if e = secrets.RotateCredential(f.ctx, token, value); e != nil {
			t.Fatal(e)
		}
	}
	rotate("ONE")
	resource, e := integrations.SecretConditionalResource(handle)
	if e != nil {
		t.Fatal(e)
	}
	auth, e := integrations.BearerConditionalAuth(token)
	if e != nil {
		t.Fatal(e)
	}
	f.resource = &resource
	f.auth = &auth
	pub := &nha101RecordingPublisher{}
	pilot, _, service, fixture := f.start(t, server.URL, pub, nil)
	nha101Eventually(t, func() bool { return pub.count() == 1 })
	nha101Idle(t, pilot)
	first := versionOf(pub.last())
	rotate("TWO")
	if e = pilot.Refresh(f.ctx); e != nil {
		t.Fatal(e)
	}
	nha101Eventually(t, func() bool { return pub.count() == 2 })
	nha101Idle(t, pilot)
	second := versionOf(pub.last())
	if first.scope == second.scope || first.digest != second.digest {
		t.Fatal("authenticated rotation did not change scope or changed logical representation")
	}
	if e = pilot.Refresh(f.ctx); e != nil {
		t.Fatal(e)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("rotation barrier not reached")
	}
	rotate("THREE")
	close(release)
	nha101Eventually(t, func() bool {
		head, e := service.GetCalendar(f.ctx, pilot.source.id)
		if e != nil {
			return false
		}
		s, _ := head.RemoteState()
		q, _ := s.Freshness()
		return q == calendars.RemoteStale
	})
	if pub.count() != 2 {
		t.Fatal("old credentials response published")
	}
	if h, ok := fixture.Health(f.instance); ok {
		status, _, _ := h.Components()
		if status == integrations.HealthReady {
			t.Fatal("credential CAS refusal left Ready")
		}
	}
}

// A deferred server error exercises the real Store's indeterminate return;
// it is not a claim of durable COMMIT followed by wire ACK loss (S1 oracle).
func TestNHA101PilotSQLServerDeferredCommitUnknown(t *testing.T) {
	f := nha101LiveSetup(t)
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n"))
	}))
	defer server.Close()
	pub := &nha101RecordingPublisher{}
	pilot, _, service, fixture := f.start(t, server.URL, pub, nil)
	nha101Eventually(t, func() bool { return pub.count() == 1 })
	nha101Idle(t, pilot)
	old := pub.last().Revision()
	e := f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
		_, e := tx.Exec(f.ctx, `CREATE FUNCTION public.nha101_t2_commit_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic deferred failure'; END $$;
 CREATE CONSTRAINT TRIGGER nha101_t2_commit_failure AFTER UPDATE ON public.remote_calendar_cache DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha101_t2_commit_failure()`)
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
	if e = pilot.Refresh(f.ctx); e != nil {
		t.Fatal(e)
	}
	nha101Eventually(t, func() bool {
		if gets.Load() < 2 {
			return false
		}
		head, e := service.GetCalendar(f.ctx, pilot.source.id)
		if e != nil {
			return false
		}
		s, _ := head.RemoteState()
		q, _ := s.Freshness()
		return q == calendars.RemoteStale
	})
	head, e := service.GetCalendar(f.ctx, pilot.source.id)
	if e != nil {
		t.Fatal(e)
	}
	if head.Revision() != old || pub.count() != 1 {
		t.Fatal("unknown commit published or replaced known snapshot")
	}
	nha101Eventually(t, func() bool {
		pilot.run.mu.Lock()
		defer pilot.run.mu.Unlock()
		return pilot.run.lastFailure != nil && pilot.run.lastFailure.Kind() == CommitUnknown
	})
	pilot.run.mu.Lock()
	diagnostic := pilot.run.lastFailure
	pilot.run.mu.Unlock()
	if diagnostic == nil || diagnostic.Kind() != CommitUnknown {
		t.Fatal("unknown commit lost precise Source diagnostic")
	}
	if h, ok := fixture.Health(f.instance); ok {
		status, _, _ := h.Components()
		if status == integrations.HealthReady {
			t.Fatal("unknown commit left Ready")
		}
	}
}

func TestNHA101PilotSQLDisabledClockRollbackBeforeCommit(t *testing.T) {
	f := nha101LiveSetup(t)
	f.disabled = true
	f.phase.beforeMutation = func() { f.clock.mu.Lock(); f.clock.wall = f.clock.wall.Add(-time.Hour); f.clock.mu.Unlock() }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n"))
	}))
	defer server.Close()
	pub := &nha101RecordingPublisher{}
	pilot, _, service, fixture := f.start(t, server.URL, pub, nil)
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
	head, e := service.GetCalendar(f.ctx, pilot.source.id)
	if e != nil {
		t.Fatal(e)
	}
	state, _ := head.RemoteState()
	q, _ := state.Freshness()
	if state.Kind() != calendars.RemoteValidated || q != calendars.RemoteStale {
		t.Fatal("clock rollback lost known snapshot or fresh guard")
	}
	if pub.count() != 0 || f.phase.reads.Load() != 0 {
		t.Fatal("disabled publication opened automation collector")
	}
	if h, ok := fixture.Health(f.instance); ok {
		status, _, _ := h.Components()
		if status == integrations.HealthReady {
			t.Fatal("rollback between observation and commit confirmed Ready")
		}
	}
}

func TestNHA101PilotSQLRollbackJustBeforeConfirmRetiresObservation(t *testing.T) {
	f := nha101LiveSetup(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n"))
	}))
	defer server.Close()
	pub := &nha101RecordingPublisher{afterReplace: func() { f.clock.mu.Lock(); f.clock.wall = f.clock.wall.Add(-time.Hour); f.clock.mu.Unlock() }}
	pilot, _, service, fixture := f.start(t, server.URL, pub, nil)
	nha101Eventually(t, func() bool {
		pilot.mu.Lock()
		run := pilot.run
		pilot.mu.Unlock()
		if run == nil {
			return false
		}
		run.mu.Lock()
		defer run.mu.Unlock()
		return run.lastFailure != nil && run.lastFailure.Kind() == PublicationFailed
	})
	head, e := service.GetCalendar(f.ctx, pilot.source.id)
	if e != nil {
		t.Fatal(e)
	}
	state, _ := head.RemoteState()
	q, _ := state.Freshness()
	if q != calendars.RemoteStale {
		t.Fatal("regressed final confirmation left fresh")
	}
	pilot.run.mu.Lock()
	retired := pilot.run.commit
	pilot.run.mu.Unlock()
	if retired == nil {
		t.Fatal("expected retired confirmation candidate")
	}
	if _, e = retired.observation.ObservationTime(); e == nil {
		t.Fatal("retired observation exposed scalar")
	}
	if e = pilot.run.http.ConfirmConditionalAdmitted(f.ctx, retired.observation, retired.receipt); e == nil {
		t.Fatal("retired receipt rearmed Ready")
	}
	if h, ok := fixture.Health(f.instance); ok {
		status, _, _ := h.Components()
		if status == integrations.HealthReady {
			t.Fatal("rollback at Confirm became Ready")
		}
	}
}

func TestNHA101PilotSQLInitialClockCancelStillPolls24h(t *testing.T) {
	f := nha101LiveSetup(t)
	var gets atomic.Int32
	blocked := make(chan struct{}, 1)
	joined := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gets.Add(1) == 1 {
			blocked <- struct{}{}
			<-r.Context().Done()
			joined <- struct{}{}
			return
		}
		_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n"))
	}))
	defer server.Close()
	pub := &nha101RecordingPublisher{}
	pilot, _, _, _ := f.start(t, server.URL, pub, nil)
	select {
	case <-blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("initial HTTP not reached")
	}
	f.clock.advance(-time.Hour, time.Second)
	select {
	case <-joined:
	case <-time.After(3 * time.Second):
		t.Fatal("initial clock jump did not cancel HTTP")
	}
	nha101Eventually(t, func() bool {
		pilot.run.mu.Lock()
		defer pilot.run.mu.Unlock()
		return pilot.run.workerCancel == nil && pilot.run.pending == intentNone
	})
	if pub.count() != 0 || gets.Load() != 1 {
		t.Fatal("first cancellation unexpectedly retried or published")
	}
	f.clock.advance(24*time.Hour, 24*time.Hour)
	nha101Eventually(t, func() bool { return gets.Load() == 2 && pub.count() == 1 })
}
