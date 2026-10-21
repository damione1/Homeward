package server

import (
	"context"
	"encoding/hex"
	"errors"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/observability"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func observationRecorderEpoch(epoch storage.RecorderEpoch) (*obs.RecorderEpochIdentity, error) {
	text, err := epoch.Hex()
	if err != nil {
		return nil, err
	}
	bytes, err := hex.DecodeString(text)
	if err != nil {
		return nil, err
	}
	return &obs.RecorderEpochIdentity{Value: bytes}, nil
}
func observationCoveragePosition(epoch storage.RecorderEpoch, sequence uint64) (*obs.AdmissionPosition, error) {
	identity, err := observationRecorderEpoch(epoch)
	if err != nil {
		return nil, err
	}
	return &obs.AdmissionPosition{Epoch: identity.Value, Sequence: sequence}, nil
}
func observationGapReason(reason storage.RecorderGapReason) (obs.GapReason, error) {
	switch reason {
	case storage.RecorderGapOverflow:
		return obs.GapReason_GAP_REASON_OVERFLOW, nil
	case storage.RecorderGapReconciliation:
		return obs.GapReason_GAP_REASON_RECONCILIATION, nil
	case storage.RecorderGapAbandoned:
		return obs.GapReason_GAP_REASON_ABANDONED, nil
	default:
		return 0, storage.ErrRecorderCoverageIntegrity
	}
}
func observationCoverage(snapshot storage.RecorderCoverageSnapshot) (*obs.ReadRecorderCoverageResponse, error) {
	if snapshot.Kind() == storage.RecorderCoverageReadTruncated {
		var reason obs.CoverageTruncationReason
		switch snapshot.Truncation() {
		case storage.RecorderCoverageEpochLimit:
			reason = obs.CoverageTruncationReason_COVERAGE_TRUNCATION_REASON_EPOCH_LIMIT
		case storage.RecorderCoverageGapLimit:
			reason = obs.CoverageTruncationReason_COVERAGE_TRUNCATION_REASON_GAP_LIMIT
		case storage.RecorderCoverageByteLimit:
			reason = obs.CoverageTruncationReason_COVERAGE_TRUNCATION_REASON_BYTE_LIMIT
		case storage.RecorderCoverageDurationLimit:
			reason = obs.CoverageTruncationReason_COVERAGE_TRUNCATION_REASON_DURATION_LIMIT
		default:
			return nil, storage.ErrRecorderCoverageIntegrity
		}
		return &obs.ReadRecorderCoverageResponse{Result: &obs.ReadRecorderCoverageResponse_Truncated{Truncated: &obs.TruncatedRecorderCoverage{Reason: reason}}}, nil
	}
	if snapshot.Kind() != storage.RecorderCoverageReadComplete {
		return nil, storage.ErrRecorderCoverageIntegrity
	}
	value := &obs.RecorderCoverageSnapshot{}
	fromEpoch, from, throughEpoch, through := snapshot.Bounds()
	var err error
	value.From, err = observationCoveragePosition(fromEpoch, from)
	if err != nil {
		return nil, err
	}
	value.Through, err = observationCoveragePosition(throughEpoch, through)
	if err != nil {
		return nil, err
	}
	epochs := snapshot.Epochs()
	gaps := snapshot.Gaps()
	if len(epochs) > 64 || len(gaps) > 512 {
		return nil, storage.ErrRecorderCoverageIntegrity
	}
	value.Epochs = make([]*obs.RecorderEpochCoverage, 0, len(epochs))
	value.Gaps = make([]*obs.RecorderCoverageGap, 0, len(gaps))
	for _, epoch := range epochs {
		identity, err := observationRecorderEpoch(epoch.Epoch())
		if err != nil {
			return nil, err
		}
		reconciliation := obs.RecorderReconciliationState_RECORDER_RECONCILIATION_STATE_INCOMPLETE
		if epoch.ReconciliationComplete() {
			reconciliation = obs.RecorderReconciliationState_RECORDER_RECONCILIATION_STATE_COMPLETE
		}
		row := &obs.RecorderEpochCoverage{Epoch: identity, Checkpoint: epoch.Checkpoint(), PolicyExcluded: epoch.PolicyExcluded(), StartedAt: timestamppb.New(epoch.StartedAt()), CleanClose: epoch.CleanClose(), Reconciliation: reconciliation, CloseTime: &obs.RecorderEpochCoverage_NotRecorded{NotRecorded: &emptypb.Empty{}}, Predecessor: &obs.RecorderEpochCoverage_NoPrevious{NoPrevious: &emptypb.Empty{}}}
		if at, present := epoch.ClosedAt(); present {
			row.CloseTime = &obs.RecorderEpochCoverage_ClosedAt{ClosedAt: timestamppb.New(at)}
		}
		if predecessor, checkpoint, clean, present := epoch.Previous(); present {
			identity, err := observationRecorderEpoch(predecessor)
			if err != nil {
				return nil, err
			}
			row.Predecessor = &obs.RecorderEpochCoverage_Previous{Previous: &obs.PreviousRecorderEpoch{Epoch: identity, Checkpoint: checkpoint, CleanClose: clean}}
		}
		value.Epochs = append(value.Epochs, row)
	}
	for _, gap := range gaps {
		identity, err := observationRecorderEpoch(gap.Epoch())
		if err != nil {
			return nil, err
		}
		from, through := gap.Bounds()
		reason, err := observationGapReason(gap.Reason())
		if err != nil {
			return nil, err
		}
		lost := observability.UnknownLost()
		if count, present := gap.KnownLost(); present {
			lost, err = observability.KnownLostLowerBound(count)
			if err != nil {
				return nil, err
			}
		}
		known, err := lost.Wire()
		if err != nil {
			return nil, err
		}
		envelope := obs.GapEnvelope_GAP_ENVELOPE_EXACT_POSITIONS
		if gap.UncertainEnvelope() {
			envelope = obs.GapEnvelope_GAP_ENVELOPE_UNCERTAIN_ENVELOPE
		}
		value.Gaps = append(value.Gaps, &obs.RecorderCoverageGap{Epoch: identity, FromSequence: from, ThroughSequence: through, Reason: reason, KnownLost: known, Envelope: envelope, UtcBounds: &obs.RecorderCoverageGap_UnknownUtc{UnknownUtc: &emptypb.Empty{}}})
	}
	return &obs.ReadRecorderCoverageResponse{Result: &obs.ReadRecorderCoverageResponse_Complete{Complete: value}}, nil
}
func (h *observationHandler) ReadRecorderCoverage(ctx context.Context, r *connect.Request[obs.ReadRecorderCoverageRequest]) (*connect.Response[obs.ReadRecorderCoverageResponse], error) {
	if r == nil || contracts.AdmitObservabilityRequest(r.Msg) != nil {
		return nil, observationRPCError(contracts.ErrInvalidObservabilityContract)
	}
	fromEpoch, err := storage.ParseRecorderEpoch(hex.EncodeToString(r.Msg.From.Epoch))
	if err != nil {
		return nil, observationRPCError(contracts.ErrInvalidObservabilityContract)
	}
	var endEpoch storage.RecorderEpoch
	var end *uint64
	switch through := r.Msg.Through.(type) {
	case *obs.ReadRecorderCoverageRequest_Position:
		endEpoch, err = storage.ParseRecorderEpoch(hex.EncodeToString(through.Position.Epoch))
		sequence := through.Position.Sequence
		end = &sequence
	case *obs.ReadRecorderCoverageRequest_LatestInEpoch:
		endEpoch, err = storage.ParseRecorderEpoch(hex.EncodeToString(through.LatestInEpoch.Value))
	default:
		return nil, observationRPCError(contracts.ErrInvalidObservabilityContract)
	}
	if err != nil {
		return nil, observationRPCError(contracts.ErrInvalidObservabilityContract)
	}
	request, err := storage.NewRecorderCoverageRequest(fromEpoch, r.Msg.From.Sequence, endEpoch, end)
	if err != nil {
		return nil, observationRPCError(contracts.ErrInvalidObservabilityContract)
	}
	var binding observability.ReadBinding
	if err = h.withCurrent(ctx, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, nil, func(value observability.ReadBinding) error { binding = value; return nil }); err != nil {
		return nil, observationRPCError(err)
	}
	snapshot, readErr := h.runtime.store.ReadRecorderCoverage(ctx, request)
	if readErr != nil && !errors.Is(readErr, storage.ErrRecorderCoverageNotFound) {
		return nil, observationRPCError(readErr)
	}
	var response *obs.ReadRecorderCoverageResponse
	err = h.withCurrent(ctx, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, &binding, func(observability.ReadBinding) error {
		if errors.Is(readErr, storage.ErrRecorderCoverageNotFound) {
			response = &obs.ReadRecorderCoverageResponse{Result: &obs.ReadRecorderCoverageResponse_NotRetained{NotRetained: &emptypb.Empty{}}}
			return contracts.AdmitObservabilityResponse(response)
		}
		var err error
		response, err = observationCoverage(snapshot)
		if err != nil {
			return err
		}
		return contracts.AdmitObservabilityResponse(response)
	})
	if err != nil {
		return nil, observationRPCError(err)
	}
	return observationResponse(response)
}
