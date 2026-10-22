package server

import (
	"context"
	"sync"
)

// The first Err call records the original live state, then pauses the reader
// while a real activation cancels it and publishes under its mutation barrier.
// Returning that saved result makes only the post-barrier check detect expiry
// or cancellation; all later Err calls delegate to the actual lease context.
type nha145PausedFirstErrContext struct {
	context.Context
	once     sync.Once
	observed chan error
	resume   func()
	released chan struct{}
}

func newNHA145PausedFirstErrContext(ctx context.Context) *nha145PausedFirstErrContext {
	released := make(chan struct{})
	return &nha145PausedFirstErrContext{
		Context:  ctx,
		observed: make(chan error, 1),
		resume:   sync.OnceFunc(func() { close(released) }),
		released: released,
	}
}

func (c *nha145PausedFirstErrContext) Err() error {
	first := false
	var initial error
	c.once.Do(func() {
		first = true
		initial = c.Context.Err()
		c.observed <- initial
		<-c.released
	})
	if first {
		return initial
	}
	return c.Context.Err()
}
