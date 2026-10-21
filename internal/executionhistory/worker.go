package executionhistory

import (
	"context"
	"errors"
	"time"

	"github.com/damione1/Homeward/internal/storage"
)

func (s *Sink) hasActiveLoss() bool {
	for i := 1; i < len(s.activeLoss); i++ {
		if s.activeLoss[i].Load() != 0 {
			return true
		}
	}
	return false
}

// freeze runs under s.mu. All records were copied and admitted at enqueue;
// the pending batch never changes while a COMMIT outcome is unresolved.
func (s *Sink) freeze(now time.Time) bool {
	if s.hasPending || (len(s.queue) == 0 && !s.hasActiveLoss()) {
		return false
	}
	count, bytes := 0, 0
	for count < len(s.queue) && count < maxBatchRecords {
		charge := s.queue[count].RetainedBytes()
		if count != 0 && bytes+charge > maxBatchBytes {
			break
		}
		bytes += charge
		count++
	}
	records := append([]storage.HistoryRecord(nil), s.queue[:count]...)
	if s.lossStart.IsZero() {
		s.lossStart = s.lastBoundary
	}
	var losses []storage.HistoryLoss
	for reason := storage.HistoryLossQueueFull; reason <= storage.HistoryLossInvalidMetadata; reason++ {
		lost := s.activeLoss[reason].Swap(0)
		if lost != 0 {
			losses = append(losses, storage.HistoryLoss{Reason: reason, KnownCount: lost,
				From: s.lossStart, UncertainEnvelope: true})
		}
	}
	// Producers can reject on TryLock while this transfer holds s.mu. Date
	// the envelope after every Swap so it covers all rejections included.
	through := time.Now().UTC()
	for i := range losses {
		losses[i].Through = through
	}
	batch, err := storage.NewHistoryBatch(s.epoch, s.nextBatch+1, records, losses)
	if err != nil {
		// Admission has already made every component valid. Any failure here
		// is an internal integrity fault; do not silently discard the queue.
		s.integrity, s.degraded = true, true
		for _, loss := range losses {
			s.activeLoss[loss.Reason].Add(loss.KnownCount)
		}
		return false
	}
	s.nextBatch++
	s.pending, s.hasPending = batch, true
	copy(s.queue, s.queue[count:])
	clear(s.queue[len(s.queue)-count:])
	s.queue = s.queue[:len(s.queue)-count]
	s.queueBytes -= bytes
	if len(s.queue) != 0 {
		s.firstQueued = now
	} else {
		s.firstQueued = time.Time{}
	}
	return true
}

func (s *Sink) run(ctx context.Context) {
	defer close(s.done)
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			s.mu.Lock()
			s.undrained = s.hasPending || len(s.queue) != 0 || s.hasActiveLoss()
			s.degraded = s.degraded || s.undrained
			s.mu.Unlock()
			return
		}
		s.mu.Lock()
		if s.integrity {
			s.undrained = s.hasPending || len(s.queue) != 0 || s.hasActiveLoss()
			s.mu.Unlock()
			return
		}
		if !s.hasPending {
			ready := s.closing || len(s.queue) >= maxBatchRecords || s.queueBytes >= maxBatchBytes ||
				(len(s.queue) != 0 && time.Since(s.firstQueued) >= flushInterval) ||
				(s.hasActiveLoss() && time.Since(s.lastBoundary) >= flushInterval)
			if ready {
				s.freeze(time.Now().UTC())
			}
		}
		if !s.hasPending && s.closing && len(s.queue) == 0 && !s.hasActiveLoss() {
			s.mu.Unlock()
			return
		}
		if !s.hasPending {
			s.mu.Unlock()
			select {
			case <-s.notify:
			case <-time.After(flushInterval):
			case <-ctx.Done():
			}
			continue
		}
		batch := s.pending
		s.mu.Unlock()
		result, err := s.store.CommitExecutionHistoryBatch(ctx, batch)
		if errors.Is(err, storage.ErrHistoryCommitUnknown) {
			resolution, resolveErr := s.store.ResolveExecutionHistoryBatch(ctx, batch)
			if resolveErr == nil {
				switch resolution {
				case storage.HistoryResolutionCommitted:
					result, err = storage.HistoryAlreadyCommitted, nil
				case storage.HistoryResolutionAbsent:
					result, err = s.store.CommitExecutionHistoryBatch(ctx, batch)
				}
			} else {
				err = resolveErr
			}
		}
		if err == nil && (result == storage.HistoryCommitted || result == storage.HistoryAlreadyCommitted) {
			s.mu.Lock()
			s.pending = storage.HistoryBatch{}
			s.hasPending = false
			s.lastBoundary = time.Now().UTC()
			// A committed loss proof remains a degraded coverage statement.
			if batch.LastOrdinal() > s.committed {
				s.committed = batch.LastOrdinal()
			}
			s.mu.Unlock()
			backoff = time.Second
			continue
		}
		s.mu.Lock()
		s.degraded = true
		if errors.Is(err, storage.ErrHistoryIntegrity) {
			s.integrity = true
		}
		s.mu.Unlock()
		if errors.Is(err, storage.ErrHistoryIntegrity) {
			continue
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
		}
		if backoff < 8*time.Second {
			backoff *= 2
			if backoff > 8*time.Second {
				backoff = 8 * time.Second
			}
		}
	}
}
