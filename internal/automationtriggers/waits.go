package automationtriggers

import (
	"context"
	"github.com/damione1/Homeward/internal/mqtt"
	"sort"
	"strconv"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
)

const MaxWaits = 64

// WaitTrigger is selected from the admitted rule, never reconstructed from a
// queue label. The index is relative to this wait's own trigger list.
type WaitTrigger struct {
	id    string
	index int
	kind  ir.TriggerKind
}

func (t WaitTrigger) ID() string           { return t.id }
func (t WaitTrigger) Index() int           { return t.index }
func (t WaitTrigger) Kind() ir.TriggerKind { return t.kind }

// WaitHandle is a one-shot observer in the existing 040 engine. It never reads
// ResultReader and owns no sequencer subscription, broker connection or timer.
// Done closes only after disarm and all source calls using this observer join.
type WaitHandle struct {
	service            *Service
	id                 uint64
	slots              []int
	ctx                context.Context
	cancel             context.CancelFunc
	stopParent         func() bool
	parentJoined       bool
	ready, done        chan struct{}
	result             Result
	err                error
	completed, closing bool
	planning           int
	resultCharge       int
	trigger            WaitTrigger
	completedAt        time.Duration
}

func (h *WaitHandle) Ready() <-chan struct{} {
	if h == nil {
		return nil
	}
	return h.ready
}
func (h *WaitHandle) Done() <-chan struct{} {
	if h == nil {
		return nil
	}
	return h.done
}
func (h *WaitHandle) Outcome() (Result, error) {
	if h == nil || h.service == nil {
		return Result{}, ErrInvalidPlan
	}
	h.service.mu.Lock()
	defer h.service.mu.Unlock()
	if !h.completed {
		return Result{}, ErrInvalidPlan
	}
	return h.result, h.err
}
func (h *WaitHandle) Trigger() (WaitTrigger, bool) {
	if h == nil || h.service == nil {
		return WaitTrigger{}, false
	}
	h.service.mu.Lock()
	defer h.service.mu.Unlock()
	return h.trigger, h.completed && h.err == nil && h.trigger.kind != ir.TriggerInvalid
}
func (h *WaitHandle) CompletedAt() (time.Duration, bool) {
	if h == nil || h.service == nil {
		return 0, false
	}
	h.service.mu.Lock()
	defer h.service.mu.Unlock()
	return h.completedAt, h.completed && h.err == nil && h.trigger.kind != ir.TriggerInvalid
}
func (h *WaitHandle) Close() <-chan struct{} {
	if h == nil || h.service == nil {
		return nil
	}
	h.service.mu.Lock()
	if h.resultCharge != 0 {
		h.service.resultBytes -= h.resultCharge
		h.resultCharge = 0
	}
	h.service.detachWaitLocked(h, context.Canceled)
	h.service.mu.Unlock()
	h.service.signalWake()
	return h.done
}

