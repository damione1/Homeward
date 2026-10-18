package view_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	statewire "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/view"
	"google.golang.org/protobuf/proto"
)

type watchClock struct {
	mu   sync.Mutex
	wall time.Time
	tick time.Duration
}

func (c *watchClock) Now() time.Time         { c.mu.Lock(); defer c.mu.Unlock(); return c.wall }
func (c *watchClock) MonoNow() time.Duration { c.mu.Lock(); defer c.mu.Unlock(); return c.tick }
func (c *watchClock) Advance(mono, wall time.Duration) {
	c.mu.Lock()
	c.tick += mono
	c.wall = c.wall.Add(wall)
	c.mu.Unlock()
}

type watchAuthority struct {
	mu                sync.Mutex
	grant             view.Grant
	otherGrants       map[view.ActorID]view.Grant
	config            view.ConfigStamp
	next              int
	grants            map[int]chan struct{}
	configs           map[int]chan struct{}
	reads             int
	onRead            func(int)
	onSubscribeConfig func()
}

func (a *watchAuthority) CurrentGrant(_ context.Context, actor view.ActorID) (view.Grant, error) {
	a.mu.Lock()
	a.reads++
	n, g, hook := a.reads, a.grant, a.onRead
	if other, ok := a.otherGrants[actor]; ok {
		g = other
	}
	a.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	return g, nil
}
func (a *watchAuthority) CurrentConfig(context.Context) (view.ConfigStamp, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.config, nil
}
func (a *watchAuthority) SubscribeGrantChanges(_ context.Context, _ view.ActorID) (<-chan struct{}, func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.next++
	id := a.next
	ch := make(chan struct{}, 1)
	a.grants[id] = ch
	return ch, func() { a.mu.Lock(); delete(a.grants, id); a.mu.Unlock() }, nil
}
func (a *watchAuthority) SubscribeConfigChanges(context.Context) (<-chan struct{}, func(), error) {
	a.mu.Lock()
	a.next++
	id := a.next
	ch := make(chan struct{}, 1)
	a.configs[id] = ch
	hook := a.onSubscribeConfig
	a.mu.Unlock()
	if hook != nil {
		hook()
	}
	return ch, func() { a.mu.Lock(); delete(a.configs, id); a.mu.Unlock() }, nil
}
func (a *watchAuthority) SetGrant(g view.Grant) {
	a.mu.Lock()
	a.grant = g
	for _, ch := range a.grants {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	a.mu.Unlock()
}
func (a *watchAuthority) SetConfig(c view.ConfigStamp) {
	a.mu.Lock()
	a.config = c
	for _, ch := range a.configs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	a.mu.Unlock()
}

type watchFixture struct {
	clock    *watchClock
	seq      *sequencer.Sequencer
	session  *integrations.Session
	bindings []integrations.Binding
	ids      []domain.EntityID
	actor    view.ActorID
	auth     *watchAuthority
	service  *view.Service
}

func newWatchFixture(t *testing.T, count int) *watchFixture {
	t.Helper()
	ctx := context.Background()
	clock := &watchClock{wall: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	seq := fixture.Sequencer()
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(fixture, &projectedSink{sequence: seq}, catalog)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("fixture-watch")
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]integrations.SourceKey, count)
	candidates := make([]integrations.Candidate, count)
	for i := 0; i < count; i++ {
		keys[i], _ = integrations.NewSourceKey("device-"+string(rune('a'+i)), "relay")
		candidates[i] = testCandidate(t, keys[i], "Read only relay")
	}
	bindings, err := session.Discover(ctx, candidates)
	if err != nil {
		t.Fatal(err)
	}
	identities, err := fixture.Resolve(ctx, instance, keys)
	if err != nil {
		t.Fatal(err)
	}
	records := make([]integrations.RegistryRecord, count)
	ids := make([]domain.EntityID, count)
	for i, identity := range identities {
		ids[i] = identity.Entity
		alias, _ := domain.ParseHAAlias("switch.fixture_" + string(rune('a'+i)))
		records[i], err = integrations.NewRegistryRecord(identity.Device, identity.Entity, instance, keys[i], alias, alias, []domain.HAAlias{alias}, false)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := seq.PublishRegistry(ctx, records); err != nil {
		t.Fatal(err)
	}
	actor := testActor(t, "watch-user")
	auth := &watchAuthority{grant: testGrant(t, actor, 1, ids), config: testConfig(t, 1, "config-one"), grants: make(map[int]chan struct{}), configs: make(map[int]chan struct{})}
	service, err := view.NewService(seq, auth, auth, clock)
	if err != nil {
		t.Fatal(err)
	}
	return &watchFixture{clock, seq, session, bindings, ids, actor, auth, service}
}
func (f *watchFixture) page(t *testing.T, filter view.Filter) *statewire.OpenViewResponse {
	t.Helper()
	page, err := f.service.OpenView(context.Background(), f.actor, filter, 1)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := view.EncodePage(page)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}
func (f *watchFixture) publish(t *testing.T, index int, on bool) {
	t.Helper()
	if err := f.publishError(index, on); err != nil {
		t.Fatal(err)
	}
}
func (f *watchFixture) publishError(index int, on bool) error {
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	value := domain.NewSwitchValue(on)
	at := f.clock.Now()
	observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		return err
	}
	return f.session.Publish(context.Background(), f.bindings[index], observation)
}
func watchRequest(cursor *statewire.ViewCursor, ids ...domain.EntityID) *statewire.WatchViewRequest {
	filter := &statewire.ViewFilter{}
	for _, id := range ids {
		value, _ := id.Value()
		filter.EntityIds = append(filter.EntityIds, value)
	}
	return &statewire.WatchViewRequest{Cursor: proto.Clone(cursor).(*statewire.ViewCursor), Filter: filter}
}
func openWatch(t *testing.T, f *watchFixture, cursor *statewire.ViewCursor, ids ...domain.EntityID) *view.Watch {
	t.Helper()
	w, err := f.service.WatchView(context.Background(), f.actor, watchRequest(cursor, ids...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}
func nextFrame(t *testing.T, w *view.Watch) *statewire.WatchViewResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	frame, err := w.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}
func requireResync(t *testing.T, frame *statewire.WatchViewResponse, reason statewire.ResyncReason) {
	t.Helper()
	if frame.GetResyncRequired() == nil || frame.GetResyncRequired().Reason != reason || frame.GetDelta() != nil || frame.Cursor != nil {
		t.Fatalf("resync=%v, want %v", frame, reason)
	}
}

func TestNHA021ReplayFilteringAndConcurrentLive(t *testing.T) {
	f := newWatchFixture(t, 2)
	f.auth.SetGrant(testGrant(t, f.actor, 2, []domain.EntityID{f.ids[0]}))
	page := f.page(t, view.All())
	f.publish(t, 1, true) // denied entity is present in journal, never in a frame
	f.publish(t, 0, true)
	w := openWatch(t, f, page.Cursor)
	first := nextFrame(t, w)
	if first.StreamSequence != 1 || len(first.GetDelta().States) != 1 || first.GetDelta().States[0].GetEntity().GetEntityId() != mustID(t, f.ids[0]) || first.Cursor.GlobalSequence != page.Cursor.GlobalSequence+2 {
		t.Fatalf("filtered replay: %v", first)
	}
	f.publish(t, 1, false)
	f.publish(t, 0, false)
	second := nextFrame(t, w)
	if second.StreamSequence != 2 || second.Cursor.GlobalSequence != first.Cursor.GlobalSequence+2 || second.GetDelta().States[0].GetEntity().GetEntityId() != mustID(t, f.ids[0]) {
		t.Fatalf("denied live entity leaked: %v", second)
	}
	const concurrent = 150
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < concurrent; i++ {
			f.publish(t, 0, i%2 == 0)
		}
	}()
	last := second.Cursor.GlobalSequence
	for i := 0; i < concurrent; i++ {
		frame := nextFrame(t, w)
		if frame.StreamSequence != uint64(i+3) || frame.Cursor.GlobalSequence != last+1 || len(frame.GetDelta().States) != 1 {
			t.Fatalf("replay/live frame %d: %v", i, frame)
		}
		last = frame.Cursor.GlobalSequence
	}
	<-done
	if last != page.Cursor.GlobalSequence+4+concurrent {
		t.Fatalf("lost or duplicated publications: %d", last)
	}
}

