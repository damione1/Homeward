package automationtriggers

import (
	"context"
	"fmt"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/domain"
)

func civilKey(at time.Time, location *time.Location) string {
	local := at.In(location)
	return fmt.Sprintf("%04d%02d%02d%02d%02d%02d", local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), local.Second())
}

func patternMatches(part ir.PatternPart, value int) bool {
	switch part.Kind() {
	case ir.PatternPartAny:
		return true
	case ir.PatternPartExact:
		return value == part.Value()
	case ir.PatternPartEvery:
		return part.Value() > 0 && value%part.Value() == 0
	default:
		return false
	}
}

func nextCivilTime(now time.Time, location *time.Location, trigger ir.Trigger, highWater string) (time.Time, bool) {
	search := now
	if highWater != "" {
		last, err := time.ParseInLocation("20060102150405", highWater, location)
		if err != nil {
			return time.Time{}, false
		}
		if last.After(search) {
			search = last
		}
	}
	local := search.In(location)
	dayStart := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
	for day := 0; day < 3; day++ {
		base := dayStart.AddDate(0, 0, day)
		y, m, d := base.Date()
		hourPart, minutePart, secondPart := trigger.Pattern()
		fixedSeconds := trigger.SecondsOfDay()
		hourStart := 0
		if day == 0 {
			hourStart = local.Hour()
		}
		for hour := hourStart; hour < 24; hour++ {
			if trigger.Kind() == ir.TriggerTime {
				if hour != fixedSeconds/3600 {
					continue
				}
			} else if !patternMatches(hourPart, hour) {
				continue
			}
			minuteStart := 0
			if day == 0 && hour == local.Hour() {
				minuteStart = local.Minute()
			}
			for minute := minuteStart; minute < 60; minute++ {
				if trigger.Kind() == ir.TriggerTime {
					if minute != fixedSeconds%3600/60 {
						continue
					}
				} else if !patternMatches(minutePart, minute) {
					continue
				}
				secondStart := 0
				if day == 0 && hour == local.Hour() && minute == local.Minute() {
					secondStart = local.Second()
				}
				for second := secondStart; second < 60; second++ {
					if trigger.Kind() == ir.TriggerTime {
						if second != fixedSeconds%60 {
							continue
						}
					} else if !patternMatches(secondPart, second) {
						continue
					}
					candidate := time.Date(y, m, d, hour, minute, second, 0, location)
					back := candidate.In(location)
					if back.Year() != y || back.Month() != m || back.Day() != d || back.Hour() != hour || back.Minute() != minute || back.Second() != second {
						continue
					}
					if !candidate.After(now) || civilKey(candidate, location) <= highWater {
						continue
					}
					return candidate.UTC(), true
				}
			}
		}
	}
	return time.Time{}, false
}

// Keep the civil instant and its monotonic lower bound together through plan
// calculation, including source calls which temporarily release the mutex.
type civilScheduleSample struct {
	wall time.Time
	mono time.Duration
}

func (s *Service) sampleScheduleTime() civilScheduleSample {
	mono := s.clock.MonoNow()
	return civilScheduleSample{wall: s.clock.Now().UTC(), mono: mono}
}

func (s *Service) scheduleAllLocked() {
	s.planning = true
	defer func() {
		s.planning = false
		s.signalWake()
	}()
	clear(s.scheduled)
	clear(s.scheduledMono)
	if s.suspended || !s.admissionsOpen || s.closed {
		return
	}
	now := s.sampleScheduleTime()
	generation := s.generation
	publication := s.publicationEpoch
	for i := range s.plan.rules {
		if s.generation != generation || s.publicationEpoch != publication || s.closed || !s.admissionsOpen || s.suspended {
			return
		}
		s.scheduleRuleLocked(i, now)
	}
}

func (s *Service) setScheduleLocked(i int, instant time.Time, sample civilScheduleSample) {
	s.scheduled[i] = instant
	delay := instant.Sub(sample.wall)
	if delay < 0 {
		delay = 0
	}
	s.scheduledMono[i] = sample.mono + delay
}

