package sequencer

import (
	"context"
	"errors"
	"sync"
)

var (
	ErrResourceExhausted  = errors.New("command queue resource exhausted")
	ErrCommandQueueClosed = errors.New("command queue is closed")
)

// CommandQueue is an admission primitive for a future typed command intent.
// It does not validate, route, execute, or replay a command. The caller must
// admit a valid intent before TryOffer; full queues reject without waiting.
type CommandQueue[T any] struct {
	mu       sync.Mutex
	items    []T
	capacity int
	rejected uint64
	closed   bool
	wake     chan struct{}
}

type CommandQueueStatus struct {
	Capacity int
	Queued   int
	Rejected uint64
	Closed   bool
}

func NewCommandQueue[T any](limits QueueLimits) (*CommandQueue[T], error) {
	if !limits.valid() {
		return nil, ErrInvalidInput
	}
	return &CommandQueue[T]{items: make([]T, 0, limits.commands), capacity: limits.commands, wake: make(chan struct{})}, nil
}

// TryOffer is nonblocking. A rejected item is never enqueued and cannot be
// mistaken for accepted work by a dispatcher.
func (q *CommandQueue[T]) TryOffer(item T) error {
	if q == nil {
		return ErrInvalidInput
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.wake == nil || q.capacity == 0 {
		return ErrInvalidInput
	}
	if q.closed {
		return ErrCommandQueueClosed
	}
	if len(q.items) == q.capacity {
		q.rejected++
		return ErrResourceExhausted
	}
	q.items = append(q.items, item)
	close(q.wake)
	q.wake = make(chan struct{})
	return nil
}

// Take drains accepted items even after Close, then reports closure. Waiting
// never holds the queue mutex or the sequencer mutex.
func (q *CommandQueue[T]) Take(ctx context.Context) (T, error) {
	var zero T
	if q == nil || ctx == nil {
		return zero, ErrInvalidInput
	}
	for {
		q.mu.Lock()
		if q.wake == nil || q.capacity == 0 {
			q.mu.Unlock()
			return zero, ErrInvalidInput
		}
		if len(q.items) > 0 {
			item := q.items[0]
			copy(q.items, q.items[1:])
			q.items[len(q.items)-1] = zero
			q.items = q.items[:len(q.items)-1]
			q.mu.Unlock()
			return item, nil
		}
		if q.closed {
			q.mu.Unlock()
			return zero, ErrCommandQueueClosed
		}
		wake := q.wake
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-wake:
		}
	}
}

func (q *CommandQueue[T]) Status() (CommandQueueStatus, error) {
	if q == nil {
		return CommandQueueStatus{}, ErrInvalidInput
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.wake == nil || q.capacity == 0 {
		return CommandQueueStatus{}, ErrInvalidInput
	}
	return CommandQueueStatus{Capacity: q.capacity, Queued: len(q.items), Rejected: q.rejected, Closed: q.closed}, nil
}

func (q *CommandQueue[T]) Close() error {
	if q == nil {
		return ErrInvalidInput
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.wake == nil || q.capacity == 0 {
		return ErrInvalidInput
	}
	if q.closed {
		return ErrCommandQueueClosed
	}
	q.closed = true
	close(q.wake)
	q.wake = make(chan struct{})
	return nil
}
