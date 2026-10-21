package automationtriggers

import (
	"context"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/domain"
	"time"
)

// RemoteCalendarDeadline retains Source's original elapsed deadline. All
// mutations of its terminal bit occur under the engine's existing mutex.
type RemoteCalendarDeadline struct {
	wall, until     time.Time
	mono, untilMono time.Duration
	terminal        bool
}

func NewRemoteCalendarDeadline(wall, until time.Time, mono, untilMono time.Duration) (RemoteCalendarDeadline, error) {
	if wall.IsZero() || !until.After(wall) || until.Sub(wall) > 24*time.Hour || mono < 0 || untilMono <= mono || untilMono-mono > 24*time.Hour {
		return RemoteCalendarDeadline{}, ErrInvalidPlan
	}
	return RemoteCalendarDeadline{wall: wall.UTC(), until: until.UTC(), mono: mono, untilMono: untilMono}, nil
}
func (d *RemoteCalendarDeadline) live(now time.Time, mono time.Duration) bool {
	if d == nil || d.terminal || d.wall.IsZero() || now.Before(d.wall) || !now.Before(d.until) || mono < d.mono || mono >= d.untilMono {
		if d != nil {
			d.terminal = true
		}
		return false
	}
	return true
}

type remoteCalendarWindow struct {
	deadline RemoteCalendarDeadline
	stale    bool
}

// ReplaceRemoteCalendarWindow consumes only the admitted scalar projection.
// Aggregate admission precedes every occurrence copy; no timezone is loaded.
func (s *Service) ReplaceRemoteCalendarWindow(ctx context.Context, source CalendarSourceID, input calendars.AutomationWindow, deadline RemoteCalendarDeadline) error {
	if s == nil || nilInterface(ctx) || !source.Valid() || !input.Valid() {
		return ErrInvalidPlan
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.closed || !s.admissionsOpen {
		return ErrServiceClosed
	}
	count := input.Len()
	for id, existing := range s.windows {
		if id != source {
			count += len(existing.occurrences)
		}
	}
	if count > MaxCalendarOccurrences || s.estimatedChargeLocked(count, s.seenBytes) > MaxEngineCharge-calendarResultReserve {
		s.rejectCalendarLocked(source, DegradedCapacity)
		return ErrCapacity
	}
	freshness, known := input.RemoteState().Freshness()
	stale := !known || freshness != calendars.RemoteFresh
	if !stale && !deadline.live(s.clock.Now().UTC(), s.clock.MonoNow()) {
		s.rejectCalendarLocked(source, DegradedSource)
		return ErrInvalidPlan
	}
	from, to, ok := input.Bounds()
	if !ok {
		return ErrInvalidPlan
	}
	revision, ok := input.Revision().Number()
	if !ok {
		return ErrInvalidPlan
	}
	window := CalendarWindow{source: source, revision: revision, fromUTC: from, toUTC: to, valid: true, remote: &remoteCalendarWindow{deadline: deadline, stale: stale}}
	window.occurrences = make([]CalendarOccurrence, input.Len())
	for i := range window.occurrences {
		projection, ok := input.At(i)
		if !ok {
			return ErrInvalidPlan
		}
		start, end, kind, ok := projection.Instants()
		if !ok {
			return ErrInvalidPlan
		}
		selected := end.After(from) && start.Before(to)
		if kind == calendars.AutomationPoint {
			selected = !start.Before(from) && start.Before(to)
		}
		if !selected {
			return ErrInvalidPlan
		}
		window.occurrences[i] = CalendarOccurrence{source: source, projection: projection, valid: true}
	}
	return s.replaceCalendarWindowLocked(ctx, window)
}
func (o CalendarOccurrence) projectedComponents() (CalendarSourceID, string, string, time.Time, time.Time, string, bool, domain.Freshness, error) {
	if !o.valid || !o.source.Valid() || !o.projection.Valid() {
		return CalendarSourceID{}, "", "", time.Time{}, time.Time{}, "", false, 0, ErrInvalidPlan
	}
	uid, ok := o.projection.ID().Event().UID().Value()
	if !ok {
		return CalendarSourceID{}, "", "", time.Time{}, time.Time{}, "", false, 0, ErrInvalidPlan
	}
	start, end, kind, _ := o.projection.Instants()
	_, zone, _, _ := o.projection.Zone()
	quality, _ := o.projection.Freshness()
	var freshness domain.Freshness
	switch quality {
	case calendars.RemoteFresh:
		freshness = domain.FreshnessFresh
	case calendars.RemoteStale:
		freshness = domain.FreshnessStale
	default:
		return CalendarSourceID{}, "", "", time.Time{}, time.Time{}, "", false, 0, ErrInvalidPlan
	}
	return o.source, uid, o.projection.ID().Point().String(), start, end, zone, kind == calendars.AutomationAllDay, freshness, nil
}

// DegradeCalendarSource closes the local gate before any SQL cleanup. It is
// synchronous, bounded and performs no I/O, including during Source.Stop.
func (s *Service) DegradeCalendarSource(ctx context.Context, source CalendarSourceID) error {
	if s == nil || nilInterface(ctx) || !source.Valid() {
		return ErrInvalidPlan
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if window, ok := s.windows[source]; ok && window.remote != nil {
		window.remote.stale = true
		window.remote.deadline.terminal = true
	}
	s.rejectCalendarLocked(source, DegradedSource)
	return nil
}
