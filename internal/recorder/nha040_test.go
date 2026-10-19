package recorder

import (
	"context"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
)

func TestNHA040NativeOnlyStreamAdvancesExcludedCheckpoint(t *testing.T) {
	_, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	sequence := fixture.Sequencer()
	writer := &nha030Writer{updated: make(chan struct{}, 8)}
	recording, err := Start(context.Background(), sequence, writer, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = recording.Stop(stopCtx)
	})
	instance, err := domain.ParseIntegrationInstanceID("nha040-mqtt")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	producerDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(500 * time.Microsecond)
		defer ticker.Stop()
		var sourceSequence uint64
		for {
			select {
			case <-stop:
				producerDone <- nil
				return
			case <-ticker.C:
			}
			sourceSequence++
			edge, err := integrations.NewNativeMQTTEdge("buttons/press", []byte("pressed"), false, sourceSequence)
			if err == nil {
				var stamp domain.SourceStamp
				stamp, err = domain.NewSourceStamp("nha040-session", &sourceSequence)
				if err == nil {
					err = sequence.AcceptNativeMQTTEdge(context.Background(), instance, stamp, edge)
				}
			}
			if err != nil {
				producerDone <- err
				return
			}
		}
	}()
	defer func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
	}()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		writer.mu.Lock()
		count := len(writer.batches)
		if count > 1 {
			batch := writer.batches[1]
			writer.mu.Unlock()
			if batch.StateCount() != 0 || batch.HasGaps() || batch.HasImage() || batch.Through() <= batch.From() {
				t.Fatalf("native-only checkpoint was not policy-excluded: states=%d gaps=%v image=%v from=%d through=%d", batch.StateCount(), batch.HasGaps(), batch.HasImage(), batch.From(), batch.Through())
			}
			break
		}
		writer.mu.Unlock()
		select {
		case err := <-producerDone:
			t.Fatalf("native producer stopped before checkpoint: %v", err)
		case <-writer.updated:
		case <-deadline.C:
			t.Fatal("continuous native-only stream never advanced SQL checkpoint")
		}
	}
	close(stop)
	if err := <-producerDone; err != nil {
		t.Fatal(err)
	}
	status, err := recording.Status()
	if err != nil || status.PolicyExcluded == 0 || status.HistoryDegraded {
		t.Fatalf("native exclusion status: %+v %v", status, err)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := recording.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-recording.Done():
	default:
		t.Fatal("Recorder Done remained open after native-only drain")
	}
}