// ObserveWait uses the exact source in an admitted IR program. Every trigger
// variant has already followed resolveRule, including all static MQTT topics.
// Registration flushes preceding journal entries before arming: an old state
// matching "to" is not a newly fired wait_for_trigger.
func (s *Service) ObserveWait(ctx context.Context, source ir.Source) (*WaitHandle, error) {
	if s == nil || nilInterface(ctx) || ctx.Err() != nil {
		return nil, ErrInvalidPlan
	}
	key := waitSource{source.Object(), source.Path()}
	s.mu.Lock()
	defer s.mu.Unlock()
	definition, present := s.plan.waits[key]
	if !present {
		return nil, ErrInvalidPlan
	}
	if !s.started || s.closed || !s.admissionsOpen || s.suspended || s.ctx.Err() != nil {
		return nil, ErrServiceClosed
	}
	if len(s.waits) >= MaxWaits || s.nextWait == ^uint64(0) {
		return nil, ErrCapacity
	}
	s.flushJournalLocked()
	// Existing source planning may release mu while joining a SunSource.
	// Revalidate shutdown and caller cancellation before creating ownership.
	if !s.started || s.closed || !s.admissionsOpen || s.suspended || s.ctx.Err() != nil {
		return nil, ErrServiceClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	slots := make([]int, 0, len(definition.rules))
	for index := s.permanentRules; index < len(s.plan.rules) && len(slots) < len(definition.rules); index++ {
		if s.waitOwners[index] == nil {
			slots = append(slots, index)
		}
	}
	if len(slots) != len(definition.rules) {
		return nil, ErrCapacity
	}
	for _, rule := range definition.rules {
		if rule.trigger.Kind() != ir.TriggerCalendar {
			continue
		}
		window, present := s.windows[rule.calendar]
		if !present || s.calendarDegraded[rule.calendar] || !window.toUTC.After(s.clock.Now()) {
			return nil, ErrInvalidPlan
		}
	}
	s.nextWait++
	runCtx, cancel := context.WithCancel(s.ctx)
	h := &WaitHandle{service: s, id: s.nextWait, slots: slots, ctx: runCtx, cancel: cancel,
		ready: make(chan struct{}), done: make(chan struct{}), planning: 1}
	s.waits[h.id] = h
	s.waitPlanning.Add(1)
	defer s.waitPlanning.Done()
	s.waitPlanning.Add(1) // the armed parent callback joins service shutdown too
	h.stopParent = context.AfterFunc(ctx, func() {
		s.mu.Lock()
		s.detachWaitLocked(h, ctx.Err())
		// A failed stopParent means this actual callback was already in
		// flight. Its acknowledgement participates in the same Done join.
		s.finishWaitParentLocked(h)
		s.signalWake()
		s.finishWaitJoinLocked(h)
		s.mu.Unlock()
	})
	for index, slot := range slots {
		rule := definition.rules[index]
		rule.wait = h.id
		rule.waitIndex = index
		s.plan.rules[slot] = rule
		s.waitOwners[slot] = h
		if rule.trigger.Kind() == ir.TriggerNumericState {
			if state, present := s.states[rule.entity]; present {
				if unit, valid := numericMeasurementUnit(rule.trigger, state); valid {
					s.numericUnits[slot] = unit
				}
				_, matching, valid := numericMatch(rule.trigger, state)
				if valid {
					s.armed[slot] = !matching
				}
			}
		}
	}
	for _, slot := range slots {
		if h.closing {
			break
		}
		s.scheduleRuleLocked(slot, s.clock.Now().UTC())
	}
	h.planning--
	s.finishWaitJoinLocked(h)
	if h.closing && h.err != nil {
		// A failed registration still owns its real source/parent callbacks.
		// Join after releasing mu; no nil handle can hide unfinished cleanup.
		done, err := h.done, h.err
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		return nil, err
	}
	// A trigger completed while source planning released mu is a genuine
	// armed observation. Return its completed handle for consumption/Close.
	s.signalWake()
	return h, nil
}

func (s *Service) detachWaitLocked(h *WaitHandle, err error) {
	if h == nil || h.closing {
		return
	}
	h.closing = true
	h.cancel()
	if h.stopParent != nil && h.stopParent() {
		s.finishWaitParentLocked(h)
	}
	for _, slot := range h.slots {
		if s.waitOwners[slot] != h {
			continue
		}
		delete(s.waitOwners, slot)
		s.plan.rules[slot] = rule{}
		delete(s.pending, slot)
		delete(s.armed, slot)
		delete(s.numericUnits, slot)
		delete(s.civil, slot)
		delete(s.civilUTC, slot)
		delete(s.mqttWater, slot)
		delete(s.scheduled, slot)
		delete(s.scheduledMono, slot)
	}
	delete(s.waits, h.id)
	for key := range s.seen {
		if key.wait != h.id {
			continue
		}
		s.seenBytes -= calendarKeyCharge(key)
		delete(s.seen, key)
	}
	if !h.completed {
		h.completed, h.err = true, err
		close(h.ready)
	}
	s.finishWaitJoinLocked(h)
}
func (s *Service) finishWaitJoinLocked(h *WaitHandle) {
	if !h.closing || h.planning != 0 || !h.parentJoined {
		return
	}
	// planning becomes zero exactly once after disarm. No callback can acquire
	// this observer again because its slots have already been removed.
	if h.done != nil {
		select {
		case <-h.done:
		default:
			close(h.done)
		}
	}
}
func (s *Service) finishWaitParentLocked(h *WaitHandle) {
	if h.parentJoined {
		return
	}
	h.parentJoined = true
	s.waitPlanning.Done()
}
func (s *Service) fireWaitLocked(h *WaitHandle, result Result, selected rule) {
	if h == nil || h.closing || h.completed {
		return
	}
	charge := resultCharge(result)
	if len(result.payload) > 64<<10 || charge > MaxOutputBytes-256-s.resultBytes ||
		s.estimatedChargeLocked(s.occurrenceCountLocked(), s.seenBytes)+charge > MaxEngineCharge-256 {
		s.detachWaitLocked(h, ErrCapacity)
		return
	}
	result.payload = append([]byte(nil), result.payload...)
	h.result, h.completed = result, true
	id := selected.trigger.ID()
	if id == "" {
		id = strconv.Itoa(selected.waitIndex)
	}
	h.trigger = WaitTrigger{id: id, index: selected.waitIndex, kind: selected.trigger.Kind()}
	h.completedAt = s.clock.MonoNow()
	// Reserve the completed observation until the interpreter has consumed it
	// and closes the handle; completion itself disarms all sibling triggers.
	h.resultCharge = charge
	s.resultBytes += charge
	close(h.ready)
	s.detachWaitLocked(h, nil)
}
func (s *Service) closeWaitsLocked(err error) {
	for _, h := range s.waits {
		s.detachWaitLocked(h, err)
	}
}

// MQTTFiltersForPrograms is the pre-bootstrap union for the real pilot
// constructor. It includes waits nested in every branch and every real script,
// even if that script has no startup automation trigger.
func MQTTFiltersForPrograms(automations []ir.Automation, scripts []ir.Script) ([]mqtt.TopicFilter, error) {
	count := 0
	for _, automation := range automations {
		if !automation.Valid() {
			return nil, ErrInvalidPlan
		}
		permanent, _ := automation.TriggerPlanBytes()
		waits, _ := automation.WaitPlanBytes()
		if permanent+waits > 2*MaxTriggers-count {
			return nil, ErrCapacity
		}
		count += permanent + waits
	}
	for _, script := range scripts {
		if !script.Valid() {
			return nil, ErrInvalidPlan
		}
		waits, _ := script.WaitPlanBytes()
		if waits > 2*MaxTriggers-count {
			return nil, ErrCapacity
		}
		count += waits
	}
	found := make(map[mqtt.TopicFilter]bool)
	add := func(triggers []ir.Trigger) {
		for _, trigger := range triggers {
			if trigger.Kind() == ir.TriggerMQTT {
				found[trigger.Topic()] = true
			}
		}
	}
	for _, automation := range automations {
		add(automation.Triggers())
		for _, wait := range automation.Waits() {
			add(wait.Triggers())
		}
	}
	for _, script := range scripts {
		for _, wait := range script.Waits() {
			add(wait.Triggers())
		}
	}
	if len(found) > 16 {
		return nil, ErrCapacity
	}
	result := make([]mqtt.TopicFilter, 0, len(found))
	for filter := range found {
		result = append(result, filter)
	}
	sort.Slice(result, func(i, j int) bool { a, _ := result[i].Value(); b, _ := result[j].Value(); return a < b })
	return result, nil
}

// HasWait validates interpreter/source composition before any run admission.
// The definitions map is immutable for the lifetime of this service.
func (s *Service) HasWait(source ir.Source) bool {
	if s == nil {
		return false
	}
	_, present := s.plan.waits[waitSource{source.Object(), source.Path()}]
	return present
}
