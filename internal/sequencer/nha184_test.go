package sequencer

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

// A list oracle owns its values independently of the observed mailbox. It has
// no circular indices and never obtains expected entries from Subscription.Next.
type nha184FIFO struct {
	entries            []Entry
	bytes              int
	capacity, maxBytes int
	loss               *Loss
	lost               uint64
	replay             bool
}

func (m *nha184FIFO) lose(entry Entry) {
	if m.loss == nil {
		m.loss = &Loss{From: entry.Cursor(), To: entry.Cursor(), Reason: LossRecorderOverflow}
	}
	if entry.cursor.seq < m.loss.From.seq {
		m.loss.From = entry.Cursor()
	}
	if entry.cursor.seq > m.loss.To.seq {
		m.loss.To = entry.Cursor()
	}
	if m.loss.Count != math.MaxUint64 {
		m.loss.Count++
	}
	if m.lost != math.MaxUint64 {
		m.lost++
	}
	m.replay = false
}

func (m *nha184FIFO) push(entry Entry) {
	if entry.EstimatedBytes() > m.maxBytes {
		m.lose(entry)
		return
	}
	for len(m.entries)+1 > m.capacity || m.bytes+entry.EstimatedBytes() > m.maxBytes {
		old := m.entries[0]
		m.entries = m.entries[1:]
		m.bytes -= old.EstimatedBytes()
		m.lose(old)
	}
	m.entries = append(m.entries, entry)
	m.bytes += entry.EstimatedBytes()
}

func (m *nha184FIFO) next() (Delivery, error) {
	if m.loss != nil {
		loss := *m.loss
		m.loss = nil
		return Delivery{kind: DeliveryLoss, loss: loss}, ErrRecorderGap
	}
	if len(m.entries) == 0 {
		return Delivery{}, context.Canceled
	}
	entry := m.entries[0]
	m.entries = m.entries[1:]
	m.bytes -= entry.EstimatedBytes()
	return Delivery{kind: DeliveryEntry, entry: entry, replayValid: m.replay}, nil
}

func nha184Sequence(tb testing.TB, capacity int) *Sequencer {
	tb.Helper()
	limits, err := NewQueueLimits(8, 8, 8, capacity)
	if err != nil {
		tb.Fatal(err)
	}
	s, err := NewWithLimits(RealClock{}, 16384, limits)
	if err != nil {
		tb.Fatal(err)
	}
	return s
}

type nha184Identity struct {
	device domain.DeviceID
	entity domain.EntityID
}

func (a nha184Identity) Resolve(ctx context.Context, instance domain.IntegrationInstanceID, keys []integrations.SourceKey) ([]integrations.ResolvedIdentity, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := instance.Value(); err != nil || len(keys) != 1 {
		return nil, integrations.ErrInvalidInput
	}
	if _, _, err := keys[0].Components(); err != nil {
		return nil, err
	}
	return []integrations.ResolvedIdentity{{Device: a.device, Entity: a.entity}}, nil
}

