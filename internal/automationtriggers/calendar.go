package automationtriggers

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/sequencer"
)

const (
	MaxCalendarOccurrences   = 512
	MaxCalendarSeen          = 4096
	MaxCalendarSeenBytes     = 2 << 20
	MaxCalendarHorizon       = 48 * time.Hour
	calendarOccurrenceCharge = 1536
	calendarResultReserve    = 640 // 256 global headroom plus a maximal source diagnostic.
)

type CalendarSourceID struct{ value string }

func validCalendarID(raw string) bool {
	return raw != "" && len(raw) <= 256 && utf8.ValidString(raw) && strings.TrimSpace(raw) == raw &&
		!strings.ContainsAny(raw, "\x00\r\n")
}

func ParseCalendarSourceID(raw string) (CalendarSourceID, error) {
	if !validCalendarID(raw) {
		return CalendarSourceID{}, ErrInvalidPlan
	}
	return CalendarSourceID{value: raw}, nil
}
func (id CalendarSourceID) Valid() bool { return validCalendarID(id.value) }
func (id CalendarSourceID) Value() (string, error) {
	if !id.Valid() {
		return "", ErrInvalidPlan
	}
	return id.value, nil
}

// CalendarOccurrence is a source-resolved, stable instance identity. A
// recurrence revision can move its time without changing this identity.
type CalendarOccurrence struct {
	source            CalendarSourceID
	uid, recurrenceID string
	startUTC, endUTC  time.Time
	zone              string
	allDay            bool
	freshness         domain.Freshness
	valid             bool
}

func NewCalendarOccurrence(source CalendarSourceID, uid, recurrenceID string, startUTC, endUTC time.Time, zone string, allDay bool, freshness domain.Freshness) (CalendarOccurrence, error) {
	if !source.Valid() || !validCalendarID(uid) || !validCalendarID(recurrenceID) ||
		startUTC.IsZero() || endUTC.IsZero() || !endUTC.After(startUTC) ||
		zone == "" || len(zone) > 256 || freshness != domain.FreshnessFresh && freshness != domain.FreshnessStale {
		return CalendarOccurrence{}, ErrInvalidPlan
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return CalendarOccurrence{}, ErrInvalidPlan
	}
	if allDay {
		start, end := startUTC.In(loc), endUTC.In(loc)
		if start.Hour() != 0 || start.Minute() != 0 || start.Second() != 0 || start.Nanosecond() != 0 ||
			end.Hour() != 0 || end.Minute() != 0 || end.Second() != 0 || end.Nanosecond() != 0 {
			return CalendarOccurrence{}, ErrInvalidPlan
		}
	}
	return CalendarOccurrence{source, uid, recurrenceID, startUTC.UTC(), endUTC.UTC(), zone, allDay, freshness, true}, nil
}

func (o CalendarOccurrence) Components() (CalendarSourceID, string, string, time.Time, time.Time, string, bool, domain.Freshness, error) {
	if !o.valid || !o.source.Valid() || !validCalendarID(o.uid) || !validCalendarID(o.recurrenceID) ||
		o.startUTC.IsZero() || !o.endUTC.After(o.startUTC) ||
		o.freshness != domain.FreshnessFresh && o.freshness != domain.FreshnessStale {
		return CalendarSourceID{}, "", "", time.Time{}, time.Time{}, "", false, 0, ErrInvalidPlan
	}
	return o.source, o.uid, o.recurrenceID, o.startUTC, o.endUTC, o.zone, o.allDay, o.freshness, nil
}

type CalendarWindow struct {
	source         CalendarSourceID
	revision       uint64
	fromUTC, toUTC time.Time
	missedFrom     *time.Time
	occurrences    []CalendarOccurrence
	valid          bool
}

func NewCalendarWindow(source CalendarSourceID, revision uint64, fromUTC, toUTC time.Time, missedFrom *time.Time, occurrences []CalendarOccurrence) (CalendarWindow, error) {
	if !source.Valid() || revision == 0 || fromUTC.IsZero() || toUTC.IsZero() || !toUTC.After(fromUTC) ||
		toUTC.Sub(fromUTC) > MaxCalendarHorizon || len(occurrences) > MaxCalendarOccurrences {
		return CalendarWindow{}, ErrInvalidPlan
	}
	w := CalendarWindow{source: source, revision: revision, fromUTC: fromUTC.UTC(), toUTC: toUTC.UTC(), valid: true}
	if missedFrom != nil {
		if missedFrom.IsZero() || !missedFrom.Before(fromUTC) {
			return CalendarWindow{}, ErrInvalidPlan
		}
		copyTime := missedFrom.UTC()
		w.missedFrom = &copyTime
	}
	seen := make(map[string]bool, len(occurrences))
	for _, occurrence := range occurrences {
		occurrenceSource, uid, recurrence, start, end, _, _, _, err := occurrence.Components()
		if err != nil || occurrenceSource != source || !end.After(w.fromUTC) || !start.Before(w.toUTC) || !end.After(start) {
			return CalendarWindow{}, ErrInvalidPlan
		}
		identity := uid + "\x00" + recurrence
		if seen[identity] {
			return CalendarWindow{}, ErrInvalidPlan
		}
		seen[identity] = true
		w.occurrences = append(w.occurrences, occurrence)
	}
	return w, nil
}

