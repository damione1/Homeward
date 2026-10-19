package recorder

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
)

type nha183TimerState uint8

const (
	nha183TimerUnspecified nha183TimerState = iota
	nha183TimerArmed
	nha183TimerExpired
	nha183TimerCanceled
)

// One clock owns now and the cancellation waking the real Subscription.Next.
// It has no goroutine, wall timer, or callback into the Host or sequencer.
type nha183ManualClock struct {
	mu                                 sync.Mutex
	origin                             time.Time
	elapsed                            time.Duration
	active                             *nha183ReadContext
	changed                            chan struct{}
	armed, expired, canceled, released uint64
}

type nha183ReadContext struct {
	base              context.Context
	clock             *nha183ManualClock
	at                time.Time
	cancel            context.CancelCauseFunc
	generation        uint64
	state             nha183TimerState
	waiting, released bool
}

type nha183ClockSnapshot struct {
	armed, expired, canceled, released uint64
	active, waiting                    bool
	at                                 time.Time
	generation                         uint64
}

func newNHA183Clock() *nha183ManualClock {
	return &nha183ManualClock{origin: time.Now(), changed: make(chan struct{})}
}

func (c *nha183ManualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.origin.IsZero() {
		return time.Time{}
	}
	return c.origin.Add(c.elapsed)
}

func (c *nha183ManualClock) signalLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *nha183ManualClock) withDeadline(parent context.Context, at time.Time) (context.Context, context.CancelFunc) {
	c.mu.Lock()
	if c.origin.IsZero() || c.changed == nil || c.active != nil || nilInterface(parent) || at.IsZero() {
		c.mu.Unlock()
		return nil, nil
	}
	base, cancel := context.WithCancelCause(parent)
	c.armed++
	w := &nha183ReadContext{base: base, clock: c, at: at, cancel: cancel, generation: c.armed, state: nha183TimerArmed}
	c.active = w
	expired := !c.origin.Add(c.elapsed).Before(at)
	if expired {
		w.state = nha183TimerExpired
		c.active = nil
		c.expired++
	}
	c.signalLocked()
	c.mu.Unlock()
	if expired {
		cancel(context.DeadlineExceeded)
	}
	return w, func() { c.release(w) }
}

func (c *nha183ManualClock) release(w *nha183ReadContext) {
	c.mu.Lock()
	if !w.released {
		w.released = true
		c.released++
	}
	switch w.state {
	case nha183TimerArmed:
		w.state = nha183TimerCanceled
		c.active = nil
		c.canceled++
	case nha183TimerExpired, nha183TimerCanceled:
	case nha183TimerUnspecified:
		panic("uninitialized manual read context")
	default:
		panic("unknown manual timer state")
	}
	c.signalLocked()
	c.mu.Unlock()
	w.cancel(context.Canceled)
}

func (c *nha183ManualClock) advanceTo(elapsed time.Duration) error {
	c.mu.Lock()
	if c.origin.IsZero() || c.changed == nil || elapsed < c.elapsed || elapsed < 0 {
		c.mu.Unlock()
		return ErrInvalidRecorder
	}
	c.elapsed = elapsed
	w := c.active
	if w != nil && !c.origin.Add(elapsed).Before(w.at) {
		w.state = nha183TimerExpired
		c.active = nil
		c.expired++
	} else {
		w = nil
	}
	c.signalLocked()
	c.mu.Unlock()
	if w != nil {
		w.cancel(context.DeadlineExceeded)
	}
	return nil
}

func (c *nha183ManualClock) snapshotLocked() nha183ClockSnapshot {
	s := nha183ClockSnapshot{armed: c.armed, expired: c.expired, canceled: c.canceled, released: c.released}
	if c.active != nil {
		s.active, s.waiting, s.at, s.generation = true, c.active.waiting, c.active.at, c.active.generation
	}
	return s
}

func (c *nha183ManualClock) snapshot() nha183ClockSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked()
}

