package automationtriggers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/domain"
)

// CalendarDeliveryJournal claims an original occurrence before exposing a fired
// result. An uncertain write must return an error, never permission to replay.
// Claim is invoked outside the engine mutex; SQL cannot stall source deadlines.
type CalendarDeliveryJournal interface {
	ClaimCalendarDelivery(context.Context, [32]byte, time.Time) (bool, error)
}

func (s *Service) AttachCalendarDeliveryJournal(j CalendarDeliveryJournal) error {
	if s == nil || nilInterface(j) {
		return ErrInvalidPlan
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.calendarJournal != nil || len(s.windows) != 0 {
		return ErrInvalidPlan
	}
	s.calendarJournal = j
	return nil
}

func calendarDeliveryKey(key calendarKey) [32]byte {
	// JSON framing preserves identity boundaries; offsets/revisions/current span
	// deliberately do not change the original occurrence's delivery identity.
	data, _ := json.Marshal([]string{key.source.value, key.uid, key.instance, key.trigger, string([]byte{key.edge})})
	return sha256.Sum256(data)
}

// CalendarDemands is a detached bounded capture of current permanent triggers
// and live waits. The bridge must renew it after plan changes.
type CalendarDemand struct {
	Source                       CalendarSourceID
	Zone                         string
	MinimumOffset, MaximumOffset time.Duration
}

func (s *Service) CalendarDemands() []CalendarDemand {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []CalendarDemand
	for _, r := range s.plan.rules {
		if r.trigger.Kind() != ir.TriggerCalendar {
			continue
		}
		offset := time.Duration(r.trigger.OffsetSeconds()) * time.Second
		found := false
		for i := range result {
			if result[i].Source == r.calendar {
				result[i].MinimumOffset = min(result[i].MinimumOffset, offset)
				result[i].MaximumOffset = max(result[i].MaximumOffset, offset)
				found = true
				break
			}
		}
		if !found {
			result = append(result, CalendarDemand{r.calendar, s.plan.zone, offset, offset})
		}
	}
	return result
}

// NewProjectedCalendarWindow separates alarm coverage from event selection.
// An event that has ended may still have a future end+offset alarm; requiring
// overlap with coverage would silently lose it on the next refresh.
func NewProjectedCalendarWindow(source CalendarSourceID, revision uint64, from, to time.Time, missed *time.Time, items []CalendarOccurrence) (CalendarWindow, error) {
	w, err := NewCalendarWindow(source, revision, from, to, missed, nil)
	if err != nil || len(items) > MaxCalendarOccurrences {
		return CalendarWindow{}, ErrInvalidPlan
	}
	seen := map[string]bool{}
	for _, item := range items {
		owner, uid, point, start, end, _, _, _, err := item.Components()
		if err != nil || owner != source || !end.After(from.Add(-24*time.Hour)) || !start.Before(to.Add(24*time.Hour)) {
			return CalendarWindow{}, ErrInvalidPlan
		}
		key := uid + "\x00" + point
		if seen[key] {
			return CalendarWindow{}, ErrInvalidPlan
		}
		seen[key] = true
		w.occurrences = append(w.occurrences, item)
	}
	return w, nil
}

// ReconcileCalendarGap consumes only elapsed alarm identities. It cannot
// enqueue Fired, including when the recovered event was moved or cancelled.
// Truncation is an explicit diagnostic; it never authorizes a catch-up run.
func (s *Service) ReconcileCalendarGap(ctx context.Context, source CalendarSourceID, from, to time.Time, items []CalendarOccurrence, truncated bool) error {
	if s == nil || nilInterface(ctx) || !source.Valid() || from.IsZero() || !to.After(from) || len(items) > MaxCalendarOccurrences {
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
	type missed struct {
		key    calendarKey
		result Result
	}
	pending := make([]missed, 0)
	bytes := s.seenBytes
	for _, r := range s.plan.rules {
		if r.trigger.Kind() != ir.TriggerCalendar || r.calendar != source {
			continue
		}
		for _, item := range items {
			key, at, err := calendarAlarm(r, item)
			if err != nil {
				return err
			}
			if !at.After(from) || at.After(to) {
				continue
			}
			if _, seen := s.seen[key]; seen {
				continue
			}
			bytes += calendarKeyCharge(key)
			if len(s.seen)+len(pending) >= MaxCalendarSeen || bytes > MaxCalendarSeenBytes {
				return ErrCapacity
			}
			pending = append(pending, missed{key, Result{kind: ResultMissed, automation: r.automation, triggerKey: r.trigger.Key(), at: at, calendarDelivery: calendarDeliveryKey(key), calendarSource: source, calendarAlarmAt: at}})
		}
	}
	if s.estimatedChargeLocked(s.occurrenceCountLocked(), bytes) > MaxEngineCharge-calendarResultReserve {
		return ErrCapacity
	}
	for _, value := range pending {
		s.seen[value.key] = struct{}{}
		s.rememberCalendarMissedLocked(calendarDeliveryKey(value.key), value.key.source, value.result.at)
		s.seenBytes += calendarKeyCharge(value.key)
		s.enqueueLocked(value.result)
		if s.suspended {
			return ErrCapacity
		}
	}
	if truncated {
		s.enqueueLocked(Result{kind: ResultMissed, reason: DegradedGap, triggerKey: source.value, at: to})
		if s.suspended {
			return ErrCapacity
		}
	}
	return nil
}

// Called only after durable claim, with the engine lock reacquired. The claim
// is never undone: invalidation during I/O consumes this occurrence as missed.
func (s *Service) calendarDeliveryCurrentLocked(result Result) bool {
	if !s.started || s.closed || !s.admissionsOpen || s.suspended || s.ctx.Err() != nil || s.activePlanImage == nil || result.plan != s.activePlanImage.plan.token || s.calendarDegraded[result.calendarSource] {
		return false
	}
	window, present := s.windows[result.calendarSource]
	now := s.clock.Now().UTC()
	if !present || !window.toUTC.After(now) {
		return false
	}
	if window.remote != nil && (window.remote.stale || !window.remote.deadline.live(now, s.clock.MonoNow())) {
		return false
	}
	for _, r := range s.plan.rules {
		if r.wait != 0 || r.trigger.Kind() != ir.TriggerCalendar || r.calendar != result.calendarSource || r.automation != result.automation || r.trigger.Key() != result.triggerKey {
			continue
		}
		for _, item := range window.occurrences {
			_, _, _, _, _, _, _, freshness, err := item.Components()
			if err != nil || freshness != domain.FreshnessFresh {
				continue
			}
			key, at, err := calendarAlarm(r, item)
			if err == nil && at.Equal(result.calendarAlarmAt) && !at.After(now) && calendarDeliveryKey(key) == result.calendarDelivery {
				return true
			}
		}
	}
	return false
}

type calendarConsumption struct {
	source CalendarSourceID
	at     time.Time
}

func (s *Service) rememberCalendarMissedLocked(key [32]byte, source CalendarSourceID, at time.Time) {
	if s.calendarMissed == nil {
		s.calendarMissed = map[[32]byte]calendarConsumption{}
	}
	s.calendarMissed[key] = calendarConsumption{source, at}
}

// CalendarCheckpoint captures consumed missed identities and a safe watermark.
// Pending Fired results hold coverage before their alarm until their durable
// claim completes; SQL runs later, outside this mutex.
func (s *Service) CalendarCheckpoint(source CalendarSourceID, through time.Time) (time.Time, [][32]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calendarCheckpointLocked(source, through)
}
func (s *Service) calendarCheckpointLocked(source CalendarSourceID, through time.Time) (time.Time, [][32]byte) {
	fence := func(value calendarConsumption) {
		if value.source == source && !value.at.After(through) {
			through = value.at.Add(-time.Second)
		}
	}
	for i, at := range s.scheduled {
		r := s.plan.rules[i]
		if r.trigger.Kind() == ir.TriggerCalendar && r.calendar == source {
			fence(calendarConsumption{source, at})
		}
	}
	for _, result := range s.results {
		if result.kind == ResultFired && result.calendarSource == source {
			fence(calendarConsumption{source, result.calendarAlarmAt})
		}
	}
	for _, value := range s.calendarClaiming {
		fence(value)
	}
	var keys [][32]byte
	for key, value := range s.calendarMissed {
		if value.source == source {
			keys = append(keys, key)
		}
	}
	return through, keys
}

func (s *Service) preservedCalendarDueLocked(window CalendarWindow, now time.Time) (map[int]bool, map[calendarKey]bool) {
	preserved := map[int]bool{}
	keys := map[calendarKey]bool{}
	prior, present := s.windows[window.source]
	if !present || s.calendarDegraded[window.source] || !prior.toUTC.After(now) {
		return preserved, keys
	}
	if prior.remote != nil && (prior.remote.stale || !prior.remote.deadline.live(now, s.clock.MonoNow())) {
		return preserved, keys
	}
	for i, r := range s.plan.rules {
		due, armed := s.scheduled[i]
		if !armed || due.After(now) || r.trigger.Kind() != ir.TriggerCalendar || r.calendar != window.source {
			continue
		}
		for _, old := range prior.occurrences {
			oldKey, oldAt, err := calendarAlarm(r, old)
			if err != nil || !oldAt.Equal(due) {
				continue
			}
			for _, current := range window.occurrences {
				key, at, err := calendarAlarm(r, current)
				if err == nil && key == oldKey && at.Equal(due) {
					preserved[i] = true
					keys[key] = true
					break
				}
			}
		}
	}
	return preserved, keys
}
