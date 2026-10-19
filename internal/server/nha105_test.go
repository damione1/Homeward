package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/damione1/Homeward/contracts"
	statewire "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/internal/view"
	"google.golang.org/protobuf/proto"
	"net"
	"sync"
	"testing"
	"time"
)

type nha105RunClock struct {
	mu     sync.Mutex
	wall   time.Time
	tick   time.Duration
	timers []*nha105RunTimer
}
type nha105RunTimer struct {
	clock   *nha105RunClock
	due     time.Duration
	ch      chan time.Time
	stopped bool
}

func (c *nha105RunClock) Now() time.Time         { c.mu.Lock(); defer c.mu.Unlock(); return c.wall }
func (c *nha105RunClock) MonoNow() time.Duration { c.mu.Lock(); defer c.mu.Unlock(); return c.tick }
func (c *nha105RunClock) NewTimer(d time.Duration) automationtriggers.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &nha105RunTimer{clock: c, due: c.tick + d, ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, timer)
	return timer
}
func (t *nha105RunTimer) C() <-chan time.Time { return t.ch }
func (t *nha105RunTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	previous := !t.stopped
	t.stopped = true
	return previous
}
func (c *nha105RunClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(d)
	c.tick += d
	for _, timer := range c.timers {
		if !timer.stopped && timer.due <= c.tick {
			select {
			case timer.ch <- c.wall:
			default:
			}
		}
	}
}

// The active configuration authority and RPC facade belong to029/147. These
// fixtures qualify the existing view codec against Run's real sequencer only.
type nha105RunViewAuthority struct {
	grant view.Grant
	stamp view.ConfigStamp
}

func (a nha105RunViewAuthority) CurrentGrant(context.Context, view.ActorID) (view.Grant, error) {
	return a.grant, nil
}
func (a nha105RunViewAuthority) CurrentConfig(context.Context) (view.ConfigStamp, error) {
	return a.stamp, nil
}
func (nha105RunViewAuthority) SubscribeGrantChanges(context.Context, view.ActorID) (<-chan struct{}, func(), error) {
	return make(chan struct{}), func() {}, nil
}
func (nha105RunViewAuthority) SubscribeConfigChanges(context.Context) (<-chan struct{}, func(), error) {
	return make(chan struct{}), func() {}, nil
}

type nha105RunHandle struct {
	cancel   context.CancelFunc
	done     chan error
	joined   bool
	err      error
	sequence *sequencer.Sequencer
	clock    *nha105RunClock
}

