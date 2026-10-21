package automationtriggers

import (
	"context"
	"unsafe"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/domain"
)

// retainedPlan is a single admitted 040 image. The active service and every
// compiled interpreter that can still request a wait hold explicit references.
type retainedPlan struct {
	plan          Plan
	refs          int
	stagingCharge int
}

// PlanRetention is a one-owner lease for a previously admitted plan in the
// same running 040 service. It is not a second trigger engine.
type PlanRetention struct {
	service      *Service
	image        *retainedPlan
	state        planRetentionState
	stagedRules  []rule
	stagedSlots  map[int]bool
	stagedWaits  map[int]uint64
	stageCharge  int
	scheduled    bool
	detachedJoin <-chan struct{}
}

const publicationMapBaseCharge = 4 << 10

// The 040 fixed runtime charge covers one active rule array. A candidate
// needs another array plus two maps until the old image's last reference is
// gone. Four times the map entry payload and a base per map cover buckets.
func publicationStagingCharge(waits, permanent int) int {
	return MaxTriggers*int(unsafe.Sizeof(rule{})) + 2*publicationMapBaseCharge +
		4*(waits*int(unsafe.Sizeof(struct {
			key   int
			value uint64
		}{}))+
			permanent*int(unsafe.Sizeof(struct {
				key   int
				value bool
			}{})))
}

// PreparePublication allocates and validates the B permanent image before
// the SQL commit/publication point. The bounded wait map reserves every slot:
// waits can legitimately finish or start while SQL is in flight.
func (r *PlanRetention) PreparePublication() error {
	if r == nil || r.service == nil || r.image == nil {
		return ErrInvalidPlan
	}
	s := r.service
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.state != planRetentionPrepared || s.pendingPlan != r || s.closed || !s.started {
		return ErrInvalidPlan
	}
	if len(r.image.plan.rules) > MaxTriggers-len(s.waitOwners) ||
		s.estimatedChargeLocked(s.occurrenceCountLocked(), s.seenBytes) > MaxEngineCharge-256 {
		return ErrCapacity
	}
	charge := publicationStagingCharge(MaxTriggers, len(r.image.plan.rules))
	if charge > MaxEngineCharge-256-s.estimatedChargeLocked(s.occurrenceCountLocked(), s.seenBytes) {
		return ErrCapacity
	}
	s.stagingCharge += charge
	r.stageCharge = charge
	rules := make([]rule, MaxTriggers)
	waits := make(map[int]uint64, MaxTriggers)
	for slot, owner := range s.waitOwners {
		waits[slot] = owner.id
		rules[slot] = s.plan.rules[slot]
	}
	permanent := make(map[int]bool, len(r.image.plan.rules))
	next := 0
	for _, admitted := range r.image.plan.rules {
		for next < MaxTriggers && s.waitOwners[next] != nil {
			next++
		}
		if next == MaxTriggers {
			s.stagingCharge -= r.stageCharge
			r.stageCharge = 0
			return ErrCapacity
		}
		rules[next], permanent[next] = admitted, true
		next++
	}
	r.stagedRules, r.stagedSlots, r.stagedWaits = rules, permanent, waits
	r.state = planRetentionStaged
	return nil
}

type planRetentionState uint8

const (
	planRetentionInvalid planRetentionState = iota
	planRetentionCurrent
	planRetentionPrepared
	planRetentionStaged
	planRetentionPublished
	planRetentionDetached
	planRetentionReleased
)

func (r *PlanRetention) Identity() *PlanIdentity {
	if r == nil || r.image == nil || r.state == planRetentionReleased {
		return nil
	}
	return r.image.plan.token
}

func (r *PlanRetention) ReadyToSelect() bool {
	if r == nil || r.service == nil {
		return false
	}
	r.service.mu.Lock()
	defer r.service.mu.Unlock()
	return r.state == planRetentionCurrent || r.state == planRetentionPublished
}

