package server

import (
	"context"
	"time"

	"github.com/damione1/Homeward/internal/executionhistory"
)

// The history owner outlives all producers, including a scene driver that
// returns after the common shutdown budget. Its real Done keeps Store alive.
// There is one shutdown join, never a goroutine per observation or retry.
func drainExecutionHistoryAfter(ctx context.Context, history *executionhistory.Sink, producers ...<-chan struct{}) error {
	result := make(chan error, 1)
	go func() {
		for _, done := range producers {
			if done != nil {
				<-done
			}
		}
		// Even when the caller's common budget elapsed while joining a
		// producer, capture and attempt to persist its final observations.
		drainCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		result <- history.Close(drainCtx)
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
