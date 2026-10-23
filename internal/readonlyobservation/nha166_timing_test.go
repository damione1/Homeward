package readonlyobservation

import (
	"context"
	"testing"
	"time"
)

type nha166DelayedRead struct {
	calls  int
	cancel context.CancelFunc
}

func (s *nha166DelayedRead) Read(ctx context.Context, _ uint64) (Sample, error) {
	s.calls++
	if s.calls == 2 {
		// Real bounded blocking on a double. This is never a substitute for the
		// required server24h; it reproduces a completion-check ordering bug.
		timer := time.NewTimer(30 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return Sample{}, ctx.Err()
		}
		s.cancel()
	}
	return Sample{Epoch: "timing-double", Binding: Binding{Profile: "synthetic"}, ConfigRevision: 1}, nil
}
func TestNHA166LongReadCannotCompleteWindowBeforeGapDetection(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := &nha166DelayedRead{cancel: cancel}
	err = run(ctx, store, source, 20*time.Millisecond, time.Millisecond, 5*time.Millisecond)
	if err != context.Canceled || store.State.Complete || store.State.WindowElapsed >= 20*time.Millisecond || store.State.Interruptions < 2 || source.calls != 2 {
		t.Fatal("slow final read was credited before detecting its gap", err, store.State)
	}
}
