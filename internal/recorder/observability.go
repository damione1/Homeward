package recorder

import (
	"encoding/hex"
	"time"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/observability"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (r *Recorder) observeConfirmedBatch(batch storage.RecorderBatch) {
	gaps, err := batch.GapSummary()
	if err != nil {
		return
	}
	for _, gap := range gaps {
		var reason obs.GapReason
		switch gap.Reason {
		case storage.RecorderGapOverflow:
			reason = obs.GapReason_GAP_REASON_OVERFLOW
		case storage.RecorderGapReconciliation:
			reason = obs.GapReason_GAP_REASON_RECONCILIATION
		case storage.RecorderGapAbandoned:
			reason = obs.GapReason_GAP_REASON_ABANDONED
		default:
			continue
		}
		lost := observability.UnknownLost()
		if gap.KnownLost != nil {
			measured, err := observability.KnownLostLowerBound(*gap.KnownLost)
			if err != nil {
				continue
			}
			lost = measured
		}
		_ = r.observation.AddCommittedGap(reason, lost)
	}
	if r.observationHealth != nil {
		_ = r.observationHealth.PublishDurableFact(obs.DurableAvailability_DURABLE_AVAILABILITY_AVAILABLE, time.Now().UTC())
	}
}
func recorderObservationPhase(phase Phase) (obs.RecorderPhase, error) {
	switch phase {
	case PhaseRunning:
		return obs.RecorderPhase_RECORDER_PHASE_RUNNING, nil
	case PhaseWriting:
		return obs.RecorderPhase_RECORDER_PHASE_WRITING, nil
	case PhaseBackoff:
		return obs.RecorderPhase_RECORDER_PHASE_BACKOFF, nil
	case PhaseStopping:
		return obs.RecorderPhase_RECORDER_PHASE_STOPPING, nil
	case PhaseFailed:
		return obs.RecorderPhase_RECORDER_PHASE_FAILED, nil
	case PhaseStopped:
		return obs.RecorderPhase_RECORDER_PHASE_STOPPED, nil
	default:
		return 0, ErrInvalidRecorder
	}
}
func recorderPosition(epoch string, sequence uint64) (*obs.AdmissionPosition, error) {
	if len(epoch) != 32 {
		return nil, ErrInvalidRecorder
	}
	bytes, err := hex.DecodeString(epoch)
	if err != nil {
		return nil, ErrInvalidRecorder
	}
	return &obs.AdmissionPosition{Epoch: bytes, Sequence: sequence}, nil
}
func recorderGauge(value int) *obs.GaugeMeasurement {
	return &obs.GaugeMeasurement{Result: &obs.GaugeMeasurement_Value{Value: uint64(value)}}
}
func (r *Recorder) ObservationHealth() (*obs.RecorderHealth, error) {
	status, err := r.Status()
	if err != nil {
		return nil, err
	}
	phase, err := recorderObservationPhase(status.Phase)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	at := r.observationAt
	r.mu.Unlock()
	if at.IsZero() {
		return nil, ErrInvalidRecorder
	}
	state := obs.RecorderHealthState_RECORDER_HEALTH_STATE_OPERATING
	if status.HistoryDegraded || status.Phase == PhaseBackoff || status.Phase == PhaseStopping || status.Phase == PhaseStopped {
		state = obs.RecorderHealthState_RECORDER_HEALTH_STATE_DEGRADED
	}
	if status.Phase == PhaseFailed {
		state = obs.RecorderHealthState_RECORDER_HEALTH_STATE_FAILED
	}
	return &obs.RecorderHealth{Result: &obs.RecorderHealth_Observed{Observed: &obs.RecorderHealthFact{Phase: phase, State: state, ObservedAt: timestamppb.New(at)}}}, nil
}
func (r *Recorder) ObservationMetrics() (*obs.RecorderMetrics, error) {
	status, err := r.Status()
	if err != nil {
		return nil, err
	}
	if status.QueuedBytes < 0 || status.PendingBytes < 0 || status.TotalCapacityBytes < 0 || status.QueueCapacityBytes < 0 || status.QueuedEntries < 0 {
		return nil, ErrInvalidRecorder
	}
	checkpoint, err := recorderPosition(status.Epoch, status.Checkpoint)
	if err != nil {
		return nil, err
	}
	// The real checkpoint snapshot is taken before the source tail. Positions
	// are subtracted only after proving the same admitted source epoch.
	cursor, err := r.sequence.RecorderCursor()
	if err != nil {
		return nil, err
	}
	tailEpoch, tailSequence, err := cursor.Components()
	if err != nil {
		return nil, err
	}
	tail, err := recorderPosition(tailEpoch, tailSequence)
	if err != nil {
		return nil, err
	}
	lag := observability.EpochMismatchLag()
	if tailEpoch == status.Epoch {
		if tailSequence < status.Checkpoint {
			return nil, ErrInvalidRecorder
		}
		lag = observability.SameEpochLag(tailSequence - status.Checkpoint)
	}
	retries, lost, excluded := r.observation.RecorderCounters()
	gaps, err := r.observation.GapCounters()
	if err != nil {
		return nil, err
	}
	active := &obs.ActiveCoverage{Result: &obs.ActiveCoverage_NoActiveRisk{NoActiveRisk: &emptypb.Empty{}}}
	if epoch, from, through, present := status.ActiveCoverage.Bounds(); present {
		text, err := epoch.Hex()
		if err != nil {
			return nil, err
		}
		first, err := recorderPosition(text, from)
		if err != nil {
			return nil, err
		}
		last, err := recorderPosition(text, through)
		if err != nil {
			return nil, err
		}
		known := observability.UnknownLost()
		if count, present := status.ActiveCoverage.KnownLost(); present {
			known, err = observability.KnownLostLowerBound(count)
			if err != nil {
				return nil, err
			}
		}
		measured, err := known.Wire()
		if err != nil {
			return nil, err
		}
		active.Result = &obs.ActiveCoverage_AtRisk{AtRisk: &obs.ActiveCoverageInterval{From: first, Through: last, KnownLost: measured}}
	}
	value := &obs.InstalledRecorderMetrics{QueuedBytes: recorderGauge(status.QueuedBytes), PendingBytes: recorderGauge(status.PendingBytes), TotalCapacityBytes: recorderGauge(status.TotalCapacityBytes), QueueCapacityBytes: recorderGauge(status.QueueCapacityBytes), QueuedEntries: recorderGauge(status.QueuedEntries), RetriesTotal: retries, LostEntriesTotal: lost, PolicyExcludedTotal: excluded, Checkpoint: &obs.PositionMeasurement{Result: &obs.PositionMeasurement_Value{Value: checkpoint}}, Tail: &obs.PositionMeasurement{Result: &obs.PositionMeasurement_Value{Value: tail}}, RecorderLagPositions: lag, CommittedGaps: gaps, ActiveCoverage: active}
	return &obs.RecorderMetrics{Result: &obs.RecorderMetrics_Installed{Installed: value}}, nil
}
