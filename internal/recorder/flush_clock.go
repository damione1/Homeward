package recorder

import (
	"context"
	"time"
)

// flushClock owns both the elapsed-time decision and the wake of Next. SQL and
// recovery attempts deliberately retain their separate, physical deadlines.
type flushClock interface {
	now() time.Time
	withDeadline(context.Context, time.Time) (context.Context, context.CancelFunc)
}

type realFlushClock struct{}

func (realFlushClock) now() time.Time { return time.Now() }
func (realFlushClock) withDeadline(parent context.Context, at time.Time) (context.Context, context.CancelFunc) {
	return context.WithDeadline(parent, at)
}

// The zero deadline names the absence of a pending batch's first entry. An
// admitted deadline may be due already; that is different from no deadline.
type flushDeadline struct{ at time.Time }

func newFlushDeadline(now time.Time) (flushDeadline, error) {
	if now.IsZero() {
		return flushDeadline{}, ErrInvalidRecorder
	}
	at := now.Add(flushInterval)
	if at.IsZero() || !at.After(now) {
		return flushDeadline{}, ErrInvalidRecorder
	}
	return flushDeadline{at: at}, nil
}

func (d flushDeadline) present() bool          { return !d.at.IsZero() }
func (d flushDeadline) due(now time.Time) bool { return d.present() && !now.Before(d.at) }

func (r *Recorder) nextReadContext(deadline flushDeadline, ready bool, imageAt time.Time) (context.Context, context.CancelFunc, error) {
	now := r.flushClock.now()
	if now.IsZero() {
		return nil, nil, ErrInvalidRecorder
	}
	if !deadline.present() || (deadline.due(now) && !ready) {
		var err error
		deadline, err = newFlushDeadline(now)
		if err != nil {
			return nil, nil, err
		}
	}
	parent := r.readCtx
	cancelImage := func() {}
	if !imageAt.IsZero() {
		parent, cancelImage = context.WithDeadline(parent, imageAt)
	}
	next, cancelFlush := r.flushClock.withDeadline(parent, deadline.at)
	if nilInterface(next) || cancelFlush == nil {
		if cancelFlush != nil {
			cancelFlush()
		}
		cancelImage()
		return nil, nil, ErrInvalidRecorder
	}
	return next, func() {
		cancelFlush()
		cancelImage()
	}, nil
}
