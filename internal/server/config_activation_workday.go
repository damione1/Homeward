package server

import (
	"context"
	"sync"

	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/workday"
)

// The consumer retains this same reader across a source refresh. Calendar
// identity admission still belongs to the delivered calendars.Repository.
type activationWorkdayCalendar struct {
	mu     sync.RWMutex
	reader workday.CalendarReader
}

func (r *activationWorkdayCalendar) current() workday.CalendarReader {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.reader
}
func (r *activationWorkdayCalendar) CalendarID() (calendars.CalendarID, error) {
	reader := r.current()
	if nilReactiveInterface(reader) {
		return calendars.CalendarID{}, configactivation.ErrActivationUnavailable
	}
	return reader.CalendarID()
}
func (r *activationWorkdayCalendar) Dates(ctx context.Context, from, to calendars.CivilDate) ([]calendars.Span, error) {
	reader := r.current()
	if nilReactiveInterface(reader) {
		return nil, configactivation.ErrActivationUnavailable
	}
	return reader.Dates(ctx, from, to)
}
func (r *activationWorkdayCalendar) publish(reader workday.CalendarReader) {
	r.mu.Lock()
	r.reader = reader
	r.mu.Unlock()
}