// All six templates have crossed existing admission boundaries. Keeping these
// templates separate lets mailbox tests vary only cursor, quota and delivery.
func nha184AdmittedTemplates(t *testing.T) (*Sequencer, []Entry) {
	t.Helper()
	s := nha184Sequence(t, 16)
	start, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	device, err := domain.ParseDeviceID("nha184-device")
	if err != nil {
		t.Fatal(err)
	}
	entity, err := domain.ParseEntityID("Host:nha184-relay")
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(nha184Identity{device, entity}, s, catalog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := host.CloseAdmissions(); err != nil {
			t.Error(err)
		}
	})
	instance, err := domain.ParseIntegrationInstanceID("nha184-templates")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.EnableNativeMQTTEdges(instance); err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("nha184-device", "relay")
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := integrations.NewCandidate(key, []*wireregistry.CapabilityDescription{{
		SchemaId: contracts.SwitchSchema, Major: 1, DisplayName: "Recorder test relay", Endpoint: "relay",
		Constraints: &wireregistry.CapabilityDescription_Switch{Switch: &wireregistry.SwitchConstraints{}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		t.Fatalf("discover: %d %v", len(bindings), err)
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	value := domain.NewSwitchValue(false)
	observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Publish(context.Background(), bindings[0], observation); err != nil {
		t.Fatal(err)
	}
	event, err := integrations.NewDeviceEvent("nha184.observed", at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Emit(context.Background(), bindings[0], event); err != nil {
		t.Fatal(err)
	}
	health, err := integrations.NewHealthReport(integrations.HealthReady, "nha184")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.ReportHealth(context.Background(), health); err != nil {
		t.Fatal(err)
	}
	alias, err := domain.ParseHAAlias("switch.nha184")
	if err != nil {
		t.Fatal(err)
	}
	record, err := integrations.NewRegistryRecord(device, entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	edge, err := integrations.NewNativeMQTTEdge("nha184/trace", []byte{0, 1, 2, 3}, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.PublishNativeMQTTEdge(context.Background(), edge); err != nil {
		t.Fatal(err)
	}
	entries, _, err := s.ReadAfter(start, 16)
	if err != nil || len(entries) != 6 {
		t.Fatalf("six admitted templates: %d %v", len(entries), err)
	}
	seen := make(map[Kind]bool)
	for _, entry := range entries {
		switch entry.Kind() {
		case KindState, KindEvent, KindHealth, KindRegistry, KindCapabilities, KindNativeMQTTEdge:
			seen[entry.Kind()] = true
		case KindUnspecified:
			t.Fatal("unspecified admitted kind")
		default:
			t.Fatalf("unhandled admitted kind %d", entry.Kind())
		}
	}
	if len(seen) != 6 {
		t.Fatalf("kind coverage: %v", seen)
	}
	return s, entries
}

func nha184CheckModel(t *testing.T, sub *Subscription, m *nha184FIFO) {
	t.Helper()
	status, err := sub.Status()
	if err != nil || status.Class != SubscriptionRecorder || status.Capacity != m.capacity || status.Closed || status.Degraded || status.Coalesced != 0 || status.Queued != len(m.entries) || status.QueuedBytes != m.bytes || status.Lost != m.lost || status.ReplayValid != m.replay || !reflect.DeepEqual(status.PendingLoss, m.loss) {
		t.Fatalf("mailbox/model mismatch: actual=%+v model=%+v err=%v", status, m, err)
	}
	if status.PendingLoss != nil {
		status.PendingLoss.Count = 0
		again, err := sub.Status()
		if err != nil || !reflect.DeepEqual(again.PendingLoss, m.loss) {
			t.Fatal("status loss aliases mailbox")
		}
	}
	sub.owner.mu.Lock()
	defer sub.owner.mu.Unlock()
	if sub.queue != nil || len(sub.recorder.body) != m.capacity || cap(sub.recorder.body) != m.capacity || sub.recorder.count != len(m.entries) || sub.recorder.head < 0 || sub.recorder.head >= m.capacity || sub.recorder.count == 0 && sub.recorder.head != 0 {
		t.Fatal("Recorder has a second store or an incorrect fixed body/occupation")
	}
}

// Full reference/value inspection is linear in the capacity. Keep it at named
// fill/wrap/drain boundaries, rather than making every operation of the FIFO
// trace perform another complete traversal. Every delivery and every status
// invariant is still compared with the independent model at each operation.
func nha184AuditSlots(t *testing.T, sub *Subscription, m *nha184FIFO, checkpoint string) {
	t.Helper()
	nha184CheckModel(t, sub, m)
	sub.owner.mu.Lock()
	defer sub.owner.mu.Unlock()
	live := make(map[int]bool, len(m.entries))
	for i, expected := range m.entries {
		slot := (sub.recorder.head + i) % len(sub.recorder.body)
		live[slot] = true
		if !reflect.DeepEqual(sub.recorder.body[slot], expected) {
			t.Fatalf("%s: ring slot differs from the independent FIFO value", checkpoint)
		}
	}
	for slot, entry := range sub.recorder.body {
		if !live[slot] && !reflect.DeepEqual(entry, Entry{}) {
			t.Fatalf("%s: free slot retains an Entry reference", checkpoint)
		}
	}
}

func nha184ModelRead(t *testing.T, sub *Subscription, m *nha184FIFO) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	want, wantErr := m.next()
	got, err := sub.Next(ctx)
	if !errors.Is(err, wantErr) || !reflect.DeepEqual(got, want) {
		t.Fatalf("delivery/model mismatch: actual=%+v expected=%+v err=%v expectedErr=%v", got, want, err, wantErr)
	}
	nha184CheckModel(t, sub, m)
}

func TestNHA184RecorderMatchesFIFOModel(t *testing.T) {
	_, templates := nha184AdmittedTemplates(t)
	for _, capacity := range []int{1, 2, 3, 16, 64, 256, 1024} {
		t.Run(fmt.Sprintf("capacity=%d", capacity), func(t *testing.T) {
			s := nha184Sequence(t, capacity)
			sub, _, err := s.SubscribeRecorder()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sub.Close() })
			m := &nha184FIFO{capacity: capacity, maxBytes: MaxRecorderQueueBytes, replay: true}
			nha184AuditSlots(t, sub, m, "initial empty")
			filled := false
			for i := 0; i < 4*capacity+37; i++ {
				entry := templates[i%len(templates)]
				entry.cursor = Cursor{epoch: s.epoch, seq: uint64(i + 1)}
				s.mu.Lock()
				s.publish(entry)
				s.mu.Unlock()
				m.push(entry)
				nha184CheckModel(t, sub, m)
				if !filled && (len(m.entries) == capacity || m.lost > 0) {
					filled = true
					nha184AuditSlots(t, sub, m, "first count/byte fill")
				}
				if (i+1)%capacity == 0 {
					nha184AuditSlots(t, sub, m, fmt.Sprintf("publication wrap boundary %d", (i+1)/capacity))
				}
				if i%3 == 1 {
					nha184ModelRead(t, sub, m)
				}
			}
			nha184AuditSlots(t, sub, m, "before final drain")
			for m.loss != nil || len(m.entries) != 0 {
				nha184ModelRead(t, sub, m)
			}
			nha184ModelRead(t, sub, m)
			nha184AuditSlots(t, sub, m, "after drain and empty read")
		})
	}
}

func TestNHA184RecorderLossAndBytesMatchModel(t *testing.T) {
	seed, templates := nha184AdmittedTemplates(t)
	var small, large Entry
	for _, entry := range templates {
		if small.EstimatedBytes() == 0 || entry.EstimatedBytes() < small.EstimatedBytes() {
			small = entry
		}
		if entry.EstimatedBytes() > large.EstimatedBytes() {
			large = entry
		}
	}
	instance, source, _, _ := templates[5].Origin()
	edge, err := integrations.NewNativeMQTTEdge("nha184/large", make([]byte, 8<<10), false, 1)
	if err != nil {
		t.Fatal(err)
	}
	before, err := seed.RecorderCursor()
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.AcceptNativeMQTTEdge(context.Background(), instance, source, edge); err != nil {
		t.Fatal(err)
	}
	admitted, _, err := seed.ReadAfter(before, 1)
	if err != nil || len(admitted) != 1 {
		t.Fatalf("large admitted template: %d %v", len(admitted), err)
	}
	large = admitted[0]
	if large.EstimatedBytes() <= 3*small.EstimatedBytes() {
		t.Fatal("fixture cannot require multiple byte evictions")
	}
	for _, test := range []struct {
		name             string
		capacity, budget int
		entries          []Entry
	}{
		{"count", 2, MaxRecorderQueueBytes, []Entry{small, large, small, large}},
		{"exact-bytes", 8, small.EstimatedBytes() * 2, []Entry{small, small, small}},
		{"multiple-byte-evictions", 8, large.EstimatedBytes(), []Entry{small, small, small, large}},
		{"oversized-empty", 8, small.EstimatedBytes() - 1, []Entry{small}},
		{"oversized-full", 1, small.EstimatedBytes(), []Entry{small, large}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := nha184Sequence(t, test.capacity)
			capture, err := s.CaptureRecorder(nil, Cursor{}, 4<<20, test.budget)
			if err != nil {
				t.Fatal(err)
			}
			sub := capture.Subscription
			t.Cleanup(func() { _ = sub.Close() })
			m := &nha184FIFO{capacity: test.capacity, maxBytes: test.budget, replay: true}
			nha184AuditSlots(t, sub, m, "byte scenario initial empty")
			for i, entry := range test.entries {
				entry.cursor = Cursor{epoch: s.epoch, seq: uint64(i + 1)}
				s.mu.Lock()
				s.publish(entry)
				s.mu.Unlock()
				m.push(entry)
				nha184CheckModel(t, sub, m)
				nha184AuditSlots(t, sub, m, fmt.Sprintf("byte admission boundary %d", i+1))
			}
			nha184AuditSlots(t, sub, m, "byte scenario before drain")
			for m.loss != nil || len(m.entries) != 0 {
				nha184ModelRead(t, sub, m)
			}
			nha184ModelRead(t, sub, m)
			nha184AuditSlots(t, sub, m, "byte scenario after drain and empty read")
		})
	}
	if large.EstimatedBytes() <= small.EstimatedBytes() {
		t.Fatal("byte fixtures do not distinguish size")
	}
}

func TestNHA184RecorderLossCountersSaturate(t *testing.T) {
	_, templates := nha184AdmittedTemplates(t)
	s := nha184Sequence(t, 1)
	sub, _, err := s.SubscribeRecorder()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	entry := templates[0]
	entry.cursor = Cursor{epoch: s.epoch, seq: 1}
	s.mu.Lock()
	s.publish(entry)
	sub.lost = math.MaxUint64 - 1
	sub.loss = &Loss{From: entry.cursor, To: entry.cursor, Count: math.MaxUint64 - 1, Reason: LossRecorderOverflow}
	sub.replayValid = false
	m := &nha184FIFO{entries: []Entry{entry}, bytes: entry.EstimatedBytes(), capacity: 1, maxBytes: MaxRecorderQueueBytes, lost: math.MaxUint64 - 1, loss: &Loss{From: entry.cursor, To: entry.cursor, Count: math.MaxUint64 - 1, Reason: LossRecorderOverflow}}
	for i := uint64(2); i <= 3; i++ {
		entry.cursor.seq = i
		s.publish(entry)
		m.push(entry)
	}
	s.mu.Unlock()
	nha184AuditSlots(t, sub, m, "saturated counters before drain")
	nha184ModelRead(t, sub, m)
	nha184ModelRead(t, sub, m)
	nha184AuditSlots(t, sub, m, "saturated counters after drain")
}

func TestNHA184RecorderCaptureAndRefusalPreserveBoundary(t *testing.T) {
	s, templates := nha184AdmittedTemplates(t)
	capture, err := s.CaptureRecorder(nil, Cursor{}, 4<<20, MaxRecorderQueueBytes)
	if err != nil {
		t.Fatal(err)
	}
	old := capture.Subscription
	t.Cleanup(func() { _ = old.Close() })
	// Real admission advances the boundary without inventing a projected state.
	instance, source, _, _ := templates[5].Origin()
	edge, ok := templates[5].NativeMQTTEdge()
	if !ok {
		t.Fatal("native template order")
	}
	if err := s.AcceptNativeMQTTEdge(context.Background(), instance, source, edge); err != nil {
		t.Fatal(err)
	}
	before, err := old.Status()
	if err != nil {
		t.Fatal(err)
	}
	beforeWake := old.wake
	if _, err := s.CaptureRecorder(old, capture.Cursor, 512, MaxRecorderQueueBytes); !errors.Is(err, ErrRecorderSnapshotTooLarge) {
		t.Fatalf("snapshot refusal: %v", err)
	}
	after, err := old.Status()
	if err != nil || !reflect.DeepEqual(before, after) || beforeWake != old.wake {
		t.Fatal("refusal mutated old mailbox")
	}
	projection, err := s.Capture()
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.CaptureRecorder(old, capture.Cursor, 4<<20, MaxRecorderQueueBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Subscription.Close() })
	if next.Cursor != projection.Cursor || len(next.States) != len(projection.States) || next.Abandoned == nil || next.Abandoned.After != capture.Cursor || next.Abandoned.Through != next.Cursor {
		t.Fatalf("capture boundary: %+v", next)
	}
	for _, state := range next.States {
		if !reflect.DeepEqual(state.State, projection.States[state.Entity]) {
			t.Fatal("snapshot state differs from projection")
		}
	}
	if err := old.Close(); !errors.Is(err, ErrSubscriptionClosed) {
		t.Fatalf("late predecessor close: %v", err)
	}
	if err := s.AcceptNativeMQTTEdge(context.Background(), instance, source, edge); err != nil {
		t.Fatal(err)
	}
	delivery, err := next.Subscription.Next(context.Background())
	entry, present := delivery.Entry()
	if err != nil || !present || entry.Cursor().seq != next.Cursor.seq+1 || !delivery.ReplayValid() {
		t.Fatal("first live entry is not strictly after capture")
	}
	if _, err := old.Next(context.Background()); !errors.Is(err, ErrSubscriptionClosed) {
		t.Fatal("old subscription survived replacement")
	}
}

func TestNHA184RecorderDirect4096AndCapture1024(t *testing.T) {
	s := nha184Sequence(t, MaxQueueCapacity)
	direct, cursor, err := s.SubscribeRecorder()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = direct.Close() })
	status, err := direct.Status()
	if err != nil || status.Capacity != 4096 {
		t.Fatalf("direct capacity: %+v %v", status, err)
	}
	if _, err := s.CaptureRecorder(direct, cursor, 4<<20, MaxRecorderQueueBytes); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("oversized predecessor admitted")
	}
	capture, err := s.CaptureRecorder(nil, Cursor{}, 4<<20, MaxRecorderQueueBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = capture.Subscription.Close() })
	status, err = capture.Subscription.Status()
	if err != nil || status.Capacity != 1024 {
		t.Fatalf("capture capacity: %+v %v", status, err)
	}
}

