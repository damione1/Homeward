package reactivetemplate

import (
	"context"

	"github.com/damione1/Homeward/internal/domain"
)

// ChangedGroupAliases closes technical binding changes over the direct group
// members used by this same engine. It does not admit nested expand support.
func (s *Service) ChangedGroupAliases(changed map[domain.HAAlias]bool) []domain.HAAlias {
	if s == nil || len(changed) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var affected []domain.HAAlias
	for _, group := range s.plan.groups {
		for _, member := range group.Members() {
			if changed[member] {
				affected = append(affected, group.ID())
				break
			}
		}
	}
	return affected
}

func (s *Service) Publication() uint64 {
	if s == nil {
		return 0
	}
	return s.publicationGeneration.Load()
}

func (s *Service) PermanentPlan() (Plan, error) {
	if s == nil {
		return Plan{}, ErrInvalidPlan
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.permanentPrograms > len(s.plan.definitions) {
		return Plan{}, ErrClosed
	}
	p, err := NewPlan(s.plan.definitions[:s.permanentPrograms], s.plan.groups, s.plan.zone)
	if err != nil {
		return Plan{}, err
	}
	p.waitsEnabled = s.plan.waitsEnabled
	return p, nil
}

type planReservationState uint8

const (
	planReservationInvalid planReservationState = iota
	planReservationPrepared
	planReservationPublishing
	planReservationPublished
	planReservationReleased
)

// PlanReservation reserves the candidate permanent programs in the existing
// render engine while transient 044 wait programs keep their owned slots.
type PlanReservation struct {
	service *Service
	plan    Plan
	state   planReservationState
}

func (s *Service) ReservePlan(plan Plan) (*PlanReservation, error) {
	if s == nil || plan.zone == nil || len(plan.definitions) > MaxPrograms {
		return nil, ErrInvalidPlan
	}
	owned, err := NewPlan(plan.definitions, plan.groups, plan.zone)
	if err != nil {
		return nil, err
	}
	owned.waitsEnabled = plan.waitsEnabled
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return nil, ErrClosed
	}
	if s.reservedPlan != nil || len(owned.definitions)+s.controlSlots > MaxPrograms {
		return nil, ErrWorkBudget
	}
	r := &PlanReservation{service: s, plan: owned, state: planReservationPrepared}
	s.reservedPlan = r
	return r, nil
}

func (r *PlanReservation) Release() error {
	if r == nil || r.service == nil {
		return ErrInvalidPlan
	}
	s := r.service
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.state != planReservationPrepared || s.reservedPlan != r {
		return ErrInvalidPlan
	}
	r.state = planReservationReleased
	s.reservedPlan = nil
	return nil
}

// Publish is serialized through the existing engine. Once admitted to its
// queue it waits for the actual result even if the caller's context expires.
// The activation owner calls it outside SQL and the common publication mutex.
func (r *PlanReservation) Publish(ctx context.Context) error {
	if r == nil || r.service == nil || nilReactiveValue(ctx) {
		return ErrInvalidPlan
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s := r.service
	s.mu.Lock()
	if r.state != planReservationPrepared || s.reservedPlan != r || s.closed {
		s.mu.Unlock()
		return ErrInvalidPlan
	}
	if len(r.plan.definitions)+s.controlSlots > MaxPrograms {
		s.mu.Unlock()
		return ErrWorkBudget
	}
	r.state = planReservationPublishing
	s.mu.Unlock()
	request := planReplacement{plan: r.plan, result: make(chan planReplacementResult, 1)}
	select {
	case s.planReplacements <- request:
	case <-ctx.Done():
		s.mu.Lock()
		r.state = planReservationPrepared
		s.mu.Unlock()
		return ctx.Err()
	case <-s.done:
		s.mu.Lock()
		r.state = planReservationPrepared
		s.mu.Unlock()
		return ErrClosed
	}
	outcome := <-request.result
	s.mu.Lock()
	if outcome.applied && r.state == planReservationPublishing {
		r.state = planReservationPublished
		s.reservedPlan = nil
	} else if r.state == planReservationPublishing {
		r.state = planReservationPrepared
	}
	s.mu.Unlock()
	return outcome.err
}