func TestNHA021ExactFilterOnReplayAndLive(t *testing.T) {
	f := newWatchFixture(t, 2) // grant contains A and B
	filter, _ := view.Only([]domain.EntityID{f.ids[0]})
	page := f.page(t, filter)
	f.publish(t, 1, true)
	f.publish(t, 0, true)
	w := openWatch(t, f, page.Cursor, f.ids[0])
	first := nextFrame(t, w)
	if first.StreamSequence != 1 || first.Cursor.GlobalSequence != page.Cursor.GlobalSequence+2 || first.GetDelta().States[0].GetEntity().GetEntityId() != mustID(t, f.ids[0]) {
		t.Fatalf("filter replay: %v", first)
	}
	f.publish(t, 1, false)
	f.publish(t, 0, false)
	second := nextFrame(t, w)
	if second.StreamSequence != 2 || second.Cursor.GlobalSequence != page.Cursor.GlobalSequence+4 || second.GetDelta().States[0].GetEntity().GetEntityId() != mustID(t, f.ids[0]) {
		t.Fatalf("filter live: %v", second)
	}
}
func mustID(t *testing.T, id domain.EntityID) string {
	t.Helper()
	v, e := id.Value()
	if e != nil {
		t.Fatal(e)
	}
	return v
}

func TestNHA021AtomicReplayLiveSeam(t *testing.T) {
	f := newWatchFixture(t, 1)
	page := f.page(t, view.All())
	for i := 0; i < 10; i++ {
		f.publish(t, 0, i%2 == 0)
	}
	started := make(chan struct{})
	half := make(chan struct{})
	finished := make(chan error, 1)
	f.auth.mu.Lock()
	f.auth.onSubscribeConfig = func() { close(started); <-half }
	f.auth.mu.Unlock()
	go func() {
		<-started
		for i := 0; i < 150; i++ {
			if err := f.publishError(0, i%2 == 0); err != nil {
				finished <- err
				return
			}
			if i == 74 {
				close(half)
			}
		}
		finished <- nil
	}()
	w := openWatch(t, f, page.Cursor)
	f.auth.mu.Lock()
	f.auth.onSubscribeConfig = nil
	f.auth.mu.Unlock()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		f.publish(t, 0, i%2 == 0)
	}
	for i := 0; i < 170; i++ {
		frame := nextFrame(t, w)
		if frame.GetDelta() == nil || frame.StreamSequence != uint64(i+1) || frame.Cursor.GlobalSequence != page.Cursor.GlobalSequence+uint64(i+1) {
			t.Fatalf("seam frame %d: %v", i, frame)
		}
	}
}