func TestNHA184RecorderRefusalAndAbandonmentCopyPendingLoss(t *testing.T) {
	s, templates := nha184AdmittedTemplates(t)
	capture, err := s.CaptureRecorder(nil, Cursor{}, 4<<20, MaxRecorderQueueBytes)
	if err != nil {
		t.Fatal(err)
	}
	old := capture.Subscription
	t.Cleanup(func() { _ = old.Close() })
	instance, source, _, _ := templates[5].Origin()
	edge, ok := templates[5].NativeMQTTEdge()
	if !ok {
		t.Fatal("native template missing")
	}
	for i := 0; i < 32; i++ {
		if err := s.AcceptNativeMQTTEdge(context.Background(), instance, source, edge); err != nil {
			t.Fatal(err)
		}
	}
	before, err := old.Status()
	if err != nil || before.PendingLoss == nil || before.PendingLoss.Count != 16 {
		t.Fatalf("pending loss fixture: %+v %v", before, err)
	}
	wake := old.wake
	for _, refusal := range []struct {
		name                      string
		after                     Cursor
		snapshotBytes, queueBytes int
		want                      error
	}{
		{"snapshot", capture.Cursor, 512, MaxRecorderQueueBytes, ErrRecorderSnapshotTooLarge},
		{"queue-zero", capture.Cursor, 4 << 20, 0, ErrInvalidInput},
		{"future-cursor", Cursor{epoch: s.epoch, seq: s.seq + 1}, 4 << 20, MaxRecorderQueueBytes, ErrInvalidInput},
		{"zero-cursor", Cursor{}, 4 << 20, MaxRecorderQueueBytes, ErrInvalidInput},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			if _, err := s.CaptureRecorder(old, refusal.after, refusal.snapshotBytes, refusal.queueBytes); !errors.Is(err, refusal.want) {
				t.Fatalf("refusal: %v want %v", err, refusal.want)
			}
			after, err := old.Status()
			if err != nil || !reflect.DeepEqual(after, before) || old.wake != wake {
				t.Fatal("refusal changed pending suffix/loss/lifecycle")
			}
		})
	}
	next, err := s.CaptureRecorder(old, capture.Cursor, 4<<20, MaxRecorderQueueBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Subscription.Close() })
	if next.Abandoned == nil || !reflect.DeepEqual(next.Abandoned.PendingLoss, before.PendingLoss) || next.Abandoned.After != capture.Cursor || next.Abandoned.Through != next.Cursor {
		t.Fatalf("abandonment: %+v", next.Abandoned)
	}
	next.Abandoned.PendingLoss.Count = 0
	closed, err := old.Status()
	if err != nil || !closed.Closed || closed.Queued != 0 || closed.QueuedBytes != 0 || !reflect.DeepEqual(closed.PendingLoss, before.PendingLoss) {
		t.Fatal("abandonment loss aliases the predecessor")
	}
	status, err := next.Subscription.Status()
	if err != nil || status.Queued != 0 || status.QueuedBytes != 0 || status.PendingLoss != nil || !status.ReplayValid {
		t.Fatalf("replacement starts with stale suffix/loss: %+v %v", status, err)
	}
}