func (c *nha183ManualClock) awaitRead(t *testing.T, minimumGeneration uint64) nha183ClockSnapshot {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		c.mu.Lock()
		s, changed := c.snapshotLocked(), c.changed
		c.mu.Unlock()
		// An arm alone is insufficient: the context must have reached Next's
		// empty-queue select after all previous accept/threshold work finished.
		if s.active && s.waiting && s.generation >= minimumGeneration {
			return s
		}
		select {
		case <-changed:
		case <-deadline.C:
			t.Fatalf("worker did not enter read generation %d: %+v", minimumGeneration, s)
		}
	}
}

func (w *nha183ReadContext) Deadline() (time.Time, bool) {
	if parent, ok := w.base.Deadline(); ok && parent.Before(w.at) {
		return parent, true
	}
	return w.at, true
}

func (w *nha183ReadContext) Done() <-chan struct{} {
	w.clock.mu.Lock()
	if w.state == nha183TimerArmed && !w.waiting {
		w.waiting = true
		w.clock.signalLocked()
	}
	w.clock.mu.Unlock()
	return w.base.Done()
}

func (w *nha183ReadContext) Err() error {
	if err := w.base.Err(); err != nil {
		if errors.Is(context.Cause(w.base), context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		return err
	}
	return nil
}

func (w *nha183ReadContext) Value(key any) any { return w.base.Value(key) }

// The spy observes actual SQL call contexts; it never substitutes manual time
// for SQL and never calls the Recorder while holding the writer mutex.
type nha183ObservedWriter struct {
	writer                         *nha030Writer
	clock                          *nha183ManualClock
	mu                             sync.Mutex
	calls                          int
	invalidDeadline, readDuringSQL bool
}

func (w *nha183ObservedWriter) observe(ctx context.Context) {
	at, ok := ctx.Deadline()
	remaining := time.Until(at)
	active := w.clock != nil && w.clock.snapshot().active
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	w.invalidDeadline = w.invalidDeadline || !ok || remaining > 2*time.Second
	w.readDuringSQL = w.readDuringSQL || active
}

func (w *nha183ObservedWriter) OpenRecorderEpoch(ctx context.Context, epoch storage.RecorderEpoch, at time.Time) (storage.RecorderPrevious, storage.TransactionOutcome, error) {
	w.observe(ctx)
	return w.writer.OpenRecorderEpoch(ctx, epoch, at)
}
func (w *nha183ObservedWriter) PersistRecorderBatch(ctx context.Context, batch storage.RecorderBatch) (storage.TransactionOutcome, error) {
	w.observe(ctx)
	return w.writer.PersistRecorderBatch(ctx, batch)
}
func (w *nha183ObservedWriter) RecorderCheckpoint(ctx context.Context, epoch storage.RecorderEpoch) (storage.RecorderCheckpoint, error) {
	w.observe(ctx)
	return w.writer.RecorderCheckpoint(ctx, epoch)
}
func (w *nha183ObservedWriter) CloseRecorderEpoch(ctx context.Context, epoch storage.RecorderEpoch, through uint64) (storage.TransactionOutcome, error) {
	w.observe(ctx)
	return w.writer.CloseRecorderEpoch(ctx, epoch, through)
}

func (w *nha183ObservedWriter) assert(t *testing.T) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.calls < 3 || w.invalidDeadline || w.readDuringSQL || attemptTimeout != 2*time.Second {
		t.Fatalf("SQL budget/read lifetime changed: calls=%d invalid_deadline=%v active_read=%v", w.calls, w.invalidDeadline, w.readDuringSQL)
	}
}

type nha183Fixture struct {
	clock           *nha183ManualClock
	host            *integrations.Manager
	fixture         *fake.Fixture
	session         *integrations.Session
	driver          *fake.Driver
	binding         integrations.Binding
	writer          *nha030Writer
	spy             *nha183ObservedWriter
	recording       *Recorder
	base            sequencer.Cursor
	baseSequence    uint64
	initialDegraded bool
}

