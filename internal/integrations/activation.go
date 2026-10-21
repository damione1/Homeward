package integrations

import (
	"context"
	"github.com/damione1/Homeward/internal/domain"
)

// Only the Run composition with a real configuration-activation owner keeps
// future replacement admissions alive. Standalone supervisors retain their
// natural Done when their existing pilots finish.
type replacementAdmission uint8

const (
	replacementAdmissionAbsent replacementAdmission = iota
	replacementAdmissionPrepared
	replacementAdmissionHeld
	replacementAdmissionReleased
)

// PrepareReplacements binds future admissions before Start can launch pilots
// or begin waiting for them. It adds no worker and invokes no pilot factory.
func (s *Supervisor) PrepareReplacements() error {
	if s == nil {
		return ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != SupervisorReady || s.replacementAdmission != replacementAdmissionAbsent {
		return ErrSupervisorState
	}
	s.replacementAdmission = replacementAdmissionPrepared
	return nil
}

type pilotReplacementPhase uint8

const (
	pilotReplacementInvalid pilotReplacementPhase = iota
	pilotReplacementPrepared
	pilotReplacementJoining
	pilotReplacementJoined
	pilotReplacementPublished
	pilotReplacementReleased
)

// PilotReplacement owns only explicitly changed instances. Reservation never
// invokes a factory. Quiesce is post-COMMIT and joins the actual former tasks
// before Publish may invoke a replacement factory on this same Supervisor.
type PilotReplacement struct {
	supervisor *Supervisor
	phase      pilotReplacementPhase
	old        []*pilotRun
	next       []PilotSpec
	remove     []domain.IntegrationInstanceID
}

func (s *Supervisor) startPilotLocked(run *pilotRun) {
	run.ctx, run.cancel = context.WithCancel(s.ctx)
	run.done = make(chan struct{})
	s.wg.Add(1)
	go func() { defer s.wg.Done(); defer close(run.done); s.run(run) }()
}

func (s *Supervisor) ReserveReplacement(next []PilotSpec, remove []domain.IntegrationInstanceID) (*PilotReplacement, error) {
	if s == nil || len(next)+len(remove) > 1024 {
		return nil, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != SupervisorRunning || s.replacementAdmission != replacementAdmissionHeld || s.pendingReplacement != nil {
		return nil, ErrSupervisorState
	}
	seen := make(map[domain.IntegrationInstanceID]bool, len(next)+len(remove))
	r := &PilotReplacement{supervisor: s, phase: pilotReplacementPrepared,
		next: append([]PilotSpec(nil), next...), remove: append([]domain.IntegrationInstanceID(nil), remove...)}
	count := len(s.pilots)
	for _, spec := range next {
		if _, err := spec.Instance(); err != nil || seen[spec.instance] {
			return nil, ErrInvalidInput
		}
		seen[spec.instance] = true
		if old := s.pilots[spec.instance]; old != nil {
			r.old = append(r.old, old)
		} else {
			count++
		}
	}
	for _, id := range remove {
		if _, err := id.Value(); err != nil || seen[id] || s.pilots[id] == nil {
			return nil, ErrInvalidInput
		}
		seen[id] = true
		r.old = append(r.old, s.pilots[id])
		count--
	}
	if count > 1024 {
		return nil, ErrInvalidInput
	}
	s.pendingReplacement = r
	return r, nil
}

func (r *PilotReplacement) Release() error {
	if r == nil || r.supervisor == nil {
		return ErrInvalidInput
	}
	s := r.supervisor
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.phase != pilotReplacementPrepared || s.pendingReplacement != r {
		return ErrSupervisorState
	}
	r.phase, s.pendingReplacement = pilotReplacementReleased, nil
	return nil
}

func (r *PilotReplacement) Quiesce(ctx context.Context) error {
	if r == nil || r.supervisor == nil || nilInterface(ctx) {
		return ErrInvalidInput
	}
	s := r.supervisor
	s.mu.Lock()
	if s.phase != SupervisorRunning || s.pendingReplacement != r ||
		(r.phase != pilotReplacementPrepared && r.phase != pilotReplacementJoining && r.phase != pilotReplacementJoined) {
		s.mu.Unlock()
		return ErrSupervisorState
	}
	if r.phase == pilotReplacementJoined {
		s.mu.Unlock()
		return nil
	}
	r.phase = pilotReplacementJoining
	s.mu.Unlock()
	for _, run := range r.old {
		run.cancel()
	}
	for _, run := range r.old {
		select {
		case <-run.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingReplacement != r || s.phase != SupervisorRunning {
		return ErrSupervisorState
	}
	r.phase = pilotReplacementJoined
	return nil
}

func (r *PilotReplacement) Publish() error {
	if r == nil || r.supervisor == nil {
		return ErrInvalidInput
	}
	s := r.supervisor
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.phase != pilotReplacementJoined || s.pendingReplacement != r || s.phase != SupervisorRunning || s.replacementAdmission != replacementAdmissionHeld {
		return ErrSupervisorState
	}
	for _, id := range r.remove {
		delete(s.pilots, id)
	}
	for _, spec := range r.next {
		run := &pilotRun{spec: spec, phase: PilotStopped}
		s.pilots[spec.instance] = run
		s.startPilotLocked(run)
	}
	r.phase, s.pendingReplacement = pilotReplacementPublished, nil
	return nil
}

func (r *PilotReplacement) Supersede(ctx context.Context) ([]domain.IntegrationInstanceID, error) {
	if r == nil || r.supervisor == nil || nilInterface(ctx) {
		return nil, ErrInvalidInput
	}
	s := r.supervisor
	s.mu.Lock()
	phase := r.phase
	s.mu.Unlock()
	if phase == pilotReplacementPrepared {
		return nil, r.Release()
	}
	if phase == pilotReplacementPublished || phase == pilotReplacementReleased {
		return nil, nil
	}
	if phase != pilotReplacementJoining && phase != pilotReplacementJoined {
		return nil, ErrSupervisorState
	}
	if err := r.Quiesce(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingReplacement != r {
		return nil, ErrSupervisorState
	}
	forced := make([]domain.IntegrationInstanceID, 0, len(r.old)+len(r.next))
	for _, run := range r.old {
		forced = append(forced, run.spec.instance)
	}
	for _, spec := range r.next {
		forced = append(forced, spec.instance)
	}
	r.phase, s.pendingReplacement = pilotReplacementReleased, nil
	return forced, nil
}