type nha184WaitingContext struct {
	context.Context
	once    sync.Once
	waiting chan struct{}
}

func (c *nha184WaitingContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestNHA184RecorderCloseAndWake(t *testing.T) {
	_, templates := nha184AdmittedTemplates(t)
	for _, action := range []string{"publish", "close", "replace", "cancel"} {
		t.Run(action, func(t *testing.T) {
			s := nha184Sequence(t, 3)
			sub, cursor, err := s.SubscribeRecorder()
			if err != nil {
				t.Fatal(err)
			}
			base, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			ctx := &nha184WaitingContext{Context: base, waiting: make(chan struct{})}
			result := make(chan error, 1)
			done := make(chan struct{})
			go func() { _, err := sub.Next(ctx); result <- err; close(done) }()
			t.Cleanup(func() { cancel(); _ = sub.Close(); <-done })
			select {
			case <-ctx.waiting:
			case <-base.Done():
				t.Fatal("Next did not wait")
			}
			wantErr := error(nil)
			switch action {
			case "publish":
				entry := templates[0]
				entry.cursor = Cursor{epoch: s.epoch, seq: 1}
				s.mu.Lock()
				s.publish(entry)
				s.mu.Unlock()
			case "close":
				if err := sub.Close(); err != nil {
					t.Fatal(err)
				}
				wantErr = ErrSubscriptionClosed
			case "replace":
				capture, err := s.CaptureRecorder(sub, cursor, 4<<20, MaxRecorderQueueBytes)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = capture.Subscription.Close() })
				wantErr = ErrSubscriptionClosed
			case "cancel":
				cancel()
				wantErr = context.Canceled
			default:
				t.Fatal("unknown lifecycle action")
			}
			select {
			case err := <-result:
				if !errors.Is(err, wantErr) {
					t.Fatalf("wake error: %v want %v", err, wantErr)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Next did not wake")
			}
		})
	}
}

