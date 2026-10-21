package sequencer

import "time"

// MaintenanceBoundary is a constant-size admission observation. It is not an
// image or a new journal entry, and no authority or SQL is called under mu.
type MaintenanceBoundary struct {
	cursor Cursor
	at     time.Time
	tick   time.Duration
}

func (b MaintenanceBoundary) Components() (Cursor, time.Time, time.Duration) {
	return b.cursor, b.at, b.tick
}
func (s *Sequencer) CaptureMaintenanceBoundary() (MaintenanceBoundary, error) {
	if s == nil || s.journal == nil {
		return MaintenanceBoundary{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	at := s.clock.Now().UTC()
	tick := s.MonotonicNow()
	if at.IsZero() || at.Year() < 1 || at.Year() > 9999 || tick < 0 {
		return MaintenanceBoundary{}, ErrInvalidInput
	}
	if tick < s.lastTick {
		tick = s.lastTick
	}
	return MaintenanceBoundary{Cursor{epoch: s.epoch, seq: s.seq}, at, tick}, nil
}
