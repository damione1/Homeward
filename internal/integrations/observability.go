package integrations

import (
	"context"
	"errors"
	"time"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/observability"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/types/known/emptypb"
)

// AttachObservation is composition-only and cannot replace cumulative cells
// after any driver or command owner has been admitted.
func (h *Manager) AttachObservation(cells *observability.Cells, journal *observability.Journal) error {
	if h == nil || h.actions == nil || journal == nil {
		return ErrInvalidInput
	}
	if _, err := cells.Epoch(); err != nil {
		return ErrInvalidInput
	}
	if _, err := journal.Snapshot(); err != nil {
		return ErrInvalidInput
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.composition != compositionOpen || h.admission != admissionOpen || len(h.active) != 0 || h.supervisor != nil || h.volatile != nil || h.durable != nil {
		return ErrInvalidInput
	}
	h.observation, h.journal = cells, journal
	h.actions.observation = cells
	return nil
}
func (h *Manager) ObservationActions() (*obs.ActionMetrics, error) {
	if h == nil || h.actions == nil || h.observation == nil {
		return nil, ErrInvalidInput
	}
	h.actions.mu.Lock()
	for _, entry := range h.actions.entries {
		h.actions.expire(entry)
	}
	h.actions.mu.Unlock()
	return h.observation.ActionCounters(), nil
}

// Only a completed durable transaction supplies a positive write fact. A
// committed duplicate or CAS refusal still proves that I/O; its business
// outcome remains the command service's concern. Read/replay/device ACKs must
// never call this with TransactionCommitted to repair an unknown commit.
func (h *Manager) observeDurableTransaction(outcome storage.TransactionOutcome, err error) {
	if h == nil {
		return
	}
	if outcome == storage.TransactionNotCommitted {
		var storageErr *storage.Error
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &storageErr) && (storageErr.Kind() == storage.InvalidSettings || storageErr.Kind() == storage.InvalidContext || storageErr.Kind() == storage.CommandRuntimeBusy) {
			// Admission validation, a held runtime guard and cancellation before
			// COMMIT do not establish a database outage. Unknown COMMIT stays below.
			return
		}
	}
	availability := obs.DurableAvailability_DURABLE_AVAILABILITY_UNAVAILABLE
	switch outcome {
	case storage.TransactionCommitted:
		if err == nil {
			availability = obs.DurableAvailability_DURABLE_AVAILABILITY_AVAILABLE
		}
	case storage.TransactionIndeterminate:
		availability = obs.DurableAvailability_DURABLE_AVAILABILITY_DEGRADED
	case storage.TransactionNotCommitted, storage.TransactionOutcomeUnspecified:
	default:
	}
	h.mu.Lock()
	health := h.observationHealth
	h.mu.Unlock()
	if health != nil {
		_ = health.PublishDurableFact(availability, time.Now().UTC())
	}
}
func observationPilotPhase(phase PilotPhase) (obs.PilotPhase, error) {
	switch phase {
	case PilotStarting:
		return obs.PilotPhase_PILOT_PHASE_STARTING, nil
	case PilotRunning:
		return obs.PilotPhase_PILOT_PHASE_RUNNING, nil
	case PilotRetrying:
		return obs.PilotPhase_PILOT_PHASE_RETRYING, nil
	case PilotDegraded:
		return obs.PilotPhase_PILOT_PHASE_DEGRADED, nil
	case PilotStopped:
		return obs.PilotPhase_PILOT_PHASE_STOPPED, nil
	default:
		return 0, ErrInvalidInput
	}
}
func (s *Supervisor) ObservationPhases() ([]*obs.PilotPhaseCount, error) {
	if s == nil || s.pilots == nil {
		return nil, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var counts [6]uint64
	for _, run := range s.pilots {
		phase, err := observationPilotPhase(run.phase)
		if err != nil {
			return nil, err
		}
		counts[phase]++
	}
	rows := make([]*obs.PilotPhaseCount, 0, 5)
	for _, phase := range []obs.PilotPhase{obs.PilotPhase_PILOT_PHASE_STARTING, obs.PilotPhase_PILOT_PHASE_RUNNING, obs.PilotPhase_PILOT_PHASE_RETRYING, obs.PilotPhase_PILOT_PHASE_DEGRADED, obs.PilotPhase_PILOT_PHASE_STOPPED} {
		rows = append(rows, &obs.PilotPhaseCount{Phase: phase, Instances: counts[phase]})
	}
	return rows, nil
}
func (s *Supervisor) ObservationIntegration(instance domain.IntegrationInstanceID) (*obs.IntegrationSupervision, error) {
	if s == nil || s.pilots == nil {
		return nil, ErrInvalidInput
	}
	if _, err := instance.Value(); err != nil {
		return nil, err
	}
	status, present := s.Status(instance)
	if !present {
		return &obs.IntegrationSupervision{Result: &obs.IntegrationSupervision_NotSupervised{NotSupervised: &emptypb.Empty{}}}, nil
	}
	phase, err := observationPilotPhase(status.Phase)
	if err != nil || status.Attempts < 0 || status.Attempts > 17 || status.Tasks < 0 || status.Tasks > 256 {
		return nil, ErrInvalidInput
	}
	return &obs.IntegrationSupervision{Result: &obs.IntegrationSupervision_Supervised{Supervised: &obs.SupervisedIntegration{Phase: phase, Attempts: uint64(status.Attempts), Tasks: uint64(status.Tasks)}}}, nil
}
func (s *Supervisor) emitObservation(code obs.LogCode, level obs.LogLevel) {
	if s.journal == nil {
		return
	}
	event, err := observability.NewEvent(code, level, obs.LogComponent_LOG_COMPONENT_SUPERVISOR, time.Now().UTC(), observability.NewProcessAuthor())
	if err == nil {
		_ = s.journal.Emit(event)
	}
}

// Trusted pilot composition runs once before Start, including RunWithFactory.
// It carries process cells only, never an actor, permission, or dispatch port.
type PilotObservationComposition interface {
	AttachObservation(*observability.Cells, *observability.Journal) error
}

func (h *Manager) ObservationCells() (*observability.Cells, error) {
	if h == nil || h.observation == nil {
		return nil, ErrInvalidInput
	}
	return h.observation, nil
}
func (p *CloudPolicy) AttachObservation(cells *observability.Cells) error {
	if p == nil || p.states == nil {
		return ErrInvalidInput
	}
	if _, err := cells.Epoch(); err != nil {
		return ErrInvalidInput
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.observation == cells {
		return nil
	}
	if p.observation != nil {
		return ErrInvalidInput
	}
	p.observation = cells
	var queued, inflight int64
	for _, state := range p.states {
		queued += int64(state.queued)
		inflight += int64(state.inFlight)
	}
	_ = cells.ChangeRuntimeQueue(obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS, int64(len(p.states)), queued, int64(len(p.states)*p.limits.value.MaxQueued), inflight, 0)
	return nil
}

func (h *Manager) AttachObservationHealth(health *observability.HealthCell) error {
	if h == nil || health == nil {
		return ErrInvalidInput
	}
	facts, err := health.Snapshot()
	if err != nil {
		return err
	}
	epoch, err := facts.Epoch()
	if err != nil {
		return err
	}
	current, err := h.observation.Epoch()
	if err != nil || epoch != current {
		return ErrInvalidInput
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.composition != compositionOpen || h.supervisor != nil {
		return ErrInvalidInput
	}
	h.observationHealth = health
	return nil
}
func (h *Manager) ObservationCoreOpen() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.admission == admissionOpen && h.composition == compositionFrozen && h.catalog.Ready()
}
