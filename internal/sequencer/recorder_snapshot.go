package sequencer

import (
	"errors"
	"math"
	"sort"
	"unsafe"

	"github.com/damione1/Homeward/internal/domain"
)

var ErrRecorderSnapshotTooLarge = errors.New("recorder state snapshot exceeds its admitted budget")

// RecorderState is a copied projection, not an admission transition. The
// projection does not retain the last Entry's integration instance, causality,
// or admission sequence; none of those fields may be inferred from a binding.
type RecorderState struct {
	Entity domain.EntityID
	State  domain.ObservedState
}

// RecorderAbandonment names the entire interval left behind by replacement.
// PendingLoss preserves the mailbox's known loss count, which is not inferred
// from the width of the interval. The interval can include non-state entries.
type RecorderAbandonment struct {
	After       Cursor
	Through     Cursor
	PendingLoss *Loss
}

// RecorderCapture couples one state-only image to the first live entry after
// Cursor. The states are snapshots at the boundary, never transitions at B.
type RecorderCapture struct {
	Cursor         Cursor
	States         []RecorderState
	EstimatedBytes int
	Subscription   *Subscription
	Abandoned      *RecorderAbandonment
}

// RecorderCursor fixes a terminal admission boundary after producers drain.
func (s *Sequencer) RecorderCursor() (Cursor, error) {
	if s == nil || s.journal == nil {
		return Cursor{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return Cursor{epoch: s.epoch, seq: s.seq}, nil
}

// CaptureRecorder replaces a Recorder subscription under the admission mutex.
// A refused image leaves the old subscription and its queue untouched. When
// previous is nil this is an initial capture; after is then ignored.
func (s *Sequencer) CaptureRecorder(previous *Subscription, after Cursor, maxSnapshotBytes, maxQueueBytes int) (RecorderCapture, error) {
	if s == nil || s.journal == nil || maxSnapshotBytes < int(unsafe.Sizeof(RecorderCapture{}))+256 || maxSnapshotBytes > 4<<20 || maxQueueBytes < 1 || maxQueueBytes > MaxRecorderQueueBytes {
		return RecorderCapture{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous != nil {
		if previous.owner != s || previous.class != SubscriptionRecorder || previous.capacity > MaxRecorderQueueEntries || previous.closed || after.epoch != s.epoch || after.seq > s.seq {
			return RecorderCapture{}, ErrInvalidInput
		}
	} else if len(s.subs) >= MaxSubscriptions {
		return RecorderCapture{}, ErrSubscriptionFull
	}

	// Count the same retained payload charge used by the journal. Charge the
	// image's own slice and metadata separately; reject before copying states.
	bytes := int(unsafe.Sizeof(RecorderCapture{})) + 256
	perState := int(unsafe.Sizeof(RecorderState{})) + 64
	if len(s.states) > (math.MaxInt-bytes)/perState {
		return RecorderCapture{}, ErrRecorderSnapshotTooLarge
	}
	bytes += len(s.states) * perState
	if bytes > maxSnapshotBytes {
		return RecorderCapture{}, ErrRecorderSnapshotTooLarge
	}
	for id, state := range s.states {
		charge := retainedBytes(Entry{kind: KindState, entity: id, state: state})
		if charge < 0 || charge > maxSnapshotBytes-bytes {
			return RecorderCapture{}, ErrRecorderSnapshotTooLarge
		}
		bytes += charge
	}
	states := make([]RecorderState, 0, len(s.states))
	for id, state := range s.states {
		states = append(states, RecorderState{Entity: id, State: state})
	}
	sort.Slice(states, func(i, j int) bool {
		left, _ := states[i].Entity.Value()
		right, _ := states[j].Entity.Value()
		return left < right
	})
	cursor := Cursor{epoch: s.epoch, seq: s.seq}
	capacity := s.limits.recorder
	if capacity > MaxRecorderQueueEntries {
		capacity = MaxRecorderQueueEntries
	}
	var abandonment *RecorderAbandonment
	if previous != nil {
		if after.seq < cursor.seq {
			abandonment = &RecorderAbandonment{After: after, Through: cursor}
			if previous.loss != nil {
				loss := *previous.loss
				abandonment.PendingLoss = &loss
			}
		}
	}
	var queue recorderQueue
	if previous != nil {
		// Detach the sole body only after the image and abandonment succeed.
		// The old owner is zero before the successor takes ownership.
		queue = recorderQueue{body: previous.recorder.release()}
		previous.queueBytes = 0
	} else {
		var err error
		queue, err = newRecorderQueue(capacity)
		if err != nil {
			return RecorderCapture{}, err
		}
	}
	sub := &Subscription{owner: s, class: SubscriptionRecorder, capacity: capacity, recorder: queue, maxBytes: maxQueueBytes, wake: make(chan struct{}), replayValid: true}
	if previous != nil {
		previous.closed = true
		delete(s.subs, previous)
		previous.signal()
	}
	s.subs[sub] = struct{}{}
	return RecorderCapture{Cursor: cursor, States: states, EstimatedBytes: bytes, Subscription: sub, Abandoned: abandonment}, nil
}