func TestNHA184RecorderQueueBoundsAndZero(t *testing.T) {
	for _, capacity := range []int{-1, 0, MaxQueueCapacity + 1, math.MaxInt} {
		queue, err := newRecorderQueue(capacity)
		if !errors.Is(err, ErrInvalidInput) || queue.body != nil || queue.head != 0 || queue.count != 0 {
			t.Fatalf("invalid capacity allocated/admitted: %d %+v %v", capacity, queue, err)
		}
	}
	var absent recorderQueue
	if _, ok := absent.pop(); ok || absent.len() != 0 || absent.release() != nil {
		t.Fatal("zero queue is not absent")
	}
	for _, capacity := range []int{1, 2, 3, 16, 1024, 4096} {
		queue, err := newRecorderQueue(capacity)
		if err != nil || len(queue.body) != capacity || cap(queue.body) != capacity || queue.len() != 0 {
			t.Fatalf("fixed body admission: %d %+v %v", capacity, queue, err)
		}
		// A slot's payload value never defines its occupation. Domain admission
		// belongs to the sequencer; the storage distinguishes present zero/empty.
		queue.push(Entry{})
		if queue.len() != 1 {
			t.Fatal("zero value was confused with an empty slot")
		}
		entry, ok := queue.pop()
		if !ok || !reflect.DeepEqual(entry, Entry{}) || queue.len() != 0 {
			t.Fatal("present zero was not delivered")
		}
		queue.release()
	}
	_, templates := nha184AdmittedTemplates(t)
	for _, name := range []string{"absent", "full"} {
		t.Run(name, func(t *testing.T) {
			var queue recorderQueue
			if name == "full" {
				queue, _ = newRecorderQueue(1)
				queue.push(templates[0])
			}
			before := queue
			before.body = append([]Entry(nil), queue.body...)
			defer func() {
				if recover() == nil {
					t.Error("invalid internal push did not fail explicitly")
				}
				if !reflect.DeepEqual(queue, before) {
					t.Error("invalid internal push mutated the queue")
				}
			}()
			queue.push(templates[1])
		})
	}
}