func TestNHA021CursorAdmissionAndWindows(t *testing.T) {
	f := newWatchFixture(t, 1)
	page := f.page(t, view.All())
	if len(page.Cursor.AdmissionProof) != 40 || len(page.Cursor.GrantSha256) != 32 {
		t.Fatal("unsigned page cursor")
	}
	bad := proto.Clone(page.Cursor).(*statewire.ViewCursor)
	bad.GlobalSequence++
	requireResync(t, nextFrame(t, openWatch(t, f, bad)), statewire.ResyncReason_RESYNC_REASON_INVALID_CURSOR)
	bad = proto.Clone(page.Cursor).(*statewire.ViewCursor)
	bad.Epoch = strings.Repeat("a", 32)
	requireResync(t, nextFrame(t, openWatch(t, f, bad)), statewire.ResyncReason_RESYNC_REASON_EPOCH_CHANGED)
	other := testActor(t, "other")
	f.auth.mu.Lock()
	f.auth.otherGrants = map[view.ActorID]view.Grant{other: testGrant(t, other, 1, f.ids)}
	f.auth.mu.Unlock()
	watch, err := f.service.WatchView(context.Background(), other, watchRequest(page.Cursor))
	if err != nil {
		t.Fatal(err)
	}
	requireResync(t, nextFrame(t, watch), statewire.ResyncReason_RESYNC_REASON_INVALID_CURSOR)
	filter, _ := view.Only(f.ids)
	requireResync(t, nextFrame(t, openWatch(t, f, page.Cursor, f.ids...)), statewire.ResyncReason_RESYNC_REASON_FILTER_CHANGED)
	_ = filter
	f.clock.Advance(61*time.Second, -2*time.Hour)
	f.publish(t, 0, true)
	if frame := nextFrame(t, openWatch(t, f, page.Cursor)); frame.GetDelta() == nil {
		t.Fatalf("snapshot TTL incorrectly limited replay: %v", frame)
	}
	f.clock.Advance(60*time.Second, 0)
	requireResync(t, nextFrame(t, openWatch(t, f, page.Cursor)), statewire.ResyncReason_RESYNC_REASON_REPLAY_EXPIRED)
}

