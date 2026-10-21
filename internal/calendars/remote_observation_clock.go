package calendars

import (
	"context"
	"github.com/damione1/Homeward/internal/storage"
	"time"
)

type remoteObservationClockKey struct{}
type remoteObservationClock struct {
	observation, source [32]byte
	at, until           time.Time
	mono                time.Duration
}

// WithRemoteObservationClock carries the original process-local observation
// clock through the existing writer call. No elapsed clock is persisted.
func WithRemoteObservationClock(ctx context.Context, observation storage.RemoteCalendarObservation, at, until time.Time, mono time.Duration) (context.Context, error) {
	if ctx == nil || ctx.Err() != nil || !observation.Valid() || at.IsZero() || until.Before(at) || until.Sub(at) > 24*time.Hour || mono < 0 {
		return nil, failure(InvalidInput)
	}
	id, _ := observation.ID()
	source, _ := observation.Source().Key()
	return context.WithValue(ctx, remoteObservationClockKey{}, remoteObservationClock{observation: id, source: source, at: at.UTC(), until: until.UTC(), mono: mono}), nil
}
func remoteWriterObservationClock(ctx context.Context, observation storage.RemoteCalendarObservation, clock RemoteClock) (remoteObservationClock, error) {
	if err := contextValid(ctx); err != nil {
		return remoteObservationClock{}, err
	}
	if !observation.Valid() {
		return remoteObservationClock{}, failure(InvalidInput)
	}
	id, _ := observation.ID()
	source, _ := observation.Source().Key()
	if start, ok := ctx.Value(remoteObservationClockKey{}).(remoteObservationClock); ok {
		if start.observation != id || start.source != source {
			return remoteObservationClock{}, failure(InvalidInput)
		}
		return start, nil
	}
	// Direct writers capture before any parse/SQL/queue delay. Source supplies
	// its earlier HTTP observation proof above, including through wrappers.
	at, mono := clock.Now().UTC(), clock.MonoNow()
	if at.IsZero() || mono < 0 {
		return remoteObservationClock{}, failure(InvalidInput)
	}
	return remoteObservationClock{observation: id, source: source, at: at, mono: mono}, nil
}