func (s *Service) scheduleRuleLocked(i int, sample civilScheduleSample) {
	if s.suspended || !s.admissionsOpen || s.closed {
		return
	}
	r := s.plan.rules[i]
	owner := s.waitOwners[i]
	if owner == nil && s.activePlanImage == nil {
		return
	}
	location := s.plan.location
	if owner != nil && owner.admitted != nil {
		location = owner.admitted.location
	}
	now := sample.wall
	switch r.trigger.Kind() {
	case ir.TriggerTime, ir.TriggerTimePattern:
		if next, ok := nextCivilTime(now, location, r.trigger, s.civil[i]); ok {
			s.setScheduleLocked(i, next, sample)
		}
	case ir.TriggerSunset:
		offset := time.Duration(r.trigger.OffsetSeconds()) * time.Second
		after := now.Add(-offset)
		if last := s.civilUTC[i].Add(-offset); last.After(after) {
			after = last
		}
		result, err, current := s.nextSunsetLocked(after, owner)
		if !current || owner != nil && (owner.closing || s.waitOwners[i] != owner) {
			return
		}
		if err != nil {
			s.suspendLocked(DegradedSource)
			return
		}
		instant, availability, err := result.Components()
		if err != nil {
			s.suspendLocked(DegradedSource)
			return
		}
		if availability == SunAbsent {
			delete(s.scheduled, i)
			delete(s.scheduledMono, i)
			return
		}
		candidate := instant.Add(offset)
		if !instant.After(after) {
			s.suspendLocked(DegradedSource)
			return
		}
		if candidate.After(now) && civilKey(candidate, location) > s.civil[i] {
			s.setScheduleLocked(i, candidate, sample)
		} else if civilKey(candidate, location) <= s.civil[i] {
			s.suspendLocked(DegradedSource)
		}
	case ir.TriggerCalendar:
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
		var first time.Time
		for _, occurrence := range occurrences {
			key, instant, err := calendarAlarm(r, occurrence)
			if err != nil || !instant.After(now) {
				continue
			}
			if _, seen := s.seen[key]; seen {
				continue
			}
			if first.IsZero() || instant.Before(first) {
				first = instant
			}
		}
		if !first.IsZero() {
			s.setScheduleLocked(i, first, sample)
		}
	}
}

// nextSunsetLocked serializes source calls while releasing the service mutex.
// A generation change or shutdown cancels the in-flight source and discards
// its answer at the original cursor; it never schedules a stale result.
func (s *Service) nextSunsetLocked(after time.Time, owner *WaitHandle) (SunResult, error, bool) {
	parent := s.ctx
	if owner != nil {
		parent = owner.ctx
	} else if s.publicationContext != nil {
		parent = s.publicationContext
	}

	generation := s.generation
	publication := s.publicationEpoch
	for s.sunDone != nil {
		done, ctx := s.sunDone, parent
		s.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
		}
		s.mu.Lock()
		if s.closed || s.suspended || !s.admissionsOpen || s.generation != generation || owner == nil && s.publicationEpoch != publication || s.ctx.Err() != nil || parent.Err() != nil {
			return SunResult{}, nil, false
		}
	}
	if parent.Err() != nil {
		return SunResult{}, nil, false
	}
	callCtx, cancel := context.WithCancel(parent)
	if owner != nil {
		owner.planning++
	}

	done := make(chan struct{})
	s.sunDone, s.cancelSun, s.sunOwner = done, cancel, owner
	source, zone, position := s.plan.sun, s.plan.zone, s.plan.position
	if owner != nil && owner.admitted != nil {
		source, zone, position = owner.admitted.sun, owner.admitted.zone, owner.admitted.position
	}
	s.mu.Unlock()
	result, err := source.NextSunset(callCtx, after, zone, position)
	s.mu.Lock()
	cancel()
	s.sunDone, s.cancelSun, s.sunOwner = nil, nil, nil
	if owner != nil {
		owner.planning--
		s.finishWaitJoinLocked(owner)
	}
	close(done)
	if s.closed || s.suspended || !s.admissionsOpen || s.generation != generation || owner == nil && s.publicationEpoch != publication || s.ctx.Err() != nil || parent.Err() != nil {
		return SunResult{}, nil, false
	}
	return result, err, true
}

func (s *Service) nextDelayLocked() (time.Duration, bool) {
	if s.suspended || !s.admissionsOpen || s.planning {
		return 0, false
	}
	nowWall := s.clock.Now().UTC()
	nowMono := s.clock.MonoNow()
	var earliest time.Duration
	ready := false
	for _, pending := range s.pending {
		d := pending.deadline - nowMono
		if d < 0 {
			d = 0
		}
		if !ready || d < earliest {
			earliest, ready = d, true
		}
	}
	for _, instant := range s.scheduled {
		d := instant.Sub(nowWall)
		if d < 0 {
			d = 0
		}
		if !ready || d < earliest {
			earliest, ready = d, true
		}
	}
	return earliest, ready
}

