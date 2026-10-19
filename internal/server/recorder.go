package server

import (
	"context"
	"errors"
	"time"

	"github.com/damione1/Homeward/internal/recorder"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
)

// prepareRecorder runs after the mandatory Store migration and before Host
// admissions. Run and RunWithFactory will share this composition point.
func prepareRecorder(ctx context.Context, sequence *sequencer.Sequencer, store *storage.Store, limits recorder.Limits) (*recorder.Recorder, error) {
	return recorder.Start(ctx, sequence, store, limits)
}

func drainRecorder(ctx context.Context, recording *recorder.Recorder) error {
	if recording == nil {
		return nil
	}
	return recording.Stop(ctx)
}

// Before Host exists, Recorder is the only asynchronous Store owner. Its Done
// remains in the Store lifetime even when the shared shutdown budget expires.
func closeStoreWithRecorder(store *storage.Store, recording *recorder.Recorder, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	stopErr := drainRecorder(ctx, recording)
	cancel()
	retainStoreUntilDrained(store, recording.Done())
	return errors.Join(cause, stopErr)
}
