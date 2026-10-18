package hilo

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/energy"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/sequencer"
)

var ErrLeaseEvidence = errors.New("hilo: invalid lease evidence")

type leaseEvidence struct {
	generation uint64
	revision   uint64
	source     domain.SourceStamp
}

// LeaseRegistry is the narrow composition adapter between Hilo's source
// continuity and NHA-108. The pilot never receives the Sequencer. A report is
// provable only after Host publication and while both streams stay in the
// same snapshot generation. Ping and timer ticks cannot add evidence.
type LeaseRegistry struct {
	mu         sync.Mutex
	sequence   *sequencer.Sequencer
	instance   domain.IntegrationInstanceID
	window     time.Duration
	generation uint64
	active     bool
	reports    map[domain.EntityID]leaseEvidence
}

func NewLeaseRegistry(sequence *sequencer.Sequencer, instance domain.IntegrationInstanceID, window time.Duration) (*LeaseRegistry, error) {
	if sequence == nil || window < 120*time.Second || window > time.Hour {
		return nil, ErrLeaseEvidence
	}
	if _, err := instance.Value(); err != nil {
		return nil, ErrLeaseEvidence
	}
	return &LeaseRegistry{sequence: sequence, instance: instance, window: window, reports: make(map[domain.EntityID]leaseEvidence)}, nil
}

// Revoke is the loss fence. Call it before publishing unavailable or beginning
// a new snapshot, including on queue overflow and any reconnect.
func (r *LeaseRegistry) Revoke() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.revokeLocked()
	r.mu.Unlock()
}

// RevokeGeneration prevents a delayed reader from retiring a newer cycle.
// Loss is ordered with PublishPower under the same registry lock.
func (r *LeaseRegistry) RevokeGeneration(generation uint64) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active || r.generation != generation {
		return false
	}
	r.revokeLocked()
	return true
}

func (r *LeaseRegistry) revokeLocked() {
	r.active = false
	r.generation++
	clear(r.reports)
}

// Activate follows a complete DeviceHub inventory and GraphQL snapshot with
// both streams alive. It does not attest any individual power report.
func (r *LeaseRegistry) Activate() (uint64, error) {
	if r == nil {
		return 0, ErrLeaseEvidence
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.generation++
	clear(r.reports)
	r.active = true
	return r.generation, nil
}

func (r *LeaseRegistry) recordPowerLocked(entity domain.EntityID, generation uint64) error {
	state, found, err := r.sequence.State(entity)
	if err != nil || !found {
		return ErrLeaseEvidence
	}
	_, _, _, source, revision, err := state.Components()
	if err != nil || revision == 0 {
		return ErrLeaseEvidence
	}
	r.reports[entity] = leaseEvidence{generation, revision, source}
	return nil
}

// PublishPower linearizes Host admission with evidence registration. A normal
// new report keeps the old lease until Host has admitted the new revision;
// Recorder therefore cannot mistake the small post-Publish window for loss.
func (r *LeaseRegistry) PublishPower(ctx context.Context, host integrations.SessionHost, binding integrations.Binding, observation integrations.StateObservation, entity domain.EntityID, generation uint64) error {
	if r == nil || nilInterface(ctx) || nilInterface(host) {
		return ErrLeaseEvidence
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active || generation != r.generation {
		return ErrLeaseEvidence
	}
	if err := host.Publish(ctx, binding, observation); err != nil {
		// Admission may already have happened even when its acknowledgement was
		// lost. Keep neither the old report nor this unproven one renewable.
		r.revokeLocked()
		return err
	}
	if err := r.recordPowerLocked(entity, generation); err != nil {
		r.revokeLocked()
		return err
	}
	return nil
}

func (r *LeaseRegistry) Forget(entity domain.EntityID) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.reports, entity)
	r.mu.Unlock()
}

// Lease checks the exact report, not merely the entity or a recent ping. The
// monotone ticks share Sequencer's clock and are never compared with wall time.
func (r *LeaseRegistry) Lease(_ context.Context, profile energy.Profile, report energy.ReportIdentity, reportAdmissionTick, checkedAtTick time.Duration) (energy.Lease, bool, error) {
	if r == nil || !profile.Valid() || checkedAtTick < reportAdmissionTick || reportAdmissionTick < 0 {
		return energy.Lease{}, false, ErrLeaseEvidence
	}
	entity, instance, source, err := report.Source()
	if err != nil || entity != profile.Source() || instance != r.instance {
		return energy.Lease{}, false, ErrLeaseEvidence
	}
	revision, err := report.StateRevision()
	if err != nil {
		return energy.Lease{}, false, ErrLeaseEvidence
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	evidence, ok := r.reports[entity]
	if !r.active || !ok || evidence.generation != r.generation || evidence.revision != revision || evidence.source != source {
		return energy.Lease{}, false, nil
	}
	state, found, err := r.sequence.State(entity)
	if err != nil {
		return energy.Lease{}, false, err
	}
	if !found {
		return energy.Lease{}, false, nil
	}
	_, _, _, currentSource, currentRevision, err := state.Components()
	if err != nil || currentRevision != revision || currentSource != source {
		return energy.Lease{}, false, nil
	}
	if reportAdmissionTick > time.Duration(1<<63-1)-r.window {
		return energy.Lease{}, false, ErrLeaseEvidence
	}
	until := reportAdmissionTick + r.window
	if checkedAtTick >= until {
		return energy.Lease{}, false, nil
	}
	lease, err := energy.NewLease(report, reportAdmissionTick, until)
	if err != nil {
		return energy.Lease{}, false, err
	}
	return lease, true, nil
}