func (s *Service) timerLoop() {
	defer close(s.timerDone)
	for {
		s.mu.Lock()
		ctx := s.ctx
		delay, hasTimer := s.nextDelayLocked()
		s.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		var timer Timer
		var timerC <-chan time.Time
		if hasTimer {
			timer = s.clock.NewTimer(delay)
			if nilInterface(timer) {
				s.mu.Lock()
				s.suspendLocked(DegradedSource)
				s.mu.Unlock()
			} else {
				timerC = timer.C()
			}
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-s.wake:
			if timer != nil {
				timer.Stop()
			}
		case <-timerC:
			if timer != nil {
				timer.Stop()
			}
			s.mu.Lock()
			if !s.suspended && s.admissionsOpen && !s.planning {
				s.flushJournalLocked()
				if !s.suspended {
					s.runDueLocked()
				}
			}
			s.mu.Unlock()
		}
	}
}

// Drain admitted sequence entries before considering a timer. An interruption
// admitted at 29 seconds cannot lose the race to a 30-second for callback.
func (s *Service) flushJournalLocked() {
	for i := 0; i < maxReplayEntries; i++ {
		entries, _, err := s.seq.ReadAfter(s.cursor, 1)
		if err != nil {
			s.suspendLocked(DegradedGap)
			return
		}
		if len(entries) == 0 {
			return
		}
		s.processEntryLocked(entries[0])
		if s.suspended {
			return
		}
	}
	entries, _, err := s.seq.ReadAfter(s.cursor, 1)
	if err != nil || len(entries) != 0 {
		s.suspendLocked(DegradedGap)
	}
}

func (s *Service) runDueLocked() {
	sample := s.sampleScheduleTime()
	nowWall := sample.wall
	// Sample elapsed time after the civil reading when deciding whether the
	// deadline was reached; clock-read latency must not look like a wall jump.
	nowMono := s.clock.MonoNow()
	generation := s.generation
	publication := s.publicationEpoch
	for i := range s.plan.rules {
		if s.generation != generation || s.publicationEpoch != publication || s.closed || s.suspended || !s.admissionsOpen {
			return
		}
		pending, present := s.pending[i]
		if !present {
			continue
		}
		if pending.deadline > nowMono {
			continue
		}
		delete(s.pending, i)
		if pending.generation != s.generation {
			continue
		}
		r := s.plan.rules[i]
		state, ok := s.states[r.entity]
		if !ok {
			continue
		}
		switch r.trigger.Kind() {
		case ir.TriggerState:
			text, present := observedText(r.trigger.Attribute(), state)
			if present && (triggerFromOnly(r.trigger) && text != pending.oldText || !triggerFromOnly(r.trigger) && text == pending.newText) {
				s.fireLocked(i, s.cursor, r.entity, nil)
			}
		case ir.TriggerNumericState:
			_, matching, valid := numericMatch(r.trigger, state)
			if valid && matching {
				s.fireLocked(i, s.cursor, r.entity, nil)
			}
		}
		if s.suspended {
			return
		}
	}
	for i := range s.plan.rules {
		if s.generation != generation || s.closed || s.suspended || !s.admissionsOpen {
			return
		}
		instant, present := s.scheduled[i]
		if !present {
			continue
		}
		if instant.After(nowWall) {
			continue
		}
		delete(s.scheduled, i)
		deadlineMono := s.scheduledMono[i]
		delete(s.scheduledMono, i)
		late := nowMono - deadlineMono
		if late < 0 {
			late = 0
		}
		missedByWallJump := nowMono < deadlineMono || nowWall.Sub(instant) > late+time.Second
		r := s.plan.rules[i]
		switch r.trigger.Kind() {
		case ir.TriggerTime, ir.TriggerTimePattern, ir.TriggerSunset:
			location := s.plan.location
			if owner := s.waitOwners[i]; owner != nil && owner.admitted != nil {
				location = owner.admitted.location
			}
			key := civilKey(instant, location)
			if key > s.civil[i] {
				s.civil[i] = key
				s.civilUTC[i] = instant
				if missedByWallJump {
					s.enqueueLocked(Result{kind: ResultMissed, automation: r.automation, triggerKey: r.trigger.Key(), at: nowWall})
				} else {
					s.fireLocked(i, s.cursor, domain.EntityID{}, nil)
				}
			}
		case ir.TriggerCalendar:
			s.fireCalendarDueLocked(i, nowWall, missedByWallJump)
		}
		if s.suspended {
			return
		}
		s.scheduleRuleLocked(i, sample)
	}
	if s.generation != generation || s.closed || s.suspended || !s.admissionsOpen {
		return
	}
	if nowWall.After(s.lastWall) {
		s.lastWall = nowWall
	}
}