func (s *Service) RetainCurrentPlan() (*PlanRetention, error) {
	if s == nil {
		return nil, ErrInvalidPlan
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.activePlanImage == nil {
		return nil, ErrServiceClosed
	}
	s.activePlanImage.refs++
	return &PlanRetention{service: s, image: s.activePlanImage, state: planRetentionCurrent}, nil
}

// RetainPlan reserves the union of the candidate and every still-live image.
// A candidate must be compiled before SQL commit; an unavailable budget is a
// precommit refusal rather than an eviction of a live run or wait.
func (s *Service) RetainPlan(plan Plan) (*PlanRetention, error) {
	if s == nil || !plan.Valid() {
		return nil, ErrInvalidPlan
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrServiceClosed
	}
	if image := s.planImages[plan.token]; image != nil {
		if image != s.activePlanImage {
			return nil, ErrInvalidPlan
		}
		image.refs++
		return &PlanRetention{service: s, image: image, state: planRetentionCurrent}, nil
	}
	if s.pendingPlan != nil {
		return nil, ErrInvalidPlan
	}
	if s.estimatedChargeLocked(s.occurrenceCountLocked(), s.seenBytes)+plan.charge > MaxEngineCharge-256 {
		return nil, ErrCapacity
	}
	image := &retainedPlan{plan: plan, refs: 1}
	s.planImages[plan.token] = image
	s.planCharge += plan.charge
	r := &PlanRetention{service: s, image: image, state: planRetentionPrepared}
	s.pendingPlan = r
	return r, nil
}

func (r *PlanRetention) AdmitWait(source ir.Source) (*AdmittedWait, error) {
	if r == nil || r.service == nil || r.image == nil {
		return nil, ErrInvalidPlan
	}
	r.service.mu.Lock()
	defer r.service.mu.Unlock()
	if r.state == planRetentionReleased || r.image.refs <= 0 {
		return nil, ErrInvalidPlan
	}
	admitted, err := r.service.admitWaitFromPlan(r.image.plan, source)
	if err == nil {
		admitted.image = r.image
	}
	return admitted, err
}

func (r *PlanRetention) Release() error {
	if r == nil || r.service == nil || r.image == nil {
		return ErrInvalidPlan
	}
	s := r.service
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.state == planRetentionReleased || r.state == planRetentionInvalid || r.image.refs <= 0 {
		return ErrInvalidPlan
	}
	if s.pendingPlan == r {
		s.pendingPlan = nil
	}
	if r.stageCharge != 0 {
		s.stagingCharge -= r.stageCharge
		r.stageCharge = 0
	}
	r.state = planRetentionReleased
	r.stagedRules, r.stagedSlots, r.stagedWaits = nil, nil, nil
	s.releasePlanLocked(r.image)
	return nil
}

// Publish replaces only permanent rules in this service. Real transient wait
// slots and their callbacks stay at their indices. The activation owner calls
// this under its common publication barrier, then SchedulePublished outside
// that barrier and before reopening occurrence admission.
func (r *PlanRetention) Publish() error {
	if r == nil || r.service == nil || r.image == nil {
		return ErrInvalidPlan
	}
	s := r.service
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.state != planRetentionStaged || s.pendingPlan != r || r.image.refs <= 0 || s.closed || !s.started || s.activePlanImage == r.image || s.publicationEpoch == ^uint64(0) ||
		len(r.stagedRules) != MaxTriggers {
		return ErrInvalidPlan
	}
	if len(r.image.plan.rules) > MaxTriggers-len(s.waitOwners) ||
		s.estimatedChargeLocked(s.occurrenceCountLocked(), s.seenBytes) > MaxEngineCharge-256 {
		return ErrCapacity
	}
	// Rebuild in the already charged allocations, under the same mutex as
	// wait detach/admission. Preserve actual handles and their current slots.
	clear(r.stagedRules)
	clear(r.stagedWaits)
	clear(r.stagedSlots)
	for slot, owner := range s.waitOwners {
		r.stagedWaits[slot] = owner.id
		r.stagedRules[slot] = s.plan.rules[slot]
	}
	next := 0
	for _, admitted := range r.image.plan.rules {
		for next < MaxTriggers && s.waitOwners[next] != nil {
			next++
		}
		if next == MaxTriggers {
			return ErrCapacity
		}
		r.stagedRules[next], r.stagedSlots[next] = admitted, true
		next++
	}
	for slot := range s.permanentSlots {
		delete(s.pending, slot)
		delete(s.armed, slot)
		delete(s.numericUnits, slot)
		delete(s.civil, slot)
		delete(s.civilUTC, slot)
		delete(s.mqttWater, slot)
		delete(s.scheduled, slot)
		delete(s.scheduledMono, slot)
	}
	for key := range s.seen {
		if key.wait == 0 {
			s.seenBytes -= calendarKeyCharge(key)
			delete(s.seen, key)
		}
	}
	for slot := range r.stagedSlots {
		admitted := r.stagedRules[slot]
		if admitted.trigger.Kind() == ir.TriggerNumericState && admitted.entity != (domain.EntityID{}) {
			if state, present := s.states[admitted.entity]; present {
				if unit, valid := numericMeasurementUnit(admitted.trigger, state); valid {
					s.numericUnits[slot] = unit
				}
				_, matching, valid := numericMatch(admitted.trigger, state)
				if valid {
					s.armed[slot] = !matching
				}
			}
		}
	}
	old := s.activePlanImage
	updated := r.image.plan
	updated.rules = r.stagedRules
	s.plan, s.activePlanImage, s.permanentSlots = updated, r.image, r.stagedSlots
	s.stagingCharge -= s.detachedStagingCharge
	s.detachedStagingCharge = 0
	r.image.stagingCharge = r.stageCharge
	r.stageCharge = 0 // ownership moves to the published retained image
	s.publicationEpoch++
	r.image.refs++
	r.state = planRetentionPublished
	s.pendingPlan = nil
	if old != nil {
		s.releasePlanLocked(old)
	}
	s.signalWake()
	return nil
}

// SchedulePublished can call a SunSource and therefore never runs while the
// caller holds the SQL transaction or common memory publication mutex.
func (r *PlanRetention) SchedulePublished() error {
	return r.SchedulePublication(context.Background())
}

// SchedulePublication is joined by the activation owner. A slow solar port
// receives this bounded context; cancellation neither orphans its call nor
// permanently suspends an otherwise live trigger service.
func (r *PlanRetention) SchedulePublication(ctx context.Context) error {
	if r == nil || r.service == nil || r.image == nil {
		return ErrInvalidPlan
	}
	if nilInterface(ctx) {
		return ErrInvalidPlan
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s := r.service
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.state != planRetentionPublished || r.scheduled || s.closed || !s.started || s.activePlanImage != r.image {
		return ErrInvalidPlan
	}
	bounded, cancel := context.WithCancel(s.ctx)
	stop := context.AfterFunc(ctx, cancel)
	s.publicationContext = bounded
	defer func() { stop(); cancel(); s.publicationContext = nil }()
	now := s.clock.Now().UTC()
	for slot := range s.permanentSlots {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.scheduleRuleLocked(slot, now)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.scheduled = true
	s.signalWake()
	if s.suspended || s.closed || s.ctx.Err() != nil {
		return ErrServiceClosed
	}
	return nil
}

// DiscardPublication removes an unpublished-to-the-house B permanent plan
// during supersession. Real A wait slots stay in their charged arrays; only
// that storage transfers to Service until C replaces it. B's IR/plan charge
// is returned when its interpreter releases the last actual reference.
func (r *PlanRetention) DiscardPublication(ctx context.Context) error {
	if r == nil || r.service == nil || r.image == nil || nilInterface(ctx) {
		return ErrInvalidPlan
	}
	s := r.service
	s.mu.Lock()
	if r.state == planRetentionDetached {
		done := r.detachedJoin
		s.mu.Unlock()
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return ctx.Err()
	}
	if r.state == planRetentionCurrent || r.state == planRetentionPrepared || r.state == planRetentionStaged {
		s.mu.Unlock()
		return nil
	}
	if r.state != planRetentionPublished || s.activePlanImage != r.image || s.pendingPlan != nil || s.closed || s.publicationEpoch == ^uint64(0) || s.detachedStagingCharge != 0 {
		s.mu.Unlock()
		return ErrInvalidPlan
	}
	for slot := range s.permanentSlots {
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
	clear(s.permanentSlots)
	for key := range s.seen {
		if key.wait == 0 {
			s.seenBytes -= calendarKeyCharge(key)
			delete(s.seen, key)
		}
	}
	s.plan = Plan{rules: s.plan.rules}
	s.detachedStagingCharge, r.image.stagingCharge = r.image.stagingCharge, 0
	s.activePlanImage = nil
	r.state = planRetentionDetached
	s.publicationEpoch++
	var done <-chan struct{}
	if s.sunOwner == nil && s.cancelSun != nil {
		s.cancelSun()
		done = s.sunDone
	}
	r.detachedJoin = done
	s.releasePlanLocked(r.image)
	s.signalWake()
	s.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return ctx.Err()
}

func (s *Service) releasePlanLocked(image *retainedPlan) {
	image.refs--
	if image.refs == 0 {
		delete(s.planImages, image.plan.token)
		s.planCharge -= image.plan.charge
		s.stagingCharge -= image.stagingCharge
	}
}

func (s *Service) closePlanImagesLocked() {
	if s.detachedStagingCharge != 0 {
		s.stagingCharge -= s.detachedStagingCharge
		s.detachedStagingCharge = 0
		s.plan.rules = nil
		clear(s.permanentSlots)
	}
	if pending := s.pendingPlan; pending != nil {
		s.pendingPlan = nil
		pending.state = planRetentionReleased
		s.stagingCharge -= pending.stageCharge
		pending.stageCharge = 0
		pending.stagedRules, pending.stagedSlots, pending.stagedWaits = nil, nil, nil
		s.releasePlanLocked(pending.image)
	}
	if active := s.activePlanImage; active != nil {
		s.activePlanImage = nil
		s.releasePlanLocked(active)
	}
}