func TestNHA021IdleRevocationAndMetadata(t *testing.T) {
	f := newWatchFixture(t, 1)
	page := f.page(t, view.All())
	w := openWatch(t, f, page.Cursor)
	result := make(chan *statewire.WatchViewResponse, 1)
	entered := make(chan struct{})
	release := make(chan struct{})
	f.auth.mu.Lock()
	target := f.auth.reads + 1
	f.auth.onRead = func(n int) {
		if n == target {
			close(entered)
			<-release
		}
	}
	f.auth.mu.Unlock()
	go func() { frame, _ := w.Next(context.Background()); result <- frame }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Next did not read old grant")
	}
	f.auth.SetGrant(testGrant(t, f.actor, 1, nil)) // same revision, changed digest
	close(release)
	select {
	case frame := <-result:
		requireResync(t, frame, statewire.ResyncReason_RESYNC_REASON_AUTHORIZATION_CHANGED)
	case <-time.After(5 * time.Second):
		t.Fatal("idle revocation did not wake")
	}
	f.auth.mu.Lock()
	f.auth.onRead = nil
	f.auth.mu.Unlock()
	// An already stale grant is rejected at admission as well.
	requireResync(t, nextFrame(t, openWatch(t, f, page.Cursor)), statewire.ResyncReason_RESYNC_REASON_AUTHORIZATION_CHANGED)
	f.auth.SetGrant(testGrant(t, f.actor, 2, f.ids))
	page = f.page(t, view.All())
	w = openWatch(t, f, page.Cursor)
	result = make(chan *statewire.WatchViewResponse, 1)
	go func() { frame, _ := w.Next(context.Background()); result <- frame }()
	f.auth.SetConfig(testConfig(t, 2, "config-two"))
	select {
	case frame := <-result:
		requireResync(t, frame, statewire.ResyncReason_RESYNC_REASON_CONFIGURATION_CHANGED)
	case <-time.After(5 * time.Second):
		t.Fatal("idle config change did not wake")
	}
	page = f.page(t, view.All())
	w = openWatch(t, f, page.Cursor)
	key, _ := integrations.NewSourceKey("device-a", "relay")
	if _, err := f.session.Discover(context.Background(), []integrations.Candidate{testCandidate(t, key, "Changed declaration")}); err != nil {
		t.Fatal(err)
	}
	requireResync(t, nextFrame(t, w), statewire.ResyncReason_RESYNC_REASON_CAPABILITIES_CHANGED)
	page = f.page(t, view.All())
	w = openWatch(t, f, page.Cursor)
	device, _, _ := f.bindings[0].IDs()
	instance, _ := domain.ParseIntegrationInstanceID("fixture-watch")
	alias, _ := domain.ParseHAAlias("switch.renamed")
	record, _ := integrations.NewRegistryRecord(device, f.ids[0], instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err := f.seq.PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	requireResync(t, nextFrame(t, w), statewire.ResyncReason_RESYNC_REASON_REGISTRY_CHANGED)
	page = f.page(t, view.All())
	w = openWatch(t, f, page.Cursor)
	f.seq.InvalidateRegistry()
	requireResync(t, nextFrame(t, w), statewire.ResyncReason_RESYNC_REASON_REGISTRY_UNAVAILABLE)
}

