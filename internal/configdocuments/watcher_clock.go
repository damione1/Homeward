package configdocuments

import (
	"context"
	"reflect"
	"time"
)

// WatcherClock injects only waiting/elapsed time. Observations, SQL admission,
// parsing and publication always use the actual shared owners. Production
// supplies RealWatcherClock; there is no user-controlled clock or activation.
type WatcherClock interface {
	Now() time.Time
	Wait(context.Context, time.Duration) error
}
type RealWatcherClock struct{}

func (RealWatcherClock) Now() time.Time { return time.Now() }
func (RealWatcherClock) Wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func validWatcherClock(clock WatcherClock) bool {
	if clock == nil {
		return false
	}
	v := reflect.ValueOf(clock)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func:
		return !v.IsNil()
	default:
		return true
	}
}
func NewWatcherWithClock(service *Service, clock WatcherClock) (*Watcher, error) {
	if !validWatcherClock(clock) || clock.Now().IsZero() {
		return nil, &Error{kind: FailureInvalid}
	}
	watcher, err := NewWatcher(service)
	if err != nil {
		return nil, err
	}
	watcher.clock = clock
	return watcher, nil
}
