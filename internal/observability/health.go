package observability

import (
	"sync"
	"time"

	"github.com/damione1/Homeward/contracts"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type HealthCell struct {
	mu      sync.Mutex
	epoch   ProcessObservationEpoch
	server  *obs.ServerHealth
	durable *obs.DurableHealth
}

func NewHealthCell(epoch ProcessObservationEpoch) (*HealthCell, error) {
	if !epoch.valid() {
		return nil, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_PROCESS_EPOCH)
	}
	return &HealthCell{epoch: epoch, server: &obs.ServerHealth{Readiness: obs.ServerReadiness_SERVER_READINESS_STARTING, Generation: &obs.GenerationPresence{Result: &obs.GenerationPresence_NotLoaded{NotLoaded: &emptypb.Empty{}}}, Causes: []obs.CoreCause{obs.CoreCause_CORE_CAUSE_GENERATION_NOT_LOADED}}, durable: &obs.DurableHealth{Availability: obs.DurableAvailability_DURABLE_AVAILABILITY_UNKNOWN, Evidence: &obs.DurableHealth_NotObserved{NotObserved: &emptypb.Empty{}}}}, nil
}
func (h *HealthCell) valid() bool { return h != nil && h.epoch.valid() }
func (h *HealthCell) PublishStarting() error {
	if !h.valid() {
		return invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_HEALTH)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.server.Readiness != obs.ServerReadiness_SERVER_READINESS_STARTING {
		return invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_HEALTH)
	}
	return nil
}

// The owner must call this only after actual active generation publication,
// opened core owners/projection/admissions, and the ready public listener.
func (h *HealthCell) PublishReady(revision ActiveRevision) error {
	if !h.valid() || revision.value == 0 {
		return invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_REVISION)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.server.Readiness == obs.ServerReadiness_SERVER_READINESS_DRAINING {
		return failure(obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_CLOSED, obs.ObservabilityField_OBSERVABILITY_FIELD_HEALTH)
	}
	h.server = &obs.ServerHealth{Readiness: obs.ServerReadiness_SERVER_READINESS_READY, Generation: &obs.GenerationPresence{Result: &obs.GenerationPresence_Present{Present: &obs.ActiveRevision{Value: revision.value}}}}
	return nil
}
func (h *HealthCell) PublishDraining() error {
	if !h.valid() {
		return invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_HEALTH)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.server.Readiness = obs.ServerReadiness_SERVER_READINESS_DRAINING
	h.server.Causes = []obs.CoreCause{obs.CoreCause_CORE_CAUSE_SHUTTING_DOWN}
	return nil
}
func (h *HealthCell) PublishCoreUnavailable(cause obs.CoreCause) error {
	if !h.valid() || cause == 0 || obs.CoreCause_name[int32(cause)] == "" {
		return invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_HEALTH)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.server.Readiness == obs.ServerReadiness_SERVER_READINESS_DRAINING {
		return failure(obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_CLOSED, obs.ObservabilityField_OBSERVABILITY_FIELD_HEALTH)
	}
	h.server.Readiness = obs.ServerReadiness_SERVER_READINESS_CORE_UNAVAILABLE
	h.server.Causes = []obs.CoreCause{cause}
	return nil
}
func (h *HealthCell) PublishDurableFact(availability obs.DurableAvailability, at time.Time) error {
	if !h.valid() || at.IsZero() || timestamppb.New(at).CheckValid() != nil {
		return invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_HEALTH)
	}
	switch availability {
	case obs.DurableAvailability_DURABLE_AVAILABILITY_AVAILABLE, obs.DurableAvailability_DURABLE_AVAILABILITY_DEGRADED, obs.DurableAvailability_DURABLE_AVAILABILITY_UNAVAILABLE, obs.DurableAvailability_DURABLE_AVAILABILITY_NOT_CONFIGURED:
	default:
		return invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_HEALTH)
	}
	wire := &obs.DurableHealth{Availability: availability, Evidence: &obs.DurableHealth_ObservedAt{ObservedAt: timestamppb.New(at.UTC())}}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.durable = wire
	return nil
}

// HealthFacts contains only admitted facts owned by this cell. The server
// assembles these with actual Recorder and Supervisor facts before admitting
// a complete HealthSnapshot. No partially populated HealthSnapshot is exposed.
type HealthFacts struct {
	epoch   ProcessObservationEpoch
	server  *obs.ServerHealth
	durable *obs.DurableHealth
}

func (f HealthFacts) valid() bool {
	return f.epoch.valid() && f.server != nil && f.durable != nil &&
		contracts.AdmitObservabilityValue(f.server) == nil &&
		contracts.AdmitObservabilityValue(f.durable) == nil
}
func (f HealthFacts) Epoch() (ProcessObservationEpoch, error) {
	if !f.valid() {
		return ProcessObservationEpoch{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_HEALTH)
	}
	return f.epoch, nil
}
func (f HealthFacts) Server() (*obs.ServerHealth, error) {
	if !f.valid() {
		return nil, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_HEALTH)
	}
	return proto.Clone(f.server).(*obs.ServerHealth), nil
}
func (f HealthFacts) Durable() (*obs.DurableHealth, error) {
	if !f.valid() {
		return nil, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_HEALTH)
	}
	return proto.Clone(f.durable).(*obs.DurableHealth), nil
}

func (h *HealthCell) Snapshot() (HealthFacts, error) {
	if !h.valid() {
		return HealthFacts{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_HEALTH)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	facts := HealthFacts{epoch: h.epoch, server: h.server, durable: h.durable}
	if !facts.valid() {
		return HealthFacts{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_HEALTH)
	}
	facts.server = proto.Clone(h.server).(*obs.ServerHealth)
	facts.durable = proto.Clone(h.durable).(*obs.DurableHealth)
	return facts, nil
}