func TestNHA184RecorderQueuePreservesSurvivorSlotsAndClearsReferences(t *testing.T) {
	_, templates := nha184AdmittedTemplates(t)
	queue, err := newRecorderQueue(3)
	if err != nil {
		t.Fatal(err)
	}
	body := queue.body
	for i := 0; i < 3; i++ {
		queue.push(templates[i])
	}
	for turn := 0; turn < 12; turn++ {
		oldHead := queue.head
		firstSurvivor := (oldHead + 1) % len(body)
		lastSurvivor := (oldHead + 2) % len(body)
		firstAddress, lastAddress := &body[firstSurvivor], &body[lastSurvivor]
		firstValue, lastValue := body[firstSurvivor], body[lastSurvivor]
		if _, ok := queue.pop(); !ok {
			t.Fatal("full queue unexpectedly empty")
		}
		if !reflect.DeepEqual(body[oldHead], Entry{}) {
			t.Fatal("pop retains payload references in its freed slot")
		}
		if &body[queue.head] != firstAddress || &body[(queue.head+1)%len(body)] != lastAddress || !reflect.DeepEqual(*firstAddress, firstValue) || !reflect.DeepEqual(*lastAddress, lastValue) {
			t.Fatal("pop moved or changed a survivor")
		}
		queue.push(templates[(turn+3)%len(templates)])
		if len(queue.body) != 3 || cap(queue.body) != 3 || &queue.body[0] != &body[0] || queue.count != 3 {
			t.Fatal("push grew/replaced the fixed body")
		}
	}
	detached := queue.release()
	if &detached[0] != &body[0] || queue.body != nil || queue.head != 0 || queue.count != 0 {
		t.Fatal("release did not detach the sole body")
	}
	for _, entry := range body {
		if !reflect.DeepEqual(entry, Entry{}) {
			t.Fatal("released body keeps a GC-visible payload reference")
		}
	}
	queue = recorderQueue{body: detached}
	if allocations := testing.AllocsPerRun(100, func() { queue.push(templates[0]); _, _ = queue.pop() }); allocations != 0 {
		t.Fatalf("push/pop allocated: %g allocations/run", allocations)
	}
	queue.release()
}

func TestNHA184RecorderStorageIsExclusiveByClass(t *testing.T) {
	s := nha184Sequence(t, 16)
	for _, class := range []SubscriptionClass{SubscriptionUI, SubscriptionEdge, SubscriptionView, SubscriptionRecorder} {
		sub, _, err := s.subscribe(class)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sub.Close() })
		if class == SubscriptionRecorder {
			if sub.queue != nil || len(sub.recorder.body) != 16 || sub.recorder.count != 0 {
				t.Fatal("Recorder has duplicate or missing storage")
			}
		} else if sub.queue == nil || sub.recorder.body != nil || sub.recorder.head != 0 || sub.recorder.count != 0 {
			t.Fatal("another class owns Recorder storage")
		}
	}
}

func TestNHA184RecorderCaptureTransfersOneBodyAndCloseReleasesIt(t *testing.T) {
	s, templates := nha184AdmittedTemplates(t)
	capture, err := s.CaptureRecorder(nil, Cursor{}, 4<<20, MaxRecorderQueueBytes)
	if err != nil {
		t.Fatal(err)
	}
	old := capture.Subscription
	t.Cleanup(func() { _ = old.Close() })
	body := old.recorder.body
	instance, source, _, _ := templates[5].Origin()
	edge, ok := templates[5].NativeMQTTEdge()
	if !ok {
		t.Fatal("native template missing")
	}
	for i := 0; i < 32; i++ {
		if err := s.AcceptNativeMQTTEdge(context.Background(), instance, source, edge); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := old.Next(context.Background()); !errors.Is(err, ErrRecorderGap) {
		t.Fatal("initial saturation loss missing")
	}
	if _, err := old.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if err := s.AcceptNativeMQTTEdge(context.Background(), instance, source, edge); err != nil {
			t.Fatal(err)
		}
	}
	if old.recorder.head == 0 || old.recorder.count != 16 {
		t.Fatal("fixture did not wrap its full body")
	}
	wantSlots := append([]Entry(nil), body...)
	head, count, bytes := old.recorder.head, old.recorder.count, old.queueBytes
	before, err := old.Status()
	if err != nil {
		t.Fatal(err)
	}
	wake := old.wake
	if _, err := s.CaptureRecorder(old, capture.Cursor, 512, MaxRecorderQueueBytes); !errors.Is(err, ErrRecorderSnapshotTooLarge) {
		t.Fatalf("snapshot refusal: %v", err)
	}
	after, err := old.Status()
	if err != nil || &old.recorder.body[0] != &body[0] || old.recorder.head != head || old.recorder.count != count || old.queueBytes != bytes || old.wake != wake || !reflect.DeepEqual(body, wantSlots) || !reflect.DeepEqual(before, after) {
		t.Fatal("refusal mutated storage, references, accounting or lifecycle")
	}
	next, err := s.CaptureRecorder(old, capture.Cursor, 4<<20, MaxRecorderQueueBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Subscription.Close() })
	if next.Subscription.queue != nil || &next.Subscription.recorder.body[0] != &body[0] || len(next.Subscription.recorder.body) != 16 || next.Subscription.recorder.count != 0 || old.recorder.body != nil || old.recorder.head != 0 || old.recorder.count != 0 || old.queueBytes != 0 {
		t.Fatal("capture did not transfer one fixed body from a zeroed predecessor")
	}
	for _, entry := range body {
		if !reflect.DeepEqual(entry, Entry{}) {
			t.Fatal("capture retained a reference to an abandoned Entry")
		}
	}
	if err := s.AcceptNativeMQTTEdge(context.Background(), instance, source, edge); err != nil {
		t.Fatal(err)
	}
	live := body[0]
	if err := old.Close(); !errors.Is(err, ErrSubscriptionClosed) || !reflect.DeepEqual(body[0], live) {
		t.Fatal("late predecessor close affected successor storage")
	}
	if err := next.Subscription.Close(); err != nil {
		t.Fatal(err)
	}
	if next.Subscription.recorder.body != nil || next.Subscription.recorder.count != 0 || next.Subscription.queueBytes != 0 {
		t.Fatal("close kept an owned body or charge")
	}
	for _, entry := range body {
		if !reflect.DeepEqual(entry, Entry{}) {
			t.Fatal("close retained a GC-visible Entry reference")
		}
	}
}