func (w CalendarWindow) Components() (CalendarSourceID, uint64, time.Time, time.Time, *time.Time, []CalendarOccurrence, error) {
	if !w.valid || !w.source.Valid() || w.revision == 0 || w.fromUTC.IsZero() || !w.toUTC.After(w.fromUTC) || len(w.occurrences) > MaxCalendarOccurrences {
		return CalendarSourceID{}, 0, time.Time{}, time.Time{}, nil, nil, ErrInvalidPlan
	}
	var missed *time.Time
	if w.missedFrom != nil {
		copyTime := *w.missedFrom
		missed = &copyTime
	}
	return w.source, w.revision, w.fromUTC, w.toUTC, missed, append([]CalendarOccurrence(nil), w.occurrences...), nil
}

func calendarAlarm(r rule, occurrence CalendarOccurrence) (calendarKey, time.Time, error) {
	source, uid, instance, start, end, _, _, _, err := occurrence.Components()
	if err != nil || source != r.calendar {
		return calendarKey{}, time.Time{}, ErrInvalidPlan
	}
	var instant time.Time
	switch r.trigger.CalendarEdge() {
	case ir.CalendarEdgeStart:
		instant = start
	case ir.CalendarEdgeEnd:
		instant = end
	default:
		return calendarKey{}, time.Time{}, ErrInvalidPlan
	}
	instant = instant.Add(time.Duration(r.trigger.OffsetSeconds()) * time.Second)
	if instant.IsZero() {
		return calendarKey{}, time.Time{}, ErrInvalidPlan
	}
	return calendarKey{source: source, uid: uid, instance: instance, trigger: r.automation + "\x00" + r.trigger.Key(), edge: uint8(r.trigger.CalendarEdge())}, instant, nil
}

func calendarKeyCharge(key calendarKey) int {
	return 96 + len(key.source.value) + len(key.uid) + len(key.instance) + len(key.trigger)
}