func TestNHA021SlowClientAndClose(t *testing.T) {
	f := newWatchFixture(t, 1)
	page := f.page(t, view.All())
	slow := openWatch(t, f, page.Cursor)
	fast := openWatch(t, f, page.Cursor)
	for i := 0; i < 300; i++ {
		f.publish(t, 0, i%2 == 0)
		frame := nextFrame(t, fast)
		if frame.StreamSequence != uint64(i+1) {
			t.Fatalf("fast client stalled at %d", i)
		}
	}
	requireResync(t, nextFrame(t, slow), statewire.ResyncReason_RESYNC_REASON_CLIENT_SLOW)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := fast.Next(ctx); finished <- err }()
	if err := fast.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("Close/Next: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not wake Next")
	}
	var zero view.Watch
	if _, err := zero.Next(context.Background()); !errors.Is(err, view.ErrInvalidInput) {
		t.Fatalf("zero Watch: %v", err)
	}
}

func TestNHA021CaptureTimeAndLargeRetainedSuffix(t *testing.T) {
	f := newWatchFixture(t, 1)
	f.auth.mu.Lock()
	f.auth.onRead = func(n int) {
		if n == 2 {
			f.clock.Advance(121*time.Second, 0)
		}
	}
	f.auth.mu.Unlock()
	page := f.page(t, view.All())
	requireResync(t, nextFrame(t, openWatch(t, f, page.Cursor)), statewire.ResyncReason_RESYNC_REASON_REPLAY_EXPIRED)
	f.auth.mu.Lock()
	f.auth.onRead = nil
	f.auth.mu.Unlock()
	page = f.page(t, view.All())
	for i := 0; i < 5000; i++ {
		f.publish(t, 0, i%2 == 0)
	}
	w := openWatch(t, f, page.Cursor)
	for i := 0; i < 5000; i++ {
		frame := nextFrame(t, w)
		if frame.StreamSequence != uint64(i+1) {
			t.Fatalf("replay %d", i)
		}
	}
}

func TestNHA021ReplayCopyExpiresAndMailboxLossPreemptsIt(t *testing.T) {
	f := newWatchFixture(t, 1)
	page := f.page(t, view.All())
	f.publish(t, 0, true)
	w := openWatch(t, f, page.Cursor)
	f.clock.Advance(121*time.Second, 0)
	requireResync(t, nextFrame(t, w), statewire.ResyncReason_RESYNC_REASON_REPLAY_EXPIRED)

	page = f.page(t, view.All())
	f.publish(t, 0, false)
	w = openWatch(t, f, page.Cursor)
	for i := 0; i < 300; i++ {
		f.publish(t, 0, i%2 == 0)
	}
	requireResync(t, nextFrame(t, w), statewire.ResyncReason_RESYNC_REASON_CLIENT_SLOW)

	page = f.page(t, view.All())
	f.publish(t, 0, false)
	w = openWatch(t, f, page.Cursor)
	f.seq.InvalidateRegistry()
	requireResync(t, nextFrame(t, w), statewire.ResyncReason_RESYNC_REASON_REGISTRY_UNAVAILABLE)
}

func TestNHA021EntryExpiresDuringAuthorityRead(t *testing.T) {
	f := newWatchFixture(t, 1)
	page := f.page(t, view.All())
	f.publish(t, 0, true)
	w := openWatch(t, f, page.Cursor)
	entered := make(chan struct{})
	release := make(chan struct{})
	f.auth.mu.Lock()
	target := f.auth.reads + 2 // second authority read in Next, after the first age check
	f.auth.onRead = func(n int) {
		if n == target {
			close(entered)
			<-release
		}
	}
	f.auth.mu.Unlock()
	result := make(chan *statewire.WatchViewResponse, 1)
	go func() { frame, _ := w.Next(context.Background()); result <- frame }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("second authority read not reached")
	}
	f.clock.Advance(121*time.Second, 0)
	close(release)
	select {
	case frame := <-result:
		requireResync(t, frame, statewire.ResyncReason_RESYNC_REASON_REPLAY_EXPIRED)
	case <-time.After(5 * time.Second):
		t.Fatal("stale replay frame did not terminate")
	}
}