// Inspect one admission-mutex snapshot against the journal, which is independent
// of the mailbox representation. This also checks that every non-live slot has
// dropped its references; no finalizer or heap-size threshold is involved.
func nha184ConcurrentRecorderInvariant(s *Sequencer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.subs) > 1 {
		return errors.New("concurrent replacement left multiple active subscriptions")
	}
	for sub := range s.subs {
		q := &sub.recorder
		if sub.class != SubscriptionRecorder || sub.closed || sub.queue != nil || len(q.body) != sub.capacity || cap(q.body) != sub.capacity || q.count < 0 || q.count > sub.capacity || q.head < 0 || q.head >= len(q.body) || q.count == 0 && q.head != 0 {
			return errors.New("concurrent mailbox ownership/occupation is invalid")
		}
		live := make(map[int]bool, q.count)
		bytes := 0
		var previous uint64
		for i := 0; i < q.count; i++ {
			slot := (q.head + i) % len(q.body)
			live[slot] = true
			entry := q.body[slot]
			if entry.cursor.seq == 0 || entry.cursor.seq <= previous || entry.cursor.seq > s.seq || !reflect.DeepEqual(entry, s.journal[(entry.cursor.seq-1)%uint64(len(s.journal))]) {
				return errors.New("concurrent FIFO slot differs from independent admission journal")
			}
			previous = entry.cursor.seq
			bytes += entry.EstimatedBytes()
		}
		if bytes != sub.queueBytes || bytes < 0 || bytes > sub.maxBytes {
			return errors.New("concurrent byte ledger differs from retained entries")
		}
		for slot, entry := range q.body {
			if !live[slot] && !reflect.DeepEqual(entry, Entry{}) {
				return errors.New("concurrent free slot retains a reference")
			}
		}
	}
	return nil
}