func newNHA183Fixture(t *testing.T) *nha183Fixture {
	t.Helper()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha183-manual")
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := fake.NewDriver(time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	f := &nha183Fixture{clock: newNHA183Clock(), host: host, fixture: fixture, session: session, driver: driver, writer: &nha030Writer{updated: make(chan struct{}, 8)}}
	f.spy = &nha183ObservedWriter{writer: f.writer, clock: f.clock}
	return f
}

func (f *nha183Fixture) startPilot(t *testing.T) {
	t.Helper()
	if err := f.driver.Start(context.Background(), f.session); err != nil {
		t.Fatal(err)
	}
	binding, err := f.driver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	f.binding = binding
}

func (f *nha183Fixture) startRecorder(t *testing.T) nha183ClockSnapshot {
	t.Helper()
	r, err := startWithFlushClock(context.Background(), f.fixture.Sequencer(), f.spy, DefaultLimits(), f.clock)
	if err != nil {
		t.Fatal(err)
	}
	f.adoptRecorder(t, r)
	return f.clock.awaitRead(t, 1)
}

func (f *nha183Fixture) adoptRecorder(t *testing.T, r *Recorder) {
	t.Helper()
	f.recording = r
	batches, checkpoint := nha183Batches(f.writer)
	if len(batches) != 1 || !batches[0].HasImage() {
		t.Fatal("initial image was not committed before worker start")
	}
	f.baseSequence = checkpoint.Sequence
	epoch, err := batches[0].Epoch().Hex()
	if err != nil {
		t.Fatal(err)
	}
	f.base, err = sequencer.ParseCursor(epoch, checkpoint.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	status, err := r.Status()
	if err != nil {
		t.Fatal(err)
	}
	f.initialDegraded = status.HistoryDegraded
	t.Cleanup(func() { f.stop(t) })
}

func (f *nha183Fixture) stop(t *testing.T) {
	t.Helper()
	if err := f.host.CloseAdmissions(); err != nil {
		t.Error(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := f.recording.Stop(ctx); err != nil {
		t.Error(err)
		return
	}
	select {
	case <-f.recording.Done():
	default:
		t.Error("Stop returned before worker joined")
	}
	s := f.clock.snapshot()
	if s.active || s.armed != s.expired+s.canceled || s.released != s.armed {
		t.Errorf("unreleased read after join: %+v", s)
	}
	f.spy.assert(t)
}

func nha183Batches(writer *nha030Writer) ([]storage.RecorderBatch, storage.RecorderCheckpoint) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return append([]storage.RecorderBatch(nil), writer.batches...), writer.checkpoint
}

func (f *nha183Fixture) publishStates(t *testing.T, count int) {
	t.Helper()
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 18, 12, 0, 1, 0, time.UTC)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for i := 0; i < count; i++ {
		value := domain.NewSwitchValue(i%2 == 0)
		observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.session.Publish(ctx, f.binding, observation); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *nha183Fixture) publishHealth(t *testing.T) {
	t.Helper()
	report, err := integrations.NewHealthReport(integrations.HealthReady, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := f.session.ReportHealth(ctx, report); err != nil {
		t.Fatal(err)
	}
}

func (f *nha183Fixture) publishEvent(t *testing.T) {
	t.Helper()
	event, err := integrations.NewDeviceEvent("fixture.relay.observed", time.Date(2026, 10, 18, 12, 0, 1, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := f.session.Emit(ctx, f.binding, event); err != nil {
		t.Fatal(err)
	}
}

func (f *nha183Fixture) advance(t *testing.T, elapsed time.Duration) {
	t.Helper()
	if err := f.clock.advanceTo(elapsed); err != nil {
		t.Fatal(err)
	}
}

func (f *nha183Fixture) assertBatchCount(t *testing.T, count int) {
	t.Helper()
	batches, _ := nha183Batches(f.writer)
	if len(batches) != count {
		t.Fatalf("batch count: got %d want %d", len(batches), count)
	}
}

func nha183ExpectedBatch(t *testing.T, sequence *sequencer.Sequencer, epoch storage.RecorderEpoch, from, through uint64) storage.RecorderBatch {
	t.Helper()
	epochText, err := epoch.Hex()
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := sequencer.ParseCursor(epochText, from)
	if err != nil {
		t.Fatal(err)
	}
	entries, _, err := sequence.ReadAfter(cursor, int(through-from))
	if err != nil || uint64(len(entries)) != through-from {
		t.Fatalf("journal prefix: entries=%d want=%d err=%v", len(entries), through-from, err)
	}
	var rows []storage.RecorderRecord
	var excluded uint64
	for _, entry := range entries {
		switch entry.Kind() {
		case sequencer.KindState:
			_, position, err := entry.Cursor().Components()
			if err != nil {
				t.Fatal(err)
			}
			_, state, _, ok := entry.State()
			instance, _, causality, origin := entry.Origin()
			if !ok || !origin {
				t.Fatal("state has no admitted origin")
			}
			row, err := storage.NewRecorderRecord(position, instance, state, causality, entry.ReceivedAt(), maxRecordPayloadBytes)
			if err != nil {
				t.Fatal(err)
			}
			rows = append(rows, row)
		case sequencer.KindEvent, sequencer.KindHealth, sequencer.KindRegistry, sequencer.KindCapabilities, sequencer.KindNativeMQTTEdge:
			excluded++
		case sequencer.KindUnspecified:
			t.Fatal("unspecified journal entry")
		default:
			t.Fatalf("unhandled journal kind %d", entry.Kind())
		}
	}
	batch, err := storage.NewRecorderBatch(epoch, from, through, rows, nil, excluded, nil)
	if err != nil {
		t.Fatal(err)
	}
	return batch
}

func (f *nha183Fixture) assertCommitted(t *testing.T, index int, from, through uint64, states int) {
	t.Helper()
	batches, checkpoint := nha183Batches(f.writer)
	if len(batches) != index+1 {
		t.Fatalf("unexpected batch count %d at index %d", len(batches), index)
	}
	actual := batches[index]
	expected := nha183ExpectedBatch(t, f.fixture.Sequencer(), actual.Epoch(), from, through)
	if actual.StateCount() != states || actual.From() != from || actual.Through() != through || actual.HasGaps() || actual.HasImage() || actual.Digest() != expected.Digest() || !checkpoint.Confirms(expected) {
		t.Fatalf("committed prefix differs: from=%d through=%d states=%d checkpoint=%d", actual.From(), actual.Through(), actual.StateCount(), checkpoint.Sequence)
	}
	status, err := f.recording.Status()
	if err != nil || status.Checkpoint != through || status.LostEntries != 0 || status.HistoryDegraded != f.initialDegraded || !status.ReconciliationComplete {
		t.Fatalf("coverage/checkpoint differs: %+v %v", status, err)
	}
}

func (f *nha183Fixture) awaitPrefix(t *testing.T, before nha183ClockSnapshot, entries uint64) nha183ClockSnapshot {
	t.Helper()
	if entries == 0 {
		t.Fatal("a consumption barrier needs a newly admitted prefix")
	}
	s := f.clock.awaitRead(t, before.generation+entries)
	status, err := f.recording.Status()
	if err != nil || status.QueuedEntries != 0 || status.LostEntries != 0 {
		t.Fatalf("prefix did not reach an empty lossless read: %+v %v", status, err)
	}
	return s
}

func nha183CountBoundary(t *testing.T) {
	t.Helper()
	f := newNHA183Fixture(t)
	f.startPilot(t)
	before := f.startRecorder(t)
	f.publishStates(t, FlushStates-1)
	before = f.awaitPrefix(t, before, FlushStates-1)
	f.assertBatchCount(t, 1)
	wantDeadline := f.clock.now().Add(flushInterval)
	if !before.at.Equal(wantDeadline) {
		t.Fatal("first entry did not own the batch deadline")
	}
	f.advance(t, 249*time.Millisecond)
	f.publishHealth(t)
	before = f.awaitPrefix(t, before, 1)
	if !before.at.Equal(wantDeadline) {
		t.Fatal("excluded wake postponed the count batch deadline")
	}
	f.assertBatchCount(t, 1)
	f.publishStates(t, 1)
	f.awaitPrefix(t, before, 1)
	f.assertCommitted(t, 1, f.baseSequence, f.baseSequence+FlushStates+1, FlushStates)
	if !f.clock.now().Equal(wantDeadline.Add(-time.Millisecond)) {
		t.Fatal("count flush advanced to its timer deadline")
	}
}

func TestNHA183CountBoundaryUsesConsumedPrefix(t *testing.T) { nha183CountBoundary(t) }

func nha183TimerBoundary(t *testing.T) {
	t.Helper()
	f := newNHA183Fixture(t)
	before := f.startRecorder(t)
	if f.baseSequence != 0 {
		t.Fatal("initial image did not precede pilot startup")
	}
	f.startPilot(t)
	cursor, err := f.fixture.Sequencer().RecorderCursor()
	if err != nil {
		t.Fatal(err)
	}
	_, through, err := cursor.Components()
	if err != nil {
		t.Fatal(err)
	}
	before = f.awaitPrefix(t, before, through-f.baseSequence)
	f.assertBatchCount(t, 1)
	wantDeadline := before.at
	f.advance(t, 249*time.Millisecond)
	f.publishHealth(t)
	through++
	before = f.awaitPrefix(t, before, 1)
	if !before.at.Equal(wantDeadline) {
		t.Fatal("249ms wake reset the first-entry deadline")
	}
	f.assertBatchCount(t, 1)
	f.advance(t, 250*time.Millisecond)
	before = f.clock.awaitRead(t, before.generation+1)
	f.assertCommitted(t, 1, f.baseSequence, through, 1)
	status, err := f.recording.Status()
	if err != nil || status.PolicyExcluded != through-f.baseSequence-1 {
		t.Fatalf("excluded kinds became states or losses: %+v %v", status, err)
	}
	f.advance(t, 500*time.Millisecond)
	f.clock.awaitRead(t, before.generation+1)
	f.assertBatchCount(t, 2)
}

func TestNHA183TimerBoundaryNeedsNoNewEntry(t *testing.T) { nha183TimerBoundary(t) }

func TestNHA183ExcludedTrafficKeepsOriginalDeadline(t *testing.T) {
	for _, states := range []int{0, 1} {
		t.Run(map[int]string{0: "excluded-only", 1: "state-and-exclusions"}[states], func(t *testing.T) {
			f := newNHA183Fixture(t)
			f.startPilot(t)
			before := f.startRecorder(t)
			through := f.baseSequence
			if states == 1 {
				f.publishStates(t, 1)
				before = f.awaitPrefix(t, before, 1)
				through++
			}
			deadline := f.clock.now().Add(flushInterval)
			for _, at := range []time.Duration{0, 75 * time.Millisecond, 150 * time.Millisecond, 249 * time.Millisecond} {
				f.advance(t, at)
				f.publishEvent(t)
				f.publishHealth(t)
				before = f.awaitPrefix(t, before, 2)
				through += 2
				if !before.at.Equal(deadline) {
					t.Fatal("excluded stream reset the batch's absolute deadline")
				}
				f.assertBatchCount(t, 1)
			}
			f.advance(t, 250*time.Millisecond)
			f.clock.awaitRead(t, before.generation+1)
			f.assertCommitted(t, 1, f.baseSequence, through, states)
			status, err := f.recording.Status()
			if err != nil || status.PolicyExcluded != 8 {
				t.Fatalf("excluded stream coverage: %+v %v", status, err)
			}
		})
	}
}

func TestNHA183OldDeadlineCannotFlushNextBatch(t *testing.T) {
	f := newNHA183Fixture(t)
	f.startPilot(t)
	before := f.startRecorder(t)
	f.publishStates(t, FlushStates-1)
	before = f.awaitPrefix(t, before, FlushStates-1)
	f.clock.mu.Lock()
	old := f.clock.active
	f.clock.mu.Unlock()
	f.advance(t, 249*time.Millisecond)
	f.publishStates(t, 1)
	before = f.awaitPrefix(t, before, 1)
	f.assertCommitted(t, 1, f.baseSequence, f.baseSequence+FlushStates, FlushStates)
	f.clock.mu.Lock()
	oldState, oldReleased := old.state, old.released
	f.clock.mu.Unlock()
	if oldState != nha183TimerCanceled || !oldReleased {
		t.Fatal("count flush retained the previous read timer")
	}
	f.publishStates(t, 1)
	before = f.awaitPrefix(t, before, 1)
	wantDeadline := f.clock.now().Add(flushInterval)
	if !before.at.Equal(wantDeadline) {
		t.Fatal("new batch inherited the old deadline")
	}
	// A stale callback cannot change an already canceled child context.
	old.cancel(context.DeadlineExceeded)
	for _, at := range []time.Duration{250 * time.Millisecond, 498 * time.Millisecond} {
		f.advance(t, at)
		f.publishHealth(t)
		before = f.awaitPrefix(t, before, 1)
		if !before.at.Equal(wantDeadline) {
			t.Fatal("new batch timer moved after stale deadline")
		}
		f.assertBatchCount(t, 2)
	}
	f.advance(t, 499*time.Millisecond)
	f.clock.awaitRead(t, before.generation+1)
	f.assertCommitted(t, 2, f.baseSequence+FlushStates, f.baseSequence+FlushStates+3, 1)
}

func TestNHA183ReadDeadlineLifecycle(t *testing.T) {
	clock := newNHA183Clock()
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	ctx, cancel := clock.withDeadline(parent, clock.now().Add(flushInterval))
	if ctx == nil || cancel == nil {
		t.Fatal("manual deadline was not admitted")
	}
	if err := clock.advanceTo(249 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("timer expired at 249ms")
	}
	if err := clock.advanceTo(250 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("deadline did not wake Next")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("deadline conversion: %v", ctx.Err())
	}
	cancel()
	cancel()
	s := clock.snapshot()
	if s.active || s.armed != 1 || s.expired != 1 || s.released != 1 {
		t.Fatalf("deadline did not finish exactly once: %+v", s)
	}
	ctx, cancel = clock.withDeadline(parent, clock.now().Add(flushInterval))
	cancelParent()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("parent cancellation did not wake read")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("parent cancellation conversion: %v", ctx.Err())
	}
	cancel()
	s = clock.snapshot()
	if s.active || s.armed != 2 || s.expired != 1 || s.canceled != 1 || s.released != 2 {
		t.Fatalf("cancellation did not finish exactly once: %+v", s)
	}
	if err := clock.advanceTo(249 * time.Millisecond); !errors.Is(err, ErrInvalidRecorder) {
		t.Fatal("manual time moved backwards")
	}
	if err := clock.advanceTo(500 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if after := clock.snapshot(); after != s {
		t.Fatalf("finished timers survived advance: %+v", after)
	}
}

func TestNHA183PhysicalParentDeadlineIsNotSimulated(t *testing.T) {
	clock := newNHA183Clock()
	readCtx, cancelRead := context.WithCancel(context.Background())
	defer cancelRead()
	r := &Recorder{readCtx: readCtx, flushClock: clock}
	physicalDeadline := time.Now().Add(-time.Second)
	ctx, cancel, err := r.nextReadContext(flushDeadline{}, false, physicalDeadline)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("physical recovery deadline was hidden by manual flush time")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("physical parent conversion: %v", ctx.Err())
	}
	if at, ok := ctx.Deadline(); !ok || !at.Equal(physicalDeadline) {
		t.Fatal("manual child replaced the earlier physical deadline")
	}
	if !clock.now().Equal(clock.origin) {
		t.Fatal("physical expiry advanced manual time")
	}
	cancel()
	if s := clock.snapshot(); s.active || s.expired != 0 || s.canceled != 1 || s.released != 1 {
		t.Fatalf("physical parent leaked the manual child: %+v", s)
	}
}

func TestNHA183DeadlineAbsenceAndRearmAreExplicit(t *testing.T) {
	clock := newNHA183Clock()
	now := clock.now()
	if (flushDeadline{}).present() || (flushDeadline{}).due(now) {
		t.Fatal("absent deadline became an immediate timeout")
	}
	if _, err := newFlushDeadline(time.Time{}); !errors.Is(err, ErrInvalidRecorder) {
		t.Fatal("zero instant admitted")
	}
	if _, err := newFlushDeadline(time.Time{}.Add(-flushInterval)); !errors.Is(err, ErrInvalidRecorder) {
		t.Fatal("derived zero deadline admitted")
	}
	d, err := newFlushDeadline(now)
	if err != nil || d.due(now.Add(249*time.Millisecond)) || !d.due(now.Add(250*time.Millisecond)) {
		t.Fatal("deadline lost its exact boundary")
	}
	if err := clock.advanceTo(250 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	r := &Recorder{readCtx: context.Background(), flushClock: clock}
	ctx, cancel, err := r.nextReadContext(d, false, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if at, ok := ctx.Deadline(); !ok || !at.Equal(now.Add(500*time.Millisecond)) || ctx.Err() != nil {
		t.Fatal("unready batch spun on an expired deadline")
	}
}

func TestNHA183StopJoinsAndReleasesRead(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "empty"
		if pending {
			name = "pending"
		}
		t.Run(name, func(t *testing.T) {
			f := newNHA183Fixture(t)
			f.startPilot(t)
			before := f.startRecorder(t)
			if pending {
				f.publishStates(t, 1)
				f.awaitPrefix(t, before, 1)
			}
			f.stop(t)
			batches, checkpoint := nha183Batches(f.writer)
			wantStates := uint64(0)
			if pending {
				wantStates = 1
			}
			if checkpoint.Sequence != f.baseSequence+wantStates || !checkpoint.CleanClose {
				t.Fatal("stop did not drain the admitted prefix")
			}
			beforeStop := f.clock.snapshot()
			f.advance(t, time.Second)
			f.assertBatchCount(t, len(batches))
			if after := f.clock.snapshot(); after != beforeStop {
				t.Fatalf("read survived worker join: %+v", after)
			}
		})
	}
}

func TestNHA183RejectsInvalidFlushClock(t *testing.T) {
	_, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	var typedNil *nha183ManualClock
	for _, clock := range []flushClock{nil, typedNil, &nha183ManualClock{}} {
		writer := &nha030Writer{}
		if r, err := startWithFlushClock(context.Background(), fixture.Sequencer(), writer, DefaultLimits(), clock); r != nil || !errors.Is(err, ErrInvalidRecorder) {
			t.Fatalf("invalid flush clock admitted: %T %v", clock, err)
		}
		batches, _ := nha183Batches(writer)
		if len(batches) != 0 || writer.writes != 0 {
			t.Fatal("invalid clock crossed the SQL boundary")
		}
	}
	// Invalid attempts must not consume the sequencer's bounded subscriptions.
	var subscriptions []*sequencer.Subscription
	defer func() {
		for _, sub := range subscriptions {
			_ = sub.Close()
		}
	}()
	for i := 0; i < sequencer.MaxSubscriptions; i++ {
		sub, _, err := fixture.Sequencer().SubscribeRecorder()
		if err != nil {
			t.Fatalf("invalid clock retained a subscription: %v", err)
		}
		subscriptions = append(subscriptions, sub)
	}
}

type nha183BrokenClock struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func (nha183BrokenClock) now() time.Time { return time.Now() }
func (c nha183BrokenClock) withDeadline(context.Context, time.Time) (context.Context, context.CancelFunc) {
	return c.ctx, c.cancel
}

func TestNHA183RejectsInvalidReadContext(t *testing.T) {
	var typedNil context.Context = (*nha030TypedNilContext)(nil)
	for _, next := range []context.Context{nil, typedNil, context.Background()} {
		_, fixture, err := fake.NewFixture()
		if err != nil {
			t.Fatal(err)
		}
		canceled := false
		clock := nha183BrokenClock{ctx: next}
		if next != context.Background() {
			clock.cancel = func() { canceled = true }
		}
		r, err := startWithFlushClock(context.Background(), fixture.Sequencer(), &nha030Writer{}, DefaultLimits(), clock)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.NewTimer(3 * time.Second)
		select {
		case <-r.Done():
		case <-deadline.C:
			t.Fatal("invalid read context left worker alive")
		}
		deadline.Stop()
		status, err := r.Status()
		if err != nil || status.Phase != PhaseFailed || !errors.Is(status.LastError, ErrInvalidRecorder) {
			t.Fatalf("invalid child not rejected: %+v %v", status, err)
		}
		if clock.cancel != nil && !canceled {
			t.Fatal("invalid child did not release its timer")
		}
		queue, err := r.sub.Status()
		if err != nil || !queue.Closed {
			t.Fatal("failed worker retained its subscription")
		}
	}
}

func TestNHA183DefaultClockKeepsPhysicalBudgets(t *testing.T) {
	if FlushStates != 500 || FlushEncodedBytes != 1<<20 || flushInterval != 250*time.Millisecond || attemptTimeout != 2*time.Second {
		t.Fatal("physical/count/encoded budgets changed")
	}
	f := newNHA183Fixture(t)
	f.startPilot(t)
	r, err := Start(context.Background(), f.fixture.Sequencer(), f.spy, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	f.adoptRecorder(t, r)
	if _, ok := r.flushClock.(realFlushClock); !ok {
		t.Fatal("public constructor did not select the real flush clock")
	}
	f.publishStates(t, 1)
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		batches, checkpoint := nha183Batches(f.writer)
		if len(batches) > 1 {
			if len(batches) != 2 || batches[1].StateCount() != 1 || !checkpoint.Confirms(batches[1]) {
				t.Fatal("public real timer did not persist exactly the sub-threshold prefix")
			}
			break
		}
		select {
		case <-f.writer.updated:
		case <-deadline.C:
			t.Fatal("public real timer never flushed")
		}
	}
	f.stop(t)
}

// This witness intentionally uses only the public parent API. The 500th state
// is withheld until a real timed batch demonstrates the old oracle's premise.
func TestNHA183RealClockAllowsTimedPrefix(t *testing.T) {
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha183-real-prefix")
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	driver, err := fake.NewDriver(at)
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	binding, err := driver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	writer := &nha030Writer{updated: make(chan struct{}, 8)}
	recording, err := Start(context.Background(), fixture.Sequencer(), writer, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := host.CloseAdmissions(); err != nil {
			t.Error(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := recording.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	writer.mu.Lock()
	base := writer.checkpoint.Sequence
	writer.mu.Unlock()
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	publish := func(i int) {
		value := domain.NewSwitchValue(i%2 == 0)
		observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := session.Publish(ctx, binding, observation); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < FlushStates-1; i++ {
		publish(i)
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	var first storage.RecorderBatch
	for {
		writer.mu.Lock()
		if len(writer.batches) > 1 {
			first = writer.batches[1]
		}
		writer.mu.Unlock()
		if first.StateCount() > 0 {
			break
		}
		select {
		case <-writer.updated:
		case <-deadline.C:
			t.Fatal("real timer did not persist the withheld prefix")
		}
	}
	legacyOracle := first.StateCount() == FlushStates
	t.Logf("public real-clock witness: first_states=%d legacy_first_batch_500=%v from=%d through=%d", first.StateCount(), legacyOracle, first.From(), first.Through())
	if legacyOracle || first.From() != base || first.Through() > base+FlushStates-1 || first.HasGaps() || first.HasImage() {
		t.Fatalf("not a legal timed prefix: states=%d from=%d through=%d", first.StateCount(), first.From(), first.Through())
	}
	epochText, err := first.Epoch().Hex()
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := sequencer.ParseCursor(epochText, base)
	if err != nil {
		t.Fatal(err)
	}
	entries, _, err := fixture.Sequencer().ReadAfter(cursor, int(first.Through()-base))
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]storage.RecorderRecord, 0, len(entries))
	for _, entry := range entries {
		_, position, err := entry.Cursor().Components()
		if err != nil {
			t.Fatal(err)
		}
		_, state, _, ok := entry.State()
		instance, _, causality, origin := entry.Origin()
		if !ok || !origin {
			t.Fatal("unexpected non-state in held prefix")
		}
		row, err := storage.NewRecorderRecord(position, instance, state, causality, entry.ReceivedAt(), maxRecordPayloadBytes)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	expected, err := storage.NewRecorderBatch(first.Epoch(), base, first.Through(), rows, nil, 0, nil)
	if err != nil || expected.Digest() != first.Digest() {
		t.Fatalf("timed prefix content changed: %v", err)
	}
	publish(FlushStates - 1)
	if err := host.CloseAdmissions(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := recording.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	states := 0
	through := base
	for _, batch := range writer.batches[1:] {
		if batch.From() != through || batch.HasGaps() {
			t.Fatal("timed prefix lost ordered coverage")
		}
		states += batch.StateCount()
		through = batch.Through()
	}
	if states != FlushStates || through != base+FlushStates || !writer.checkpoint.Confirms(writer.batches[len(writer.batches)-1]) {
		t.Fatalf("final prefix not fully committed: states=%d checkpoint=%d", states, writer.checkpoint.Sequence)
	}
}
