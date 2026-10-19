package recorder

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
)

// The fixture's identity authority is reused, while the real sequencer is the
// Host sink. The generic fake sink does not expose native MQTT admission.
func newNHA184WorkerFixture(t *testing.T) *nha183Fixture {
	t.Helper()
	original, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	if err := original.CloseAdmissions(); err != nil {
		t.Fatal(err)
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(fixture, fixture.Sequencer(), catalog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := host.CloseAdmissions(); err != nil {
			t.Error(err)
		}
	})
	instance, err := domain.ParseIntegrationInstanceID("nha184-worker")
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
	driver, err := fake.NewDriver(time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	f := &nha183Fixture{clock: newNHA183Clock(), host: host, fixture: fixture, session: session, driver: driver, writer: &nha030Writer{updated: make(chan struct{}, 8)}}
	f.spy = &nha183ObservedWriter{writer: f.writer, clock: f.clock}
	f.startPilot(t)
	return f
}

func nha184ImageBatch(t *testing.T, snapshot sequencer.Snapshot, epoch storage.RecorderEpoch, from uint64, knownLost uint64) storage.RecorderBatch {
	t.Helper()
	_, through, err := snapshot.Cursor.Components()
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]domain.EntityID, 0, len(snapshot.States))
	for id := range snapshot.States {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { left, _ := ids[i].Value(); right, _ := ids[j].Value(); return left < right })
	rows := make([]storage.RecorderSnapshotRecord, 0, len(ids))
	for _, id := range ids {
		row, err := storage.NewRecorderSnapshotRecord(snapshot.States[id], maxRecordPayloadBytes)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	var gaps []storage.RecorderGap
	if through > from {
		var count *uint64
		if knownLost != 0 {
			count = &knownLost
		}
		gaps = []storage.RecorderGap{{From: from + 1, Through: through, KnownLost: count, Reason: storage.RecorderGapReconciliation, UncertainEnvelope: true}}
	}
	batch, err := storage.NewRecorderBatch(epoch, from, through, nil, gaps, 0, &storage.RecorderImage{Boundary: through, States: rows})
	if err != nil {
		t.Fatal(err)
	}
	return batch
}

func nha184BeforeWorker(t *testing.T, f *nha183Fixture) (storage.RecorderEpoch, uint64, storage.RecorderBatch) {
	t.Helper()
	snapshot, err := f.fixture.Sequencer().Capture()
	if err != nil {
		t.Fatal(err)
	}
	epochText, boundary, err := snapshot.Cursor.Components()
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := storage.ParseRecorderEpoch(epochText)
	if err != nil {
		t.Fatal(err)
	}
	return epoch, boundary, nha184ImageBatch(t, snapshot, epoch, 0, 0)
}

func nha184AssertBatch(t *testing.T, f *nha183Fixture, index int, expected storage.RecorderBatch) {
	t.Helper()
	batches, checkpoint := nha183Batches(f.writer)
	if len(batches) != index+1 {
		t.Fatalf("batch count: got %d want %d", len(batches), index+1)
	}
	actual := batches[index]
	if actual.Epoch() != expected.Epoch() || actual.From() != expected.From() || actual.Through() != expected.Through() || actual.StateCount() != expected.StateCount() || actual.HasImage() != expected.HasImage() || actual.HasGaps() != expected.HasGaps() || actual.Digest() != expected.Digest() || !checkpoint.Confirms(expected) {
		t.Fatalf("batch %d differs from the independently fixed boundary: from=%d through=%d states=%d checkpoint=%+v", index, actual.From(), actual.Through(), actual.StateCount(), checkpoint)
	}
	status, err := f.recording.Status()
	if err != nil || status.Checkpoint != expected.Through() {
		t.Fatalf("worker checkpoint: %+v %v", status, err)
	}
}