// ReplaceCalendarWindow validates the complete replacement before publishing
// it. Seen keys survive revisions and remain for this process's entire run.
func (s *Service) ReplaceCalendarWindow(ctx context.Context, window CalendarWindow) error {
	if s == nil || nilInterface(ctx) {
		return ErrInvalidPlan
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !window.valid || len(window.occurrences) > MaxCalendarOccurrences {
		return ErrInvalidPlan
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.closed || !s.admissionsOpen {
		return ErrServiceClosed
	}
	// Count the prospective window while it still belongs to the caller. A
	// validated Components call copies its slice, so the aggregate admission
	// must precede that copy when other sources already occupy the budget.
	source := window.source
	occurrenceCount := len(window.occurrences)
	for existingSource, existing := range s.windows {
		if existingSource != source {
			occurrenceCount += len(existing.occurrences)
		}
	}
	if occurrenceCount > MaxCalendarOccurrences || s.estimatedChargeLocked(occurrenceCount, s.seenBytes) > MaxEngineCharge-calendarResultReserve {
		s.rejectCalendarLocked(source, DegradedCapacity)
		return ErrCapacity
	}
	_, _, from, to, missedFrom, occurrences, err := window.Components()
	if err != nil {
		return err
	}
	stale := false
	for _, item := range occurrences {
		_, _, _, _, _, _, _, freshness, err := item.Components()
		if err != nil {
			return err
		}
		if freshness == domain.FreshnessStale {
			stale = true
		}
	}
	now := s.clock.Now().UTC()
	if !to.After(now) || to.Sub(now) > MaxCalendarHorizon || from.After(to) {
		return ErrInvalidPlan
	}
	// Reserve every potentially new key, including future borders. This keeps
	// later fires within the seen-set budget without evicting a useful key.
	prospective := make(map[calendarKey]struct{})
	prospectiveBytes := s.seenBytes
	collect := func(r rule, item CalendarOccurrence) error {
		key, _, err := calendarAlarm(r, item)
		if err != nil {
			return err
		}
		if _, seen := s.seen[key]; seen {
			return nil
		}
		if _, seen := prospective[key]; seen {
			return nil
		}
		if len(s.seen)+len(prospective) == MaxCalendarSeen {
			return ErrCapacity
		}
		charge := calendarKeyCharge(key)
		if charge > MaxCalendarSeenBytes-prospectiveBytes {
			return ErrCapacity
		}
		prospective[key] = struct{}{}
		prospectiveBytes += charge
		return nil
	}
	for existingSource, existing := range s.windows {
		if existingSource == source {
			continue
		}
		for _, r := range s.plan.rules {
			if r.trigger.Kind() != ir.TriggerCalendar || r.calendar != existingSource {
				continue
			}
			for _, item := range existing.occurrences {
				if err := collect(r, item); err != nil {
					s.rejectCalendarLocked(source, DegradedCapacity)
					return err
				}
			}
		}
	}
	for _, r := range s.plan.rules {
		if r.trigger.Kind() != ir.TriggerCalendar || r.calendar != source {
			continue
		}
		for _, item := range occurrences {
			if err := collect(r, item); err != nil {
				s.rejectCalendarLocked(source, DegradedCapacity)
				return err
			}
		}
	}
	if s.estimatedChargeLocked(occurrenceCount, prospectiveBytes) > MaxEngineCharge-calendarResultReserve {
		s.rejectCalendarLocked(source, DegradedCapacity)
		return ErrCapacity
	}
	// A stale window remains observable, but none of its borders can arm or
	// consume a seen key. A later fresh revision accounts for elapsed borders
	// as missed without replaying them.
	if stale {
		s.windows[source] = window
		s.rejectCalendarLocked(source, DegradedSource)
		return nil
	}
	missed := missedFrom != nil
	for _, r := range s.plan.rules {
		if r.trigger.Kind() != ir.TriggerCalendar || r.calendar != source {
			continue
		}
		for _, item := range occurrences {
			key, instant, err := calendarAlarm(r, item)
			if err != nil {
				return err
			}
			if instant.After(now) && instant.After(s.lastWall) {
				continue
			}
			if _, seen := s.seen[key]; seen {
				continue
			}
			s.seen[key] = struct{}{}
			s.seenBytes += calendarKeyCharge(key)
			missed = true
		}
	}
	s.windows[source] = window
	delete(s.calendarDegraded, source)
	if missed {
		s.enqueueLocked(Result{kind: ResultMissed, triggerKey: source.value, at: now})
	}
	for i, r := range s.plan.rules {
		if r.trigger.Kind() == ir.TriggerCalendar && r.calendar == source {
			delete(s.scheduled, i)
			delete(s.scheduledMono, i)
			s.scheduleRuleLocked(i, now)
		}
	}
	s.signalWake()
	return nil
}

func (s *Service) rejectCalendarLocked(source CalendarSourceID, reason DegradedReason) {
	s.calendarDegraded[source] = true
	for i, r := range s.plan.rules {
		if r.trigger.Kind() == ir.TriggerCalendar && r.calendar == source {
			delete(s.scheduled, i)
			delete(s.scheduledMono, i)
		}
	}
	s.enqueueLocked(Result{kind: ResultDegraded, reason: reason, triggerKey: source.value, at: s.clock.Now().UTC()})
	s.signalWake()
}

func (s *Service) estimatedChargeLocked(occurrences, prospectiveSeenBytes int) int {
	return runtimeFixedCharge + sequencer.MaxViewQueueBytes + s.projectionBytes + s.replayBytes +
		s.plan.charge + len(s.pending)*128 + occurrences*calendarOccurrenceCharge + prospectiveSeenBytes + s.resultBytes
}

func (s *Service) occurrenceCountLocked() int {
	count := 0
	for _, window := range s.windows {
		count += len(window.occurrences)
	}
	return count
}

func (s *Service) fireCalendarDueLocked(i int, now time.Time, missedByWallJump bool) {
	r := s.plan.rules[i]
	if s.calendarDegraded[r.calendar] {
		return
	}
	window, ok := s.windows[r.calendar]
	if !ok {
		return
	}
	_, _, _, to, _, occurrences, err := window.Components()
	if err != nil || !to.After(now) {
		return
	}
	for _, item := range occurrences {
		_, _, _, _, _, _, _, freshness, err := item.Components()
		if err != nil || freshness != domain.FreshnessFresh {
			continue
		}
		key, instant, err := calendarAlarm(r, item)
		if err != nil || instant.After(now) {
			continue
		}
		if _, seen := s.seen[key]; seen {
			continue
		}
		charge := calendarKeyCharge(key)
		if len(s.seen) >= MaxCalendarSeen || charge > MaxCalendarSeenBytes-s.seenBytes {
			s.rejectCalendarLocked(r.calendar, DegradedCapacity)
			return
		}
		s.seen[key] = struct{}{}
		s.seenBytes += charge
		if missedByWallJump || instant.Before(s.lastWall) {
			s.enqueueLocked(Result{kind: ResultMissed, automation: r.automation, triggerKey: r.trigger.Key(), at: now})
		} else {
			s.fireLocked(i, s.cursor, domain.EntityID{}, nil)
		}
		if s.suspended {
			return
		}
	}
}