func TestNHA184RecorderConcurrentLifecycle(t *testing.T) {
	s := nha184Sequence(t, 16)
	sub, initial, err := s.SubscribeRecorder()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type currentRecorder struct {
		sync.Mutex
		sub      *Subscription
		boundary Cursor
		changed  chan struct{}
	}
	current := &currentRecorder{sub: sub, boundary: initial, changed: make(chan struct{})}
	get := func() (*Subscription, Cursor, <-chan struct{}) {
		current.Lock()
		defer current.Unlock()
		return current.sub, current.boundary, current.changed
	}
	failures := make(chan error, 8)
	fail := func(err error) {
		select {
		case failures <- err:
		default:
		}
		cancel()
	}
	publish := func(producer, position int) error {
		instance, err := domain.ParseIntegrationInstanceID(fmt.Sprintf("nha184-concurrent-%d", producer))
		if err != nil {
			return err
		}
		seq := uint64(position + 1)
		source, err := domain.NewSourceStamp(fmt.Sprintf("nha184-producer-%d", producer), &seq)
		if err != nil {
			return err
		}
		payload := make([]byte, 64)
		payload[0], payload[1] = byte(producer), byte(position)
		edge, err := integrations.NewNativeMQTTEdge("nha184/concurrent", payload, false, seq)
		if err != nil {
			return err
		}
		return s.AcceptNativeMQTTEdge(ctx, instance, source, edge)
	}
	if err := publish(0, 0); err != nil {
		t.Fatal(err)
	}
	start, readerReady, readerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(readerDone)
		last := uint64(0)
		first := true
		for {
			stream, boundary, changed := get()
			delivery, err := stream.Next(ctx)
			if first {
				first = false
				close(readerReady)
				if err != nil {
					fail(fmt.Errorf("initial available entry: %w", err))
					return
				}
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			if errors.Is(err, ErrSubscriptionClosed) {
				select {
				case <-changed:
					continue
				case <-ctx.Done():
					return
				}
			}
			if errors.Is(err, ErrRecorderGap) {
				loss, ok := delivery.Loss()
				if !ok || loss.Reason != LossRecorderOverflow || loss.Count == 0 || loss.From.epoch != s.epoch || loss.To.epoch != s.epoch || loss.From.seq > loss.To.seq || loss.Count > loss.To.seq-loss.From.seq+1 {
					fail(errors.New("invalid concurrent loss delivery"))
					return
				}
			} else {
				entry, ok := delivery.Entry()
				if err != nil || !ok || entry.cursor.seq <= last || entry.cursor.seq <= boundary.seq {
					fail(fmt.Errorf("concurrent delivery is not FIFO/live: cursor=%v last=%d boundary=%v err=%v", entry.cursor, last, boundary, err))
					return
				}
				journal, _, err := s.ReadAfter(Cursor{epoch: s.epoch, seq: entry.cursor.seq - 1}, 1)
				if err != nil || len(journal) != 1 || !reflect.DeepEqual(entry, journal[0]) {
					fail(errors.New("concurrent delivery differs from admitted journal"))
					return
				}
				last = entry.cursor.seq
			}
			if last == 1 {
				select {
				case <-start:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	var workers sync.WaitGroup
	run := func(work func() error) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			select {
			case <-start:
			case <-ctx.Done():
				return
			}
			if err := work(); err != nil && ctx.Err() == nil {
				fail(err)
			}
		}()
	}
	for producer := 1; producer <= 2; producer++ {
		run(func() error {
			for position := 0; position < 256; position++ {
				if err := publish(producer, position); err != nil {
					return err
				}
			}
			return nil
		})
	}
	run(func() error {
		for i := 0; i < 64; i++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			current.Lock()
			previous, after := current.sub, current.boundary
			capture, err := s.CaptureRecorder(previous, after, 4<<20, MaxRecorderQueueBytes)
			if errors.Is(err, ErrInvalidInput) {
				status, statusErr := previous.Status()
				if statusErr != nil || !status.Closed {
					current.Unlock()
					return errors.New("capture refused an active current subscription")
				}
				capture, err = s.CaptureRecorder(nil, Cursor{}, 4<<20, MaxRecorderQueueBytes)
			}
			if err != nil {
				current.Unlock()
				return err
			}
			if capture.Cursor.seq < after.seq || len(capture.States) != 0 {
				current.Unlock()
				return errors.New("concurrent capture boundary/image is invalid")
			}
			current.sub, current.boundary = capture.Subscription, capture.Cursor
			close(current.changed)
			current.changed = make(chan struct{})
			current.Unlock()
			if _, err := previous.Next(ctx); !errors.Is(err, ErrSubscriptionClosed) {
				return errors.New("replaced predecessor remained readable")
			}
		}
		return nil
	})
	run(func() error {
		for i := 0; i < 64; i++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			stream, _, _ := get()
			if err := stream.Close(); err != nil && !errors.Is(err, ErrSubscriptionClosed) {
				return err
			}
		}
		return nil
	})
	run(func() error {
		for i := 0; i < 256; i++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			stream, _, _ := get()
			status, err := stream.Status()
			if err != nil || status.Queued < 0 || status.Queued > status.Capacity || status.QueuedBytes < 0 || status.QueuedBytes > MaxRecorderQueueBytes || status.Closed && (status.Queued != 0 || status.QueuedBytes != 0) {
				return errors.New("concurrent status is outside admitted bounds")
			}
			if status.PendingLoss != nil {
				status.PendingLoss.Count = 0
				fresh, err := stream.Status()
				if err != nil || fresh.PendingLoss != nil && fresh.PendingLoss.Count == 0 {
					return errors.New("concurrent status loss aliases the mailbox")
				}
			}
			if err := nha184ConcurrentRecorderInvariant(s); err != nil {
				return err
			}
		}
		return nil
	})
	workersDone := make(chan struct{})
	go func() { workers.Wait(); close(workersDone) }()
	t.Cleanup(func() {
		cancel()
		<-workersDone
		<-readerDone
		// Also release a successor whose validation failed before it could be
		// installed in the test's current pointer. No goroutine remains here.
		s.mu.Lock()
		streams := make([]*Subscription, 0, len(s.subs))
		for stream := range s.subs {
			streams = append(streams, stream)
		}
		s.mu.Unlock()
		for _, stream := range streams {
			_ = stream.Close()
		}
	})
	select {
	case <-readerReady:
	case <-ctx.Done():
		t.Fatal("reader did not consume its initial entry")
	}
	close(start)
	select {
	case <-workersDone:
	case <-ctx.Done():
		t.Fatal("concurrent workers did not join")
	}
	cancel()
	<-readerDone
	select {
	case err := <-failures:
		t.Fatal(err)
	default:
	}
	if err := nha184ConcurrentRecorderInvariant(s); err != nil {
		t.Fatal(err)
	}
	end, err := s.RecorderCursor()
	if err != nil || end.seq != initial.seq+513 {
		t.Fatalf("producers did not preserve the admission trace: end=%v err=%v", end, err)
	}
}