func (h *nha105RunHandle) stop(t *testing.T) {
	t.Helper()
	h.cancel()
	if h.joined {
		return
	}
	select {
	case h.err = <-h.done:
		h.joined = true
		if h.err != nil {
			t.Error(h.err)
		}
	case <-time.After(10 * time.Second):
		t.Error("configured solar Run did not join")
	}
}
func nha105StartRun(t *testing.T, c config.Config, at time.Time) *nha105RunHandle {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &nha105RunHandle{cancel: cancel, done: make(chan error, 1), clock: &nha105RunClock{wall: at}}
	t.Cleanup(func() { h.stop(t) })
	sequence := make(chan *sequencer.Sequencer, 1)
	ready := make(chan struct{})
	dependencies := runDependencies{sequenceClock: h.clock, solarClock: h.clock, observe: func(_ context.Context, s *sequencer.Sequencer) error { sequence <- s; return nil }}
	go func() {
		h.done <- runWithDependencies(ctx, c, (&net.ListenConfig{}).Listen, func(net.Addr) { close(ready) }, nil, dependencies)
	}()
	select {
	case <-ready:
	case h.err = <-h.done:
		h.joined = true
		t.Fatal("configured solar Run exited before ready")
	case <-time.After(8 * time.Second):
		t.Fatal("configured solar Run did not start")
	}
	select {
	case h.sequence = <-sequence:
	case <-time.After(time.Second):
		t.Fatal("Run did not expose its actual sequencer to qualification")
	}
	return h
}
func nha105RunSnapshot(t *testing.T, h *nha105RunHandle) sequencer.Snapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot, err := h.sequence.CaptureBounded(16 << 20)
		if err == nil && len(snapshot.States) == 7 {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatal("configured solar states not observable")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestNHA105ConfiguredRunPublishesSevenSelectedAliases(t *testing.T) {
	cluster := testpg.Start(t)
	raw := fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\nsolar: {latitude: 0, longitude: 0, elevation_meters: 1000, timezone: America/New_York}\n", shortPrivateDataDir(t), cluster.Port)
	c, err := config.ParseWithEnv([]byte(raw), func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal("synthetic bootstrap failed")
	}
	observer := nha030ServerObserver(t, cluster.Port)
	first := nha105StartRun(t, c, time.Date(2024, 3, 10, 12, 0, 0, 0, time.UTC))
	snapshot := nha105RunSnapshot(t, first)
	ids := make([]domain.EntityID, 0, 7)
	aliases := make(map[string]string, 7)
	for id, record := range snapshot.Registry {
		_, current, _, err := record.Aliases()
		if err != nil {
			t.Fatal(err)
		}
		name, _ := current.Value()
		entity, _ := id.Value()
		aliases[name] = entity
		ids = append(ids, id)
		state, exists := snapshot.States[id]
		if !exists {
			t.Fatal("selected solar alias has no state")
		}
		native, _ := state.CurrentValue()
		kind, err := native.Kind()
		if err != nil || name == "sun.sun" && kind != domain.ValueSolar || name != "sun.sun" && kind != domain.ValueSolarEvent {
			t.Fatal("solar alias has wrong state kind")
		}
	}
	if len(aliases) != 7 || aliases["sun.sun"] == "" {
		t.Fatal("solar-only Run failed selected registry projection")
	}
	actor, _ := view.ParseActorID("nha105-fixture-view")
	grant, _ := view.NewGrant(actor, 1, ids)
	stamp, _ := view.NewConfigStamp(1, sha256.Sum256([]byte("nha105-view-fixture")))
	authority := nha105RunViewAuthority{grant, stamp}
	views, err := view.NewService(first.sequence, authority, authority, first.clock)
	if err != nil {
		t.Fatal(err)
	}
	page, err := views.OpenView(context.Background(), actor, view.All(), 3)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := view.EncodePage(page)
	if err != nil {
		t.Fatal(err)
	}
	oldWire, err := proto.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	seen := len(wire.Entities)
	current := page
	for current.NextPageToken != "" {
		current, err = views.ReadPage(context.Background(), actor, current.SnapshotToken, current.NextPageToken)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := view.EncodePage(current)
		if err != nil {
			t.Fatal(err)
		}
		seen += len(encoded.Entities)
	}
	if seen != 7 {
		t.Fatal("multipage solar view lost entities")
	}
	watch, err := views.WatchView(context.Background(), actor, &statewire.WatchViewRequest{Cursor: wire.Cursor, Filter: &statewire.ViewFilter{}})
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()
	// Wait for the pilot's sole timer before advancing the deterministic clock.
	deadline := time.Now().Add(time.Second)
	for {
		first.clock.mu.Lock()
		armed := len(first.clock.timers) > 0
		first.clock.mu.Unlock()
		if armed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("solar pilot failed to schedule")
		}
		time.Sleep(time.Millisecond)
	}
	first.clock.advance(5 * time.Minute)
	readCtx, readCancel := context.WithTimeout(context.Background(), 3*time.Second)
	frame, err := watch.Next(readCtx)
	readCancel()
	if err != nil || frame.GetDelta() == nil || len(frame.GetDelta().States) == 0 {
		t.Fatal("Run solar update lost replay/live view")
	}
	unchanged, _ := proto.Marshal(wire)
	if !bytes.Equal(unchanged, oldWire) {
		t.Fatal("captured wire changed after new solar publication")
	}
	first.stop(t)
	var epoch string
	var clean bool
	if err := observer.QueryRow(context.Background(), `SELECT encode(epoch,'hex'),clean_close FROM public.recorder_epochs ORDER BY run_id DESC LIMIT 1`).Scan(&epoch, &clean); err != nil || !clean {
		t.Fatal("Run did not drain Recorder")
	}
	var count int
	if err := observer.QueryRow(context.Background(), `SELECT count(*) FROM public.state_history WHERE epoch=decode($1,'hex') AND value_kind IN(7,8)`, epoch).Scan(&count); err != nil || count < 7 {
		t.Fatal("real Recorder did not persist seven solar observations")
	}
	var oldPayload, oldDigest []byte
	var oldSequence string
	if err := observer.QueryRow(context.Background(), `SELECT sequence::text,payload,payload_digest FROM public.state_history WHERE epoch=decode($1,'hex') AND value_kind IN(7,8) ORDER BY sequence LIMIT 1`, epoch).Scan(&oldSequence, &oldPayload, &oldDigest); err != nil {
		t.Fatal(err)
	}
	var stored statewire.ObservedState
	if err := proto.Unmarshal(oldPayload, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.GetSolarEvent() == nil {
		t.Fatal("ordered event-first publication was not recorded")
	}
	if _, err := contracts.AdmitWireSolarEventValue(stored.GetSolarEvent()); err != nil {
		t.Fatal(err)
	}
	second := nha105StartRun(t, c, time.Date(2024, 3, 11, 14, 0, 0, 0, time.UTC))
	reopened := nha105RunSnapshot(t, second)
	for id, record := range reopened.Registry {
		_, current, _, _ := record.Aliases()
		alias, _ := current.Value()
		entity, _ := id.Value()
		if aliases[alias] != entity {
			t.Fatal("reopen reassigned stable solar identity")
		}
		_, _, times, _, _, _ := reopened.States[id].Components()
		if times.ObservedAt == nil || !times.ObservedAt.Equal(second.clock.Now()) {
			t.Fatal("reopen replayed an old future as fresh")
		}
	}
	if _, _, err := second.sequence.ReadAfter(snapshot.Cursor, 1); err != sequencer.ErrEpochMismatch {
		t.Fatal("restart claimed cross-epoch journal replay")
	}
	var payload, digest []byte
	if err := observer.QueryRow(context.Background(), `SELECT payload,payload_digest FROM public.state_history WHERE epoch=decode($1,'hex') AND sequence=$2::numeric`, epoch, oldSequence).Scan(&payload, &digest); err != nil || !bytes.Equal(payload, oldPayload) || !bytes.Equal(digest, oldDigest) {
		t.Fatal("reopen changed historical bytes")
	}
	second.stop(t)
}
