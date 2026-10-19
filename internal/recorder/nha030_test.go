package recorder

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/storage"
)

type nha030Writer struct {
	mu                sync.Mutex
	checkpoint        storage.RecorderCheckpoint
	batches           []storage.RecorderBatch
	writes            int
	reads             int
	indeterminateOnce bool
	readFailureOnce   bool
	updated           chan struct{}
	blockSecond       chan struct{}
	secondEntered     chan struct{}
	secondFinished    chan struct{}
}

type nha030TypedNilContext struct{}

func (*nha030TypedNilContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (*nha030TypedNilContext) Done() <-chan struct{}       { return nil }
func (*nha030TypedNilContext) Err() error                  { return nil }
func (*nha030TypedNilContext) Value(any) any               { return nil }

func TestNHA030RejectsTypedNilContextAtLifecycleBoundary(t *testing.T) {
	_, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	var typedNil context.Context = (*nha030TypedNilContext)(nil)
	writer := &nha030Writer{}
	if _, err := Start(typedNil, fixture.Sequencer(), writer, DefaultLimits()); !errors.Is(err, ErrInvalidRecorder) {
		t.Fatalf("typed nil start context accepted: %v", err)
	}
	recording, err := Start(context.Background(), fixture.Sequencer(), writer, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := recording.Stop(typedNil); !errors.Is(err, ErrInvalidRecorder) {
		t.Fatalf("typed nil stop context accepted: %v", err)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := recording.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}

func (w *nha030Writer) OpenRecorderEpoch(context.Context, storage.RecorderEpoch, time.Time) (storage.RecorderPrevious, storage.TransactionOutcome, error) {
	return storage.RecorderPrevious{}, storage.TransactionCommitted, nil
}

func (w *nha030Writer) PersistRecorderBatch(_ context.Context, batch storage.RecorderBatch) (storage.TransactionOutcome, error) {
	w.mu.Lock()
	w.writes++
	second := w.writes == 2 && w.blockSecond != nil
	w.mu.Unlock()
	if second {
		close(w.secondEntered)
		<-w.blockSecond
		defer close(w.secondFinished)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if batch.From() != w.checkpoint.Sequence {
		return storage.TransactionNotCommitted, storage.ErrRecorderIntegrity
	}
	complete := w.checkpoint.ReconciliationComplete
	if batch.HasImage() {
		complete = true
	} else if batch.HasGaps() {
		complete = false
	}
	w.checkpoint = storage.RecorderCheckpoint{Sequence: batch.Through(), LastFrom: batch.From(), LastThrough: batch.Through(), LastDigest: batch.Digest(), HasLastBatch: true, ReconciliationComplete: complete}
	w.batches = append(w.batches, batch)
	if w.updated != nil {
		select {
		case w.updated <- struct{}{}:
		default:
		}
	}
	if w.indeterminateOnce {
		w.indeterminateOnce = false
		return storage.TransactionIndeterminate, errors.New("ack lost after commit")
	}
	return storage.TransactionCommitted, nil
}

func TestNHA030BlockedWriterLeavesHostAndMailboxBounded(t *testing.T) {
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	writer := &nha030Writer{blockSecond: make(chan struct{}), secondEntered: make(chan struct{}), secondFinished: make(chan struct{})}
	recording, err := Start(context.Background(), fixture.Sequencer(), writer, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		select {
		case <-writer.blockSecond:
		default:
			close(writer.blockSecond)
		}
	}()
	instance, err := domain.ParseIntegrationInstanceID("nha030-blocked-writer")
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
	if err := driver.Start(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	binding, err := driver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-writer.secondEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("writer never reached blocked flush")
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	publishBurst := func(from, count int) error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for i := from; i < from+count; i++ {
			value := domain.NewSwitchValue(i%2 == 0)
			observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
			if err != nil {
				return err
			}
			if err := session.Publish(ctx, binding, observation); err != nil {
				return err
			}
		}
		return nil
	}
	progress := make(chan error, 1)
	go func() { progress <- publishBurst(0, 10000) }()
	select {
	case err := <-progress:
		if err != nil {
			t.Fatalf("Host stopped while writer blocked: %v", err)
		}
	case <-time.After(18 * time.Second):
		close(writer.blockSecond)
		t.Fatal("Host admission waited for the blocked writer")
	}
	runtime.GC()
	var firstHeap runtime.MemStats
	runtime.ReadMemStats(&firstHeap)
	if err := publishBurst(10000, 20000); err != nil {
		t.Fatalf("Host stopped during sustained bounded-mailbox admissions: %v", err)
	}
	runtime.GC()
	var stableHeap runtime.MemStats
	runtime.ReadMemStats(&stableHeap)
	if stableHeap.Alloc > firstHeap.Alloc+32<<20 {
		t.Fatalf("retained heap kept growing after mailbox saturation: first=%d later=%d", firstHeap.Alloc, stableHeap.Alloc)
	}
	var process syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &process); err == nil {
		t.Logf("bounded writer pressure: retained heap after 10k=%d B, after 30k=%d B, high-water RSS=%d (%s OS units)", firstHeap.Alloc, stableHeap.Alloc, process.Maxrss, runtime.GOOS)
	}
	select {
	case <-writer.secondFinished:
		t.Fatal("writer completed before blocker release")
	default:
	}
	status, err := recording.Status()
	if err != nil || status.QueuedEntries > MaxQueuedEntries || status.QueuedBytes > recording.limits.QueueBytes() || status.LostEntries == 0 || !status.HistoryDegraded || status.ReconciliationComplete {
		t.Fatalf("blocked writer did not bound and report mailbox: %+v %v", status, err)
	}
	close(writer.blockSecond)
	if err := host.CloseAdmissions(); err != nil {
		t.Fatal(err)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := recording.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}

func (w *nha030Writer) RecorderCheckpoint(context.Context, storage.RecorderEpoch) (storage.RecorderCheckpoint, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reads++
	if w.readFailureOnce {
		w.readFailureOnce = false
		return storage.RecorderCheckpoint{}, errors.New("checkpoint read unavailable")
	}
	return w.checkpoint, nil
}

func (w *nha030Writer) CloseRecorderEpoch(_ context.Context, _ storage.RecorderEpoch, sequence uint64) (storage.TransactionOutcome, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if sequence != w.checkpoint.Sequence {
		return storage.TransactionNotCommitted, storage.ErrRecorderIntegrity
	}
	w.checkpoint.CleanClose = true
	return storage.TransactionCommitted, nil
}

func TestNHA030TimerFlushAndExcludedKinds(t *testing.T) {
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	writer := &nha030Writer{updated: make(chan struct{}, 8)}
	recording, err := Start(context.Background(), fixture.Sequencer(), writer, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if writer.checkpoint.Sequence != 0 || len(writer.batches) != 1 || !writer.batches[0].HasImage() {
		t.Fatal("initial image or epoch/0 was not committed before pilot start")
	}
	instance, err := domain.ParseIntegrationInstanceID("nha030-fake")
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
	started := time.Now()
	if err := driver.Start(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		writer.mu.Lock()
		count := len(writer.batches)
		var batch storage.RecorderBatch
		if count > 1 {
			batch = writer.batches[1]
		}
		writer.mu.Unlock()
		if count > 1 {
			if elapsed := time.Since(started); elapsed < 100*time.Millisecond {
				t.Fatalf("sub-threshold batch flushed before timer: %v", elapsed)
			}
			if batch.StateCount() != 1 || batch.HasImage() || batch.Through() < 3 {
				t.Fatalf("state and excluded kinds were confused: states=%d image=%v through=%d", batch.StateCount(), batch.HasImage(), batch.Through())
			}
			break
		}
		select {
		case <-writer.updated:
		case <-deadline.C:
			t.Fatal("sub-threshold state did not flush without another report")
		}
	}
	status, err := recording.Status()
	if err != nil || status.PolicyExcluded < 2 {
		t.Fatalf("non-state policy coverage absent: %+v %v", status, err)
	}
	if err := host.CloseAdmissions(); err != nil {
		t.Fatal(err)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := recording.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-recording.Done():
	default:
		t.Fatal("Done remained open after drain")
	}
}

func TestNHA030CountThresholdFlushesFiveHundredStates(t *testing.T) {
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha030-count-threshold")
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
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 18, 12, 0, 1, 0, time.UTC)
	for i := 0; i < FlushStates; i++ {
		value := domain.NewSwitchValue(i%2 == 0)
		observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Publish(context.Background(), binding, observation); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.After(3 * time.Second)
	for {
		writer.mu.Lock()
		count := len(writer.batches)
		states := 0
		if count > 1 {
			states = writer.batches[1].StateCount()
		}
		writer.mu.Unlock()
		if count > 1 {
			if states != FlushStates {
				t.Fatalf("count threshold split unexpectedly: first batch has %d states", states)
			}
			break
		}
		select {
		case <-writer.updated:
		case <-deadline:
			t.Fatal("500-state threshold did not flush")
		}
	}
	if err := host.CloseAdmissions(); err != nil {
		t.Fatal(err)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := recording.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}

func TestNHA030EncodedByteThresholdFlushesBeforeCount(t *testing.T) {
	_, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	units := domain.NewUnitCatalog()
	longUnit := strings.Repeat("u", 200000)
	if err := units.Register(longUnit, domain.DimensionPower, 1, 0); err != nil {
		t.Fatal(err)
	}
	catalog, err := contracts.NewCapabilityCatalog(units)
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(fixture, fixture, catalog)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha030-byte-threshold")
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("meter", "power")
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := integrations.NewCandidate(key, []*wireregistry.CapabilityDescription{{
		SchemaId: contracts.MeasurementSchema, Major: 1, DisplayName: "Power", Endpoint: "power",
		Constraints: &wireregistry.CapabilityDescription_Measurement{Measurement: &wireregistry.MeasurementConstraints{
			Unit: longUnit, Precision: 1, Semantic: wireregistry.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT,
			Aggregation: wireregistry.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT,
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		t.Fatalf("measurement binding: %v %d", err, len(bindings))
	}
	writer := &nha030Writer{updated: make(chan struct{}, 8)}
	recording, err := Start(context.Background(), fixture.Sequencer(), writer, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	unit, err := units.Parse(longUnit)
	if err != nil {
		t.Fatal(err)
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 18, 12, 0, 1, 0, time.UTC)
	for i := 0; i < 7; i++ {
		measurement, err := domain.NewMeasurement(float64(i), unit, 1, domain.SemanticMeasurement, domain.AggregationCurrent)
		if err != nil {
			t.Fatal(err)
		}
		value, err := domain.NewMeasurementValue(measurement)
		if err != nil {
			t.Fatal(err)
		}
		observation, err := integrations.NewStateObservation("power", quality, &value, nil, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Publish(context.Background(), bindings[0], observation); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.After(3 * time.Second)
	for {
		writer.mu.Lock()
		count := len(writer.batches)
		var first storage.RecorderBatch
		if count > 1 {
			first = writer.batches[1]
		}
		writer.mu.Unlock()
		if count > 1 {
			if first.StateCount() >= FlushStates || first.StateCount() == 0 || first.EncodedStateBytes() < FlushEncodedBytes {
				t.Fatalf("encoded threshold did not produce a state batch: states=%d encoded=%d", first.StateCount(), first.EncodedStateBytes())
			}
			break
		}
		select {
		case <-writer.updated:
		case <-deadline:
			t.Fatal("encoded-byte threshold did not flush")
		}
	}
	if err := host.CloseAdmissions(); err != nil {
		t.Fatal(err)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := recording.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}

func TestNHA030OversizedRecoveryImagePersistsGapAndKeepsHostLive(t *testing.T) {
	_, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	units := domain.NewUnitCatalog()
	longUnit := strings.Repeat("u", 1000000)
	if err := units.Register(longUnit, domain.DimensionPower, 1, 0); err != nil {
		t.Fatal(err)
	}
	catalog, err := contracts.NewCapabilityCatalog(units)
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(fixture, fixture, catalog)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha030-oversize-image")
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("meter", "power")
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := integrations.NewCandidate(key, []*wireregistry.CapabilityDescription{{
		SchemaId: contracts.MeasurementSchema, Major: 1, DisplayName: "Power", Endpoint: "power",
		Constraints: &wireregistry.CapabilityDescription_Measurement{Measurement: &wireregistry.MeasurementConstraints{
			Unit: longUnit, Precision: 1, Semantic: wireregistry.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT,
			Aggregation: wireregistry.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT,
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		t.Fatalf("measurement binding: %v %d", err, len(bindings))
	}
	writer := &nha030Writer{}
	recording, err := Start(context.Background(), fixture.Sequencer(), writer, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	initial, err := recording.Status()
	if err != nil {
		t.Fatal(err)
	}
	unit, err := units.Parse(longUnit)
	if err != nil {
		t.Fatal(err)
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 18, 12, 0, 1, 0, time.UTC)
	publish := func(amount float64) error {
		measurement, err := domain.NewMeasurement(amount, unit, 1, domain.SemanticMeasurement, domain.AggregationCurrent)
		if err != nil {
			return err
		}
		value, err := domain.NewMeasurementValue(measurement)
		if err != nil {
			return err
		}
		observation, err := integrations.NewStateObservation("power", quality, &value, nil, at, nil)
		if err != nil {
			return err
		}
		return session.Publish(context.Background(), bindings[0], observation)
	}
	if err := publish(1); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, err := recording.Status()
		if err != nil {
			t.Fatal(err)
		}
		if status.ReconciliationTooLarge && status.Checkpoint > initial.Checkpoint {
			if !status.HistoryDegraded || status.ReconciliationComplete || status.Phase == PhaseFailed {
				t.Fatalf("oversized image hid incomplete coverage or stopped writer: %+v", status)
			}
			writer.mu.Lock()
			last := writer.batches[len(writer.batches)-1]
			writer.mu.Unlock()
			if !last.HasGaps() || last.HasImage() {
				t.Fatalf("oversized image produced no explicit gap: gap=%v image=%v", last.HasGaps(), last.HasImage())
			}
			if err := publish(2); err != nil {
				t.Fatalf("Host stopped after oversized image: %v", err)
			}
			if err := host.CloseAdmissions(); err != nil {
				t.Fatal(err)
			}
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := recording.Stop(stopCtx); err != nil {
				t.Fatal(err)
			}
			return
		}
		select {
		case <-recording.Done():
			t.Fatal("Recorder exited instead of retaining degraded coverage")
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("oversized recovery image never persisted an uncertain gap")
}

func TestNHA030IndeterminateCommitRequiresReadBeforeRetry(t *testing.T) {
	epoch, err := storage.ParseRecorderEpoch("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	batch, err := storage.NewRecorderBatch(epoch, 0, 0, nil, nil, 0, &storage.RecorderImage{Boundary: 0})
	if err != nil {
		t.Fatal(err)
	}
	writer := &nha030Writer{indeterminateOnce: true, readFailureOnce: true}
	r := &Recorder{writer: writer, epoch: epoch, readCtx: context.Background(), status: Status{Phase: PhaseRunning}}
	if err := r.write(batch); err != nil {
		t.Fatal(err)
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.writes != 1 || writer.reads != 2 || !writer.checkpoint.Confirms(batch) {
		t.Fatalf("indeterminate commit replayed before checkpoint read: writes=%d reads=%d", writer.writes, writer.reads)
	}
}