func nha184PublishExcludedKinds(t *testing.T, f *nha183Fixture) {
	t.Helper()
	f.publishEvent(t)
	f.publishHealth(t)
	device, entity, err := f.binding.IDs()
	if err != nil {
		t.Fatal(err)
	}
	instance, key, err := f.binding.Source()
	if err != nil {
		t.Fatal(err)
	}
	alias, err := domain.ParseHAAlias("switch.nha184_worker")
	if err != nil {
		t.Fatal(err)
	}
	record, err := integrations.NewRegistryRecord(device, entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.fixture.Sequencer().PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	candidate, err := integrations.NewCandidate(key, []*wireregistry.CapabilityDescription{{
		SchemaId: contracts.SwitchSchema, Major: 1, DisplayName: "Recorder worker relay", Endpoint: "relay",
		Constraints: &wireregistry.CapabilityDescription_Switch{Switch: &wireregistry.SwitchConstraints{}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.session.Discover(context.Background(), []integrations.Candidate{candidate}); err != nil {
		t.Fatal(err)
	}
	edge, err := integrations.NewNativeMQTTEdge("nha184/worker", []byte{0, 1, 0, 2}, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.session.PublishNativeMQTTEdge(context.Background(), edge); err != nil {
		t.Fatal(err)
	}
}

func TestNHA184RecorderFixedTraceBatches(t *testing.T) {
	f := newNHA184WorkerFixture(t)
	epoch, base, initial := nha184BeforeWorker(t, f)
	before := f.startRecorder(t)
	if f.baseSequence != base {
		t.Fatal("startup boundary changed")
	}
	nha184AssertBatch(t, f, 0, initial)
	f.publishStates(t, 499)
	before = f.awaitPrefix(t, before, 499)
	nha184PublishExcludedKinds(t, f)
	before = f.awaitPrefix(t, before, 5)
	f.advance(t, 249*time.Millisecond)
	f.assertBatchCount(t, 1)
	f.publishStates(t, 1)
	before = f.awaitPrefix(t, before, 1)
	quantityThrough := base + 505 // 500 states and exactly five policy exclusions.
	expected := nha183ExpectedBatch(t, f.fixture.Sequencer(), epoch, base, quantityThrough)
	nha184AssertBatch(t, f, 1, expected)
	entries, _, err := f.fixture.Sequencer().ReadAfter(f.base, 505)
	if err != nil || len(entries) != 505 {
		t.Fatalf("fixed trace: %d %v", len(entries), err)
	}
	seen := make(map[sequencer.Kind]bool)
	for _, entry := range entries {
		seen[entry.Kind()] = true
	}
	for _, kind := range []sequencer.Kind{sequencer.KindState, sequencer.KindEvent, sequencer.KindHealth, sequencer.KindRegistry, sequencer.KindCapabilities, sequencer.KindNativeMQTTEdge} {
		if !seen[kind] {
			t.Fatalf("fixed trace omitted admitted kind %d", kind)
		}
	}
	f.publishStates(t, 7)
	f.publishEvent(t)
	before = f.awaitPrefix(t, before, 8)
	f.assertBatchCount(t, 2)
	f.advance(t, 498*time.Millisecond)
	f.assertBatchCount(t, 2)
	f.advance(t, 499*time.Millisecond)
	f.clock.awaitRead(t, before.generation+1)
	expected = nha183ExpectedBatch(t, f.fixture.Sequencer(), epoch, quantityThrough, quantityThrough+8)
	nha184AssertBatch(t, f, 2, expected)
	status, err := f.recording.Status()
	if err != nil || status.LostEntries != 0 || status.PolicyExcluded != 6 || !status.ReconciliationComplete || status.HistoryDegraded != f.initialDegraded {
		t.Fatalf("lossless coverage changed: %+v %v", status, err)
	}
}

// This independent list model uses admitted charges and counts positions, not
// the width of a loss interval. It is evaluated while the worker is in SQL.
func nha184BlockedSuffix(t *testing.T, entries []sequencer.Entry, capacity, maxBytes int) ([]sequencer.Entry, int, uint64) {
	t.Helper()
	var suffix []sequencer.Entry
	bytes := 0
	var lost uint64
	for _, entry := range entries {
		charge := entry.EstimatedBytes()
		if charge <= 0 || charge > maxBytes {
			t.Fatal("state burst contains an oversized entry")
		}
		for len(suffix)+1 > capacity || bytes+charge > maxBytes {
			bytes -= suffix[0].EstimatedBytes()
			suffix = suffix[1:]
			lost++
		}
		suffix = append(suffix, entry)
		bytes += charge
	}
	return suffix, bytes, lost
}

func nha184PublishBurst(ctx context.Context, f *nha183Fixture, count int) error {
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		return err
	}
	at := time.Date(2026, 10, 19, 12, 0, 1, 0, time.UTC)
	for i := 0; i < count; i++ {
		value := domain.NewSwitchValue(i%2 == 0)
		observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
		if err != nil {
			return err
		}
		if err := f.session.Publish(ctx, f.binding, observation); err != nil {
			return fmt.Errorf("admission %d: %w", i, err)
		}
	}
	return nil
}

func TestNHA184RecorderBlockedWriterReconcilesFixedTrace(t *testing.T) {
	f := newNHA184WorkerFixture(t)
	f.writer.blockSecond = make(chan struct{})
	f.writer.secondEntered = make(chan struct{})
	f.writer.secondFinished = make(chan struct{})
	epoch, base, initial := nha184BeforeWorker(t, f)
	before := f.startRecorder(t)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(f.writer.blockSecond) }) }
	t.Cleanup(release) // Runs before the fixture's Stop/join cleanup.
	nha184AssertBatch(t, f, 0, initial)
	f.publishStates(t, FlushStates)
	select {
	case <-f.writer.secondEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("identified state batch did not enter writer")
	}
	const burst = 2048
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	progress := make(chan error, 1)
	producerDone := make(chan struct{})
	go func() { progress <- nha184PublishBurst(ctx, f, burst); close(producerDone) }()
	t.Cleanup(func() { cancel(); release(); <-producerDone })
	select {
	case err := <-progress:
		if err != nil {
			t.Fatalf("Host stopped before writer release: %v", err)
		}
	case <-time.After(18 * time.Second):
		t.Fatal("Host admissions waited for the blocked writer")
	}
	<-producerDone
	cancel()
	sequence := f.fixture.Sequencer()
	burstStart, err := sequencer.ParseCursor(mustNHA184Epoch(t, epoch), base+FlushStates)
	if err != nil {
		t.Fatal(err)
	}
	entries, final, err := sequence.ReadAfter(burstStart, burst)
	if err != nil || len(entries) != burst {
		t.Fatalf("blocked journal trace: %d %v", len(entries), err)
	}
	f.recording.mu.Lock()
	sub := f.recording.sub
	f.recording.mu.Unlock()
	queue, err := sub.Status()
	if err != nil || queue.Capacity != 1024 {
		t.Fatalf("production queue count limit: %+v %v", queue, err)
	}
	suffix, queuedBytes, knownLost := nha184BlockedSuffix(t, entries, queue.Capacity, DefaultLimits().QueueBytes())
	if knownLost == 0 || len(suffix) == 0 || queue.Queued != len(suffix) || queue.QueuedBytes != queuedBytes || queue.Lost != knownLost || queue.ReplayValid || queue.PendingLoss == nil || queue.PendingLoss.From != entries[0].Cursor() || queue.PendingLoss.To != entries[knownLost-1].Cursor() || queue.PendingLoss.Count != knownLost {
		t.Fatalf("blocked suffix/loss differs from list model: queue=%+v suffix=%d bytes=%d lost=%d", queue, len(suffix), queuedBytes, knownLost)
	}
	status, err := f.recording.Status()
	if err != nil || status.Checkpoint != base || status.QueuedEntries != len(suffix) || status.QueuedBytes != queuedBytes || status.QueuedBytes > DefaultLimits().QueueBytes() {
		t.Fatalf("blocked checkpoint/budget: %+v %v", status, err)
	}
	snapshot, err := sequence.Capture()
	if err != nil || snapshot.Cursor != final {
		t.Fatalf("final producer boundary: %v", err)
	}
	quantityThrough := base + FlushStates
	expectedQuantity := nha183ExpectedBatch(t, sequence, epoch, base, quantityThrough)
	expectedImage := nha184ImageBatch(t, snapshot, epoch, quantityThrough, knownLost)
	release()
	after := f.clock.awaitRead(t, before.generation+FlushStates+1)
	batches, checkpoint := nha183Batches(f.writer)
	if len(batches) != 3 || batches[1].Digest() != expectedQuantity.Digest() || !checkpoint.Confirms(expectedImage) {
		t.Fatal("fixed quantity/image trace changed")
	}
	nha184AssertBatch(t, f, 2, expectedImage)
	select {
	case <-f.writer.secondFinished:
	case <-time.After(3 * time.Second):
		t.Fatal("blocked writer did not finish")
	}
	status, err = f.recording.Status()
	if err != nil || status.LostEntries != knownLost || !status.HistoryDegraded || !status.ReconciliationComplete || status.QueuedEntries != 0 {
		t.Fatalf("reconciled coverage: %+v %v", status, err)
	}
	f.publishStates(t, 7)
	f.publishEvent(t)
	after = f.clock.awaitRead(t, after.generation+8)
	f.assertBatchCount(t, 3)
	f.advance(t, 250*time.Millisecond)
	f.clock.awaitRead(t, after.generation+1)
	_, finalSequence, err := final.Components()
	if err != nil {
		t.Fatal(err)
	}
	expectedSuffix := nha183ExpectedBatch(t, sequence, epoch, finalSequence, finalSequence+8)
	nha184AssertBatch(t, f, 3, expectedSuffix)
	status, err = f.recording.Status()
	if err != nil || status.LostEntries != knownLost || !status.HistoryDegraded || !status.ReconciliationComplete || status.PolicyExcluded != 1 {
		t.Fatalf("suffix coverage: %+v %v", status, err)
	}
}

func mustNHA184Epoch(t *testing.T, epoch storage.RecorderEpoch) string {
	t.Helper()
	text, err := epoch.Hex()
	if err != nil {
		t.Fatal(err)
	}
	return text
}
