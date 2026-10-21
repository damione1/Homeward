package recorder

import (
	"context"
	"sync"
	"testing"
	"time"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/storage"
)

type nha056GatePGWriter struct {
	*nha031PGWriter
	gateMu           sync.Mutex
	calls            int
	entered, release chan struct{}
}

func (w *nha056GatePGWriter) PersistRecorderBatch(ctx context.Context, batch storage.RecorderBatch) (storage.TransactionOutcome, error) {
	w.gateMu.Lock()
	w.calls++
	blocked := w.calls == 2
	w.gateMu.Unlock()
	if blocked {
		close(w.entered)
		<-w.release
	}
	return w.nha031PGWriter.PersistRecorderBatch(ctx, batch)
}
func TestNHA056RecorderCountsConfirmedGapOnceAfterLostAcknowledgment(t *testing.T) {
	store, _ := nha031RecorderPG(t)
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	writer := &nha056GatePGWriter{nha031PGWriter: &nha031PGWriter{store: store}, entered: make(chan struct{}), release: make(chan struct{})}
	recording, err := Start(context.Background(), fixture.Sequencer(), writer, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case <-writer.release:
		default:
			close(writer.release)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := recording.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	instance, err := domain.ParseIntegrationInstanceID("nha056-pg-recorder")
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := fake.NewDriver(time.Now().UTC())
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
	case <-writer.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("real PG write barrier not reached")
	}
	nha031PublishBurst(t, session, binding, 3000)
	before, err := recording.observation.GapCounters()
	if err != nil {
		t.Fatal(err)
	}
	for _, gap := range before {
		if gap.RowsTotal.GetExact() != 0 {
			t.Fatal("uncommitted risk was counted")
		}
	}
	writer.nha031PGWriter.mu.Lock()
	writer.ackLost = true
	writer.nha031PGWriter.mu.Unlock()
	close(writer.release)
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := recording.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	status, err := recording.Status()
	if err != nil {
		t.Fatal(err)
	}
	through := status.Checkpoint
	request, err := storage.NewRecorderCoverageRequest(recording.epoch, 0, recording.epoch, &through)
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := store.ReadRecorderCoverage(stopCtx, request)
	if err != nil || coverage.Kind() != storage.RecorderCoverageReadComplete {
		t.Fatal("real committed coverage unavailable", err)
	}
	var rows, known [4]uint64
	for _, gap := range coverage.Gaps() {
		var reason obs.GapReason
		switch gap.Reason() {
		case storage.RecorderGapOverflow:
			reason = obs.GapReason_GAP_REASON_OVERFLOW
		case storage.RecorderGapReconciliation:
			reason = obs.GapReason_GAP_REASON_RECONCILIATION
		case storage.RecorderGapAbandoned:
			reason = obs.GapReason_GAP_REASON_ABANDONED
		default:
			t.Fatal("unknown persisted gap")
		}
		rows[reason]++
		if count, present := gap.KnownLost(); present {
			known[reason] += count
		}
	}
	total := uint64(0)
	for range 10 {
		metrics, err := recording.ObservationMetrics()
		if err != nil {
			t.Fatal(err)
		}
		for _, gap := range metrics.GetInstalled().CommittedGaps {
			if gap.RowsTotal.GetExact() != rows[gap.Reason] || gap.KnownLostLowerBoundTotal.GetExact() != known[gap.Reason] {
				t.Fatal("retry/read changed confirmed PG row counts")
			}
		}
	}
	for _, count := range rows {
		total += count
	}
	if total == 0 {
		t.Fatal("overflow produced no actual committed gap")
	}
	writer.nha031PGWriter.mu.Lock()
	confirmed := writer.ackConfirmed
	writer.nha031PGWriter.mu.Unlock()
	if !confirmed {
		t.Fatal("lost acknowledgment was not confirmed against real PG")
	}
}
func TestNHA056RecorderLiveRiskDoesNotBecomeCommittedGap(t *testing.T) {
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	writer := &nha030Writer{blockSecond: make(chan struct{}), secondEntered: make(chan struct{}), secondFinished: make(chan struct{})}
	recording, err := Start(context.Background(), fixture.Sequencer(), writer, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case <-writer.blockSecond:
		default:
			close(writer.blockSecond)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = recording.Stop(ctx)
	})
	instance, err := domain.ParseIntegrationInstanceID("nha056-recorder-risk")
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := fake.NewDriver(time.Now().UTC())
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
		t.Fatal("real writer barrier not reached")
	}
	nha031PublishBurst(t, session, binding, 3000)
	metrics, err := recording.ObservationMetrics()
	if err != nil {
		t.Fatal(err)
	}
	installed := metrics.GetInstalled()
	if installed.ActiveCoverage.GetAtRisk() == nil || installed.LostEntriesTotal.GetExact() == 0 {
		t.Fatal("real bounded mailbox loss was hidden")
	}
	for _, gap := range installed.CommittedGaps {
		if gap.RowsTotal.GetExact() != 0 {
			t.Fatal("volatile risk fabricated a committed gap")
		}
	}
	if installed.RecorderLagPositions.GetSameEpochPositions() == 0 {
		t.Fatal("real pending Recorder positions lost their lag")
	}
	health, err := recording.ObservationHealth()
	if err != nil || health.GetObserved().State != obs.RecorderHealthState_RECORDER_HEALTH_STATE_DEGRADED {
		t.Fatal("history risk did not degrade Recorder separately", err)
	}
	// The mailbox already owns this cumulative loss population before its
	// blocked consumer can receive the loss notice. Draining/reconciliation
	// consumes that notice and replaces the mailbox; neither may add it again.
	lostBeforeDrain := installed.LostEntriesTotal.GetExact()
	close(writer.blockSecond)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopCancel()
	if err := recording.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		after, err := recording.ObservationMetrics()
		if err != nil {
			t.Fatal(err)
		}
		if after.GetInstalled().LostEntriesTotal.GetExact() != lostBeforeDrain {
			t.Fatal("consuming or replacing the mailbox counted its known losses again")
		}
	}
}
