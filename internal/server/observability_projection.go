package server

import (
	"context"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	native "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"math"
)

// observationProjection admits a closed native snapshot before copying its
// fields. Names are resolved from native owners; no public ID is reverse-hashed.
// An error invalidates the entire projection, never a partially named response.
type observationProjection struct {
	ctx        context.Context
	names      administrationNames
	home       apitypes.HomeName
	controller apitypes.RuntimeControllerName
	err        error
}

func (p *observationProjection) account(value string) string {
	if p.err != nil {
		return ""
	}
	id, err := contracts.ParseAccountID(value)
	if err != nil {
		p.err = storage.ErrAPIResourceIntegrity
		return ""
	}
	name, err := (authResourceNames{p.names}).account(p.ctx, id)
	if err != nil {
		p.err = err
		return ""
	}
	return name.String()
}
func (p *observationProjection) controllerName(value *native.ControllerHandle) string {
	if value == nil || len(value.Value) != 32 {
		p.err = storage.ErrAPIResourceIntegrity
		return ""
	}
	var id [32]byte
	copy(id[:], value.Value)
	name, err := apitypes.RuntimeControllerNameForIdentity(p.home, id)
	if err != nil {
		p.err = err
		return ""
	}
	return name.String()
}
func (p *observationProjection) runName(value uint64) string {
	name, err := apitypes.RuntimeRunNameForIdentity(p.controller, value)
	if err != nil {
		p.err = err
		return ""
	}
	return name.String()
}

func (p *observationProjection) projectProcessObservationEpoch(value *native.ProcessObservationEpoch) *api.ObservationProcessObservationEpoch {
	if value == nil {
		return nil
	}
	result := &api.ObservationProcessObservationEpoch{}
	result.Value = append([]byte(nil), value.Value...)
	return result
}

func (p *observationProjection) projectAdmissionPosition(value *native.AdmissionPosition) *api.AdmissionPosition {
	if value == nil {
		return nil
	}
	result := &api.AdmissionPosition{}
	result.Epoch = append([]byte(nil), value.Epoch...)
	result.Sequence = apitypes.DecimalUint64FromNative(value.Sequence).String()
	return result
}

func (p *observationProjection) projectReadUnavailable(value *native.ReadUnavailable) *api.ObservationReadUnavailable {
	if value == nil {
		return nil
	}
	result := &api.ObservationReadUnavailable{}
	result.Reason = api.ObservationReadUnavailableReason(value.Reason)
	return result
}

func (p *observationProjection) projectActiveRevision(value *native.ActiveRevision) *api.ObservationActiveRevision {
	if value == nil {
		return nil
	}
	result := &api.ObservationActiveRevision{}
	result.Value = p.signed(value.Value)
	return result
}

func (p *observationProjection) projectGenerationPresence(value *native.GenerationPresence) *api.ObservationGenerationPresence {
	if value == nil {
		return nil
	}
	result := &api.ObservationGenerationPresence{}
	switch v := value.Result.(type) {
	case *native.GenerationPresence_Present:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationGenerationPresence_Present{Present: p.projectActiveRevision(v.Present)}
	case *native.GenerationPresence_NotLoaded:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationGenerationPresence_NotLoaded{NotLoaded: &emptypb.Empty{}}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectDurableHealth(value *native.DurableHealth) *api.ObservationDurableHealth {
	if value == nil {
		return nil
	}
	result := &api.ObservationDurableHealth{}
	result.Availability = api.ObservationDurableAvailability(value.Availability)
	switch v := value.Evidence.(type) {
	case *native.DurableHealth_ObservedAt:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Evidence = &api.ObservationDurableHealth_ObservedAt{ObservedAt: proto.Clone(v.ObservedAt).(*timestamppb.Timestamp)}
	case *native.DurableHealth_NotObserved:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Evidence = &api.ObservationDurableHealth_NotObserved{NotObserved: &emptypb.Empty{}}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectPilotPhaseCount(value *native.PilotPhaseCount) *api.ObservationPilotPhaseCount {
	if value == nil {
		return nil
	}
	result := &api.ObservationPilotPhaseCount{}
	result.Phase = api.ObservationPilotPhase(value.Phase)
	result.Instances = p.signed(value.Instances)
	return result
}

func (p *observationProjection) projectRecorderHealthFact(value *native.RecorderHealthFact) *api.ObservationRecorderHealthFact {
	if value == nil {
		return nil
	}
	result := &api.ObservationRecorderHealthFact{}
	result.Phase = api.ObservationRecorderPhase(value.Phase)
	result.State = api.ObservationRecorderHealthState(value.State)
	if value.ObservedAt != nil {
		result.ObservedAt = proto.Clone(value.ObservedAt).(*timestamppb.Timestamp)
	}
	return result
}

func (p *observationProjection) projectRecorderHealth(value *native.RecorderHealth) *api.ObservationRecorderHealth {
	if value == nil {
		return nil
	}
	result := &api.ObservationRecorderHealth{}
	switch v := value.Result.(type) {
	case *native.RecorderHealth_Observed:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationRecorderHealth_Observed{Observed: p.projectRecorderHealthFact(v.Observed)}
	case *native.RecorderHealth_NotInstalled:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationRecorderHealth_NotInstalled{NotInstalled: &emptypb.Empty{}}
	case *native.RecorderHealth_Unavailable:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationRecorderHealth_Unavailable{Unavailable: p.projectReadUnavailable(v.Unavailable)}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectServerHealth(value *native.ServerHealth) *api.ObservationServerHealth {
	if value == nil {
		return nil
	}
	result := &api.ObservationServerHealth{}
	result.Readiness = api.ObservationServerReadiness(value.Readiness)
	result.Generation = p.projectGenerationPresence(value.Generation)
	for _, item := range value.Causes {
		result.Causes = append(result.Causes, api.ObservationCoreCause(item))
	}
	return result
}

func (p *observationProjection) projectHealthSnapshot(value *native.HealthSnapshot) *api.Health {
	if value == nil {
		return nil
	}
	result := &api.Health{}
	result.ProcessEpoch = p.projectProcessObservationEpoch(value.ProcessEpoch)
	result.Liveness = api.ObservationLiveness(value.Liveness)
	result.Server = p.projectServerHealth(value.Server)
	result.Durable = p.projectDurableHealth(value.Durable)
	result.Recorder = p.projectRecorderHealth(value.Recorder)
	for _, item := range value.Pilots {
		result.Pilots = append(result.Pilots, p.projectPilotPhaseCount(item))
	}
	return result
}

func (p *observationProjection) projectIntegrationReport(value *native.IntegrationReport) *api.ObservationIntegrationReport {
	if value == nil {
		return nil
	}
	result := &api.ObservationIntegrationReport{}
	result.Status = api.ObservationIntegrationHealthStatus(value.Status)
	switch v := value.AdmissionTime.(type) {
	case *native.IntegrationReport_AdmittedAt:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.AdmissionTime = &api.ObservationIntegrationReport_AdmittedAt{AdmittedAt: proto.Clone(v.AdmittedAt).(*timestamppb.Timestamp)}
	case *native.IntegrationReport_NotRecorded:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.AdmissionTime = &api.ObservationIntegrationReport_NotRecorded{NotRecorded: &emptypb.Empty{}}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectIntegrationReportPresence(value *native.IntegrationReportPresence) *api.ObservationIntegrationReportPresence {
	if value == nil {
		return nil
	}
	result := &api.ObservationIntegrationReportPresence{}
	switch v := value.Result.(type) {
	case *native.IntegrationReportPresence_Present:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationIntegrationReportPresence_Present{Present: p.projectIntegrationReport(v.Present)}
	case *native.IntegrationReportPresence_NoReport:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationIntegrationReportPresence_NoReport{NoReport: &emptypb.Empty{}}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectSupervisedIntegration(value *native.SupervisedIntegration) *api.ObservationSupervisedIntegration {
	if value == nil {
		return nil
	}
	result := &api.ObservationSupervisedIntegration{}
	result.Phase = api.ObservationPilotPhase(value.Phase)
	result.Attempts = p.signed(value.Attempts)
	result.Tasks = p.signed(value.Tasks)
	return result
}

func (p *observationProjection) projectIntegrationSupervision(value *native.IntegrationSupervision) *api.ObservationIntegrationSupervision {
	if value == nil {
		return nil
	}
	result := &api.ObservationIntegrationSupervision{}
	switch v := value.Result.(type) {
	case *native.IntegrationSupervision_Supervised:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationIntegrationSupervision_Supervised{Supervised: p.projectSupervisedIntegration(v.Supervised)}
	case *native.IntegrationSupervision_NotSupervised:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationIntegrationSupervision_NotSupervised{NotSupervised: &emptypb.Empty{}}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectSaturatedCounter(value *native.SaturatedCounter) *api.ObservationSaturatedCounter {
	if value == nil {
		return nil
	}
	result := &api.ObservationSaturatedCounter{}
	result.LowerBound = int64(min(value.Value, uint64(math.MaxInt64)))
	return result
}

func (p *observationProjection) projectCounterMeasurement(value *native.CounterMeasurement) *api.ObservationCounterMeasurement {
	if value == nil {
		return nil
	}
	result := &api.ObservationCounterMeasurement{}
	switch v := value.Result.(type) {
	case *native.CounterMeasurement_Exact:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		if v.Exact > math.MaxInt64 {
			result.Result = &api.ObservationCounterMeasurement_Saturated{Saturated: &api.ObservationSaturatedCounter{LowerBound: math.MaxInt64}}
		} else {
			result.Result = &api.ObservationCounterMeasurement_Exact{Exact: int64(v.Exact)}
		}
	case *native.CounterMeasurement_Saturated:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationCounterMeasurement_Saturated{Saturated: p.projectSaturatedCounter(v.Saturated)}
	case *native.CounterMeasurement_NotMeasured:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationCounterMeasurement_NotMeasured{NotMeasured: &emptypb.Empty{}}
	case *native.CounterMeasurement_Unavailable:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationCounterMeasurement_Unavailable{Unavailable: p.projectReadUnavailable(v.Unavailable)}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectGaugeMeasurement(value *native.GaugeMeasurement) *api.ObservationGaugeMeasurement {
	if value == nil {
		return nil
	}
	result := &api.ObservationGaugeMeasurement{}
	switch v := value.Result.(type) {
	case *native.GaugeMeasurement_Value:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationGaugeMeasurement_Value{Value: p.signed(v.Value)}
	case *native.GaugeMeasurement_NotMeasured:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationGaugeMeasurement_NotMeasured{NotMeasured: &emptypb.Empty{}}
	case *native.GaugeMeasurement_Unavailable:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationGaugeMeasurement_Unavailable{Unavailable: p.projectReadUnavailable(v.Unavailable)}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectPositionMeasurement(value *native.PositionMeasurement) *api.ObservationPositionMeasurement {
	if value == nil {
		return nil
	}
	result := &api.ObservationPositionMeasurement{}
	switch v := value.Result.(type) {
	case *native.PositionMeasurement_Value:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationPositionMeasurement_Value{Value: p.projectAdmissionPosition(v.Value)}
	case *native.PositionMeasurement_Unavailable:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationPositionMeasurement_Unavailable{Unavailable: p.projectReadUnavailable(v.Unavailable)}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectLagMeasurement(value *native.LagMeasurement) *api.ObservationLagMeasurement {
	if value == nil {
		return nil
	}
	result := &api.ObservationLagMeasurement{}
	switch v := value.Result.(type) {
	case *native.LagMeasurement_SameEpochPositions:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationLagMeasurement_SameEpochPositions{SameEpochPositions: apitypes.DecimalUint64FromNative(v.SameEpochPositions).String()}
	case *native.LagMeasurement_Invalidated:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationLagMeasurement_Invalidated{Invalidated: &emptypb.Empty{}}
	case *native.LagMeasurement_EpochMismatch:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationLagMeasurement_EpochMismatch{EpochMismatch: &emptypb.Empty{}}
	case *native.LagMeasurement_Unavailable:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationLagMeasurement_Unavailable{Unavailable: p.projectReadUnavailable(v.Unavailable)}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectKnownLost(value *native.KnownLost) *api.KnownLost {
	if value == nil {
		return nil
	}
	result := &api.KnownLost{}
	switch v := value.Result.(type) {
	case *native.KnownLost_LowerBound:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.KnownLost_LowerBound{LowerBound: int64(min(v.LowerBound, uint64(math.MaxInt64)))}
	case *native.KnownLost_NotKnown:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.KnownLost_NotKnown{NotKnown: &emptypb.Empty{}}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectInstalledQueueMetrics(value *native.InstalledQueueMetrics) *api.ObservationInstalledQueueMetrics {
	if value == nil {
		return nil
	}
	result := &api.ObservationInstalledQueueMetrics{}
	result.ActiveQueues = p.projectGaugeMeasurement(value.ActiveQueues)
	result.QueuedEntries = p.projectGaugeMeasurement(value.QueuedEntries)
	result.CapacityEntries = p.projectGaugeMeasurement(value.CapacityEntries)
	result.RejectedTotal = p.projectCounterMeasurement(value.RejectedTotal)
	result.LostDeliveriesTotal = p.projectCounterMeasurement(value.LostDeliveriesTotal)
	result.CoalescedTotal = p.projectCounterMeasurement(value.CoalescedTotal)
	result.DegradedQueues = p.projectGaugeMeasurement(value.DegradedQueues)
	result.QueuedAccountedBytes = p.projectGaugeMeasurement(value.QueuedAccountedBytes)
	result.CapacityAccountedBytes = p.projectGaugeMeasurement(value.CapacityAccountedBytes)
	result.InFlightEntries = p.projectGaugeMeasurement(value.InFlightEntries)
	result.PendingEntries = p.projectGaugeMeasurement(value.PendingEntries)
	return result
}

func (p *observationProjection) projectQueueAggregate(value *native.QueueAggregate) *api.ObservationQueueAggregate {
	if value == nil {
		return nil
	}
	result := &api.ObservationQueueAggregate{}
	result.QueueClass = api.ObservationQueueClass(value.QueueClass)
	switch v := value.Result.(type) {
	case *native.QueueAggregate_Installed:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationQueueAggregate_Installed{Installed: p.projectInstalledQueueMetrics(v.Installed)}
	case *native.QueueAggregate_NotInstalled:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationQueueAggregate_NotInstalled{NotInstalled: &emptypb.Empty{}}
	case *native.QueueAggregate_Unavailable:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationQueueAggregate_Unavailable{Unavailable: p.projectReadUnavailable(v.Unavailable)}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectQueueLag(value *native.QueueLag) *api.ObservationQueueLag {
	if value == nil {
		return nil
	}
	result := &api.ObservationQueueLag{}
	result.QueueClass = api.ObservationQueueClass(value.QueueClass)
	switch v := value.Result.(type) {
	case *native.QueueLag_Installed:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationQueueLag_Installed{Installed: p.projectLagMeasurement(v.Installed)}
	case *native.QueueLag_NotInstalled:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationQueueLag_NotInstalled{NotInstalled: &emptypb.Empty{}}
	case *native.QueueLag_Unavailable:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationQueueLag_Unavailable{Unavailable: p.projectReadUnavailable(v.Unavailable)}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectCommittedGapMetrics(value *native.CommittedGapMetrics) *api.ObservationCommittedGapMetrics {
	if value == nil {
		return nil
	}
	result := &api.ObservationCommittedGapMetrics{}
	result.Reason = api.GapReason(value.Reason)
	result.RowsTotal = p.projectCounterMeasurement(value.RowsTotal)
	result.KnownLostLowerBoundTotal = p.projectCounterMeasurement(value.KnownLostLowerBoundTotal)
	result.RowsWithoutKnownCountTotal = p.projectCounterMeasurement(value.RowsWithoutKnownCountTotal)
	return result
}

func (p *observationProjection) projectActiveCoverageInterval(value *native.ActiveCoverageInterval) *api.ObservationActiveCoverageInterval {
	if value == nil {
		return nil
	}
	result := &api.ObservationActiveCoverageInterval{}
	result.From = p.projectAdmissionPosition(value.From)
	result.Through = p.projectAdmissionPosition(value.Through)
	result.KnownLost = p.projectKnownLost(value.KnownLost)
	return result
}

func (p *observationProjection) projectActiveCoverage(value *native.ActiveCoverage) *api.ObservationActiveCoverage {
	if value == nil {
		return nil
	}
	result := &api.ObservationActiveCoverage{}
	switch v := value.Result.(type) {
	case *native.ActiveCoverage_AtRisk:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationActiveCoverage_AtRisk{AtRisk: p.projectActiveCoverageInterval(v.AtRisk)}
	case *native.ActiveCoverage_NoActiveRisk:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationActiveCoverage_NoActiveRisk{NoActiveRisk: &emptypb.Empty{}}
	case *native.ActiveCoverage_Unavailable:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationActiveCoverage_Unavailable{Unavailable: p.projectReadUnavailable(v.Unavailable)}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectInstalledRecorderMetrics(value *native.InstalledRecorderMetrics) *api.ObservationInstalledRecorderMetrics {
	if value == nil {
		return nil
	}
	result := &api.ObservationInstalledRecorderMetrics{}
	result.QueuedBytes = p.projectGaugeMeasurement(value.QueuedBytes)
	result.PendingBytes = p.projectGaugeMeasurement(value.PendingBytes)
	result.TotalCapacityBytes = p.projectGaugeMeasurement(value.TotalCapacityBytes)
	result.QueueCapacityBytes = p.projectGaugeMeasurement(value.QueueCapacityBytes)
	result.QueuedEntries = p.projectGaugeMeasurement(value.QueuedEntries)
	result.RetriesTotal = p.projectCounterMeasurement(value.RetriesTotal)
	result.LostEntriesTotal = p.projectCounterMeasurement(value.LostEntriesTotal)
	result.PolicyExcludedTotal = p.projectCounterMeasurement(value.PolicyExcludedTotal)
	result.Checkpoint = p.projectPositionMeasurement(value.Checkpoint)
	result.Tail = p.projectPositionMeasurement(value.Tail)
	result.RecorderLagPositions = p.projectLagMeasurement(value.RecorderLagPositions)
	for _, item := range value.CommittedGaps {
		result.CommittedGaps = append(result.CommittedGaps, p.projectCommittedGapMetrics(item))
	}
	result.ActiveCoverage = p.projectActiveCoverage(value.ActiveCoverage)
	return result
}

func (p *observationProjection) projectRecorderMetrics(value *native.RecorderMetrics) *api.ObservationRecorderMetrics {
	if value == nil {
		return nil
	}
	result := &api.ObservationRecorderMetrics{}
	switch v := value.Result.(type) {
	case *native.RecorderMetrics_Installed:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationRecorderMetrics_Installed{Installed: p.projectInstalledRecorderMetrics(v.Installed)}
	case *native.RecorderMetrics_NotInstalled:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationRecorderMetrics_NotInstalled{NotInstalled: &emptypb.Empty{}}
	case *native.RecorderMetrics_Unavailable:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationRecorderMetrics_Unavailable{Unavailable: p.projectReadUnavailable(v.Unavailable)}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectFirstAttemptMetrics(value *native.FirstAttemptMetrics) *api.ObservationFirstAttemptMetrics {
	if value == nil {
		return nil
	}
	result := &api.ObservationFirstAttemptMetrics{}
	result.Ledger = api.ObservationAttemptLedger(value.Ledger)
	result.IndeterminateTotal = p.projectCounterMeasurement(value.IndeterminateTotal)
	return result
}

func (p *observationProjection) projectActionMetrics(value *native.ActionMetrics) *api.ObservationActionMetrics {
	if value == nil {
		return nil
	}
	result := &api.ObservationActionMetrics{}
	result.ProgressIndeterminateTotal = p.projectCounterMeasurement(value.ProgressIndeterminateTotal)
	for _, item := range value.FirstAttempts {
		result.FirstAttempts = append(result.FirstAttempts, p.projectFirstAttemptMetrics(item))
	}
	result.DurableRecoveredIndeterminateTotal = p.projectCounterMeasurement(value.DurableRecoveredIndeterminateTotal)
	return result
}

func (p *observationProjection) projectRunKindMetrics(value *native.RunKindMetrics) *api.ObservationRunKindMetrics {
	if value == nil {
		return nil
	}
	result := &api.ObservationRunKindMetrics{}
	result.Kind = api.ObservationRunKind(value.Kind)
	result.Active = p.projectGaugeMeasurement(value.Active)
	result.Pending = p.projectGaugeMeasurement(value.Pending)
	result.CancelledTotal = p.projectCounterMeasurement(value.CancelledTotal)
	result.SupersededTotal = p.projectCounterMeasurement(value.SupersededTotal)
	return result
}

func (p *observationProjection) projectInstalledRunMetrics(value *native.InstalledRunMetrics) *api.ObservationInstalledRunMetrics {
	if value == nil {
		return nil
	}
	result := &api.ObservationInstalledRunMetrics{}
	for _, item := range value.Kinds {
		result.Kinds = append(result.Kinds, p.projectRunKindMetrics(item))
	}
	result.Preparing = p.projectGaugeMeasurement(value.Preparing)
	result.Controller = p.controllerName(value.Controller)
	return result
}

func (p *observationProjection) projectRunMetrics(value *native.RunMetrics) *api.ObservationRunMetrics {
	if value == nil {
		return nil
	}
	result := &api.ObservationRunMetrics{}
	switch v := value.Result.(type) {
	case *native.RunMetrics_Installed:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationRunMetrics_Installed{Installed: p.projectInstalledRunMetrics(v.Installed)}
	case *native.RunMetrics_NotInstalled:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationRunMetrics_NotInstalled{NotInstalled: &emptypb.Empty{}}
	case *native.RunMetrics_Unavailable:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationRunMetrics_Unavailable{Unavailable: p.projectReadUnavailable(v.Unavailable)}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectJournalMetrics(value *native.JournalMetrics) *api.ObservationJournalMetrics {
	if value == nil {
		return nil
	}
	result := &api.ObservationJournalMetrics{}
	result.RetainedRows = p.projectGaugeMeasurement(value.RetainedRows)
	result.ChargedBytes = p.projectGaugeMeasurement(value.ChargedBytes)
	result.EvictedTotal = p.projectCounterMeasurement(value.EvictedTotal)
	return result
}

func (p *observationProjection) projectObservationCut(value *native.ObservationCut) *api.ObservationObservationCut {
	if value == nil {
		return nil
	}
	result := &api.ObservationObservationCut{}
	result.Owner = api.ObservationMetricOwner(value.Owner)
	if value.CapturedAt != nil {
		result.CapturedAt = proto.Clone(value.CapturedAt).(*timestamppb.Timestamp)
	}
	return result
}

func (p *observationProjection) projectMetricsSnapshot(value *native.MetricsSnapshot) *api.Metrics {
	if value == nil {
		return nil
	}
	result := &api.Metrics{}
	result.ProcessEpoch = p.projectProcessObservationEpoch(value.ProcessEpoch)
	for _, item := range value.Queues {
		result.Queues = append(result.Queues, p.projectQueueAggregate(item))
	}
	for _, item := range value.QueueLags {
		result.QueueLags = append(result.QueueLags, p.projectQueueLag(item))
	}
	result.Recorder = p.projectRecorderMetrics(value.Recorder)
	result.Actions = p.projectActionMetrics(value.Actions)
	result.Runs = p.projectRunMetrics(value.Runs)
	result.Journal = p.projectJournalMetrics(value.Journal)
	for _, item := range value.Cuts {
		result.Cuts = append(result.Cuts, p.projectObservationCut(item))
	}
	return result
}

func (p *observationProjection) projectRuntimeAuthor(value *native.RuntimeAuthor) *api.ObservationRuntimeAuthor {
	if value == nil {
		return nil
	}
	result := &api.ObservationRuntimeAuthor{}
	switch v := value.Result.(type) {
	case *native.RuntimeAuthor_Process:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationRuntimeAuthor_Process{Process: &emptypb.Empty{}}
	case *native.RuntimeAuthor_InternalAutomation:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationRuntimeAuthor_InternalAutomation{InternalAutomation: &emptypb.Empty{}}
	case *native.RuntimeAuthor_HumanAccount:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationRuntimeAuthor_HumanAccount{HumanAccount: p.account(v.HumanAccount.GetValue())}
	case *native.RuntimeAuthor_Unauthenticated:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationRuntimeAuthor_Unauthenticated{Unauthenticated: &emptypb.Empty{}}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectRuntimeEvent(value *native.RuntimeEvent) *api.ObservationRuntimeEvent {
	if value == nil {
		return nil
	}
	result := &api.ObservationRuntimeEvent{}
	result.Code = api.ObservationLogCode(value.Code)
	result.Level = api.ObservationLogLevel(value.Level)
	result.Component = api.ObservationLogComponent(value.Component)
	if value.OccurredAt != nil {
		result.OccurredAt = proto.Clone(value.OccurredAt).(*timestamppb.Timestamp)
	}
	result.Author = p.projectRuntimeAuthor(value.Author)
	return result
}

func (p *observationProjection) projectRuntimeLogRow(value *native.RuntimeLogRow) *api.ObservationRuntimeLogRow {
	if value == nil {
		return nil
	}
	result := &api.ObservationRuntimeLogRow{}
	result.Sequence = apitypes.DecimalUint64FromNative(value.Sequence).String()
	result.Event = p.projectRuntimeEvent(value.Event)
	return result
}

func (p *observationProjection) projectIntegrationReference(value *native.IntegrationReference) *api.ObservationIntegrationReference {
	if value == nil {
		return nil
	}
	result := &api.ObservationIntegrationReference{}
	result.Value = append([]byte(nil), value.Value...)
	return result
}

func (p *observationProjection) projectAuditAuthor(value *native.AuditAuthor) *api.ObservationAuditAuthor {
	if value == nil {
		return nil
	}
	result := &api.ObservationAuditAuthor{}
	switch v := value.Result.(type) {
	case *native.AuditAuthor_HumanAccount:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationAuditAuthor_HumanAccount{HumanAccount: p.account(v.HumanAccount.GetValue())}
	case *native.AuditAuthor_InternalIntegration:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationAuditAuthor_InternalIntegration{InternalIntegration: p.projectIntegrationReference(v.InternalIntegration)}
	case *native.AuditAuthor_NotRecorded:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationAuditAuthor_NotRecorded{NotRecorded: &emptypb.Empty{}}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectAuditRoute(value *native.AuditRoute) *api.ObservationAuditRoute {
	if value == nil {
		return nil
	}
	result := &api.ObservationAuditRoute{}
	switch v := value.Result.(type) {
	case *native.AuditRoute_RegisteredProcedure:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationAuditRoute_RegisteredProcedure{RegisteredProcedure: v.RegisteredProcedure}
	case *native.AuditRoute_OtherRegisteredRoute:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationAuditRoute_OtherRegisteredRoute{OtherRegisteredRoute: &emptypb.Empty{}}
	case *native.AuditRoute_NotRecorded:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationAuditRoute_NotRecorded{NotRecorded: &emptypb.Empty{}}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectStoredMetadataInvalid(value *native.StoredMetadataInvalid) *api.ObservationStoredMetadataInvalid {
	if value == nil {
		return nil
	}
	result := &api.ObservationStoredMetadataInvalid{}
	for _, item := range value.Fields {
		result.Fields = append(result.Fields, api.ObservationStoredMetadataField(item))
	}
	return result
}

func (p *observationProjection) projectAuthenticationAudit(value *native.AuthenticationAudit) *api.ObservationAuthenticationAudit {
	if value == nil {
		return nil
	}
	result := &api.ObservationAuthenticationAudit{}
	if value.OccurredAt != nil {
		result.OccurredAt = proto.Clone(value.OccurredAt).(*timestamppb.Timestamp)
	}
	result.Action = api.ObservationAuthAuditAction(value.Action)
	result.Outcome = api.ObservationAuditOutcome(value.Outcome)
	result.Author = p.projectAuditAuthor(value.Author)
	result.Route = p.projectAuditRoute(value.Route)
	return result
}

func (p *observationProjection) projectSecretAccessAudit(value *native.SecretAccessAudit) *api.ObservationSecretAccessAudit {
	if value == nil {
		return nil
	}
	result := &api.ObservationSecretAccessAudit{}
	if value.OccurredAt != nil {
		result.OccurredAt = proto.Clone(value.OccurredAt).(*timestamppb.Timestamp)
	}
	result.Reason = api.ObservationSecretDenialReason(value.Reason)
	result.Author = p.projectAuditAuthor(value.Author)
	return result
}

func (p *observationProjection) projectAuditRow(value *native.AuditRow) *api.ObservationAuditRow {
	if value == nil {
		return nil
	}
	result := &api.ObservationAuditRow{}
	result.Id = value.Id
	switch v := value.Result.(type) {
	case *native.AuditRow_Authentication:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationAuditRow_Authentication{Authentication: p.projectAuthenticationAudit(v.Authentication)}
	case *native.AuditRow_SecretAccess:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationAuditRow_SecretAccess{SecretAccess: p.projectSecretAccessAudit(v.SecretAccess)}
	case *native.AuditRow_StoredMetadataInvalid:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationAuditRow_StoredMetadataInvalid{StoredMetadataInvalid: p.projectStoredMetadataInvalid(v.StoredMetadataInvalid)}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectTraceOrdinal(value *native.TraceOrdinal) *api.ObservationTraceOrdinal {
	if value == nil {
		return nil
	}
	result := &api.ObservationTraceOrdinal{}
	switch v := value.Result.(type) {
	case *native.TraceOrdinal_Native:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationTraceOrdinal_Native{Native: apitypes.DecimalUint64FromNative(v.Native).String()}
	case *native.TraceOrdinal_Exhausted:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationTraceOrdinal_Exhausted{Exhausted: &emptypb.Empty{}}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectControllerTraceRecord(value *native.ControllerTraceRecord) *api.ObservationControllerTraceRecord {
	if value == nil {
		return nil
	}
	result := &api.ObservationControllerTraceRecord{}
	result.Decision = api.ObservationTraceDecision(value.Decision)
	result.TriggerPosition = p.projectAdmissionPosition(value.TriggerPosition)
	switch v := value.MaxExceeded.(type) {
	case *native.ControllerTraceRecord_Mode:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.MaxExceeded = &api.ObservationControllerTraceRecord_Mode{Mode: api.ObservationTraceMaxExceededMode(v.Mode)}
	case *native.ControllerTraceRecord_NotApplicable:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.MaxExceeded = &api.ObservationControllerTraceRecord_NotApplicable{NotApplicable: &emptypb.Empty{}}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectTraceFailure(value *native.TraceFailure) *api.ObservationTraceFailure {
	if value == nil {
		return nil
	}
	result := &api.ObservationTraceFailure{}
	switch v := value.Result.(type) {
	case *native.TraceFailure_Known:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationTraceFailure_Known{Known: api.ObservationTraceExecutionErrorKind(v.Known)}
	case *native.TraceFailure_NoFailure:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationTraceFailure_NoFailure{NoFailure: &emptypb.Empty{}}
	case *native.TraceFailure_NotRecorded:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Result = &api.ObservationTraceFailure_NotRecorded{NotRecorded: &emptypb.Empty{}}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectNodeTraceRecord(value *native.NodeTraceRecord) *api.ObservationNodeTraceRecord {
	if value == nil {
		return nil
	}
	result := &api.ObservationNodeTraceRecord{}
	result.ActionKind = api.ObservationTraceActionKind(value.ActionKind)
	result.Stage = api.ObservationTraceNodeStage(value.Stage)
	if value.OccurredAt != nil {
		result.OccurredAt = proto.Clone(value.OccurredAt).(*timestamppb.Timestamp)
	}
	result.Failure = p.projectTraceFailure(value.Failure)
	return result
}

func (p *observationProjection) projectTraceRow(value *native.TraceRow) *api.ObservationTraceRow {
	if value == nil {
		return nil
	}
	result := &api.ObservationTraceRow{}
	result.Run = p.runName(value.RunId)
	if value.LocalIndex > 511 {
		p.err = storage.ErrAPIResourceIntegrity
		return nil
	}
	result.LocalIndex = int32(value.LocalIndex)
	result.Ordinal = p.projectTraceOrdinal(value.Ordinal)
	switch v := value.Record.(type) {
	case *native.TraceRow_Controller:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Record = &api.ObservationTraceRow_Controller{Controller: p.projectControllerTraceRecord(v.Controller)}
	case *native.TraceRow_Node:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Record = &api.ObservationTraceRow_Node{Node: p.projectNodeTraceRecord(v.Node)}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectTraceCutMetadata(value *native.TraceCutMetadata) *api.ObservationTraceCutMetadata {
	if value == nil {
		return nil
	}
	result := &api.ObservationTraceCutMetadata{}
	result.Coherence = api.ObservationTraceCutCoherence(value.Coherence)
	result.Order = api.ObservationTraceOrderState(value.Order)
	result.ControllerEvicted = apitypes.DecimalUint64FromNative(value.ControllerEvicted).String()
	result.NodeEvicted = apitypes.DecimalUint64FromNative(value.NodeEvicted).String()
	if value.ControllerCapturedAt != nil {
		result.ControllerCapturedAt = proto.Clone(value.ControllerCapturedAt).(*timestamppb.Timestamp)
	}
	if value.NodeCapturedAt != nil {
		result.NodeCapturedAt = proto.Clone(value.NodeCapturedAt).(*timestamppb.Timestamp)
	}
	return result
}

func (p *observationProjection) projectRecorderEpochIdentity(value *native.RecorderEpochIdentity) *api.RecorderEpochIdentity {
	if value == nil {
		return nil
	}
	result := &api.RecorderEpochIdentity{}
	result.Value = append([]byte(nil), value.Value...)
	return result
}

func (p *observationProjection) projectPreviousRecorderEpoch(value *native.PreviousRecorderEpoch) *api.PreviousRecorderEpoch {
	if value == nil {
		return nil
	}
	result := &api.PreviousRecorderEpoch{}
	result.Epoch = p.projectRecorderEpochIdentity(value.Epoch)
	result.Checkpoint = apitypes.DecimalUint64FromNative(value.Checkpoint).String()
	result.CleanClose = value.CleanClose
	return result
}

func (p *observationProjection) projectRecorderEpochCoverage(value *native.RecorderEpochCoverage) *api.RecorderEpochCoverage {
	if value == nil {
		return nil
	}
	result := &api.RecorderEpochCoverage{}
	result.Epoch = p.projectRecorderEpochIdentity(value.Epoch)
	result.Checkpoint = apitypes.DecimalUint64FromNative(value.Checkpoint).String()
	result.PolicyExcluded = apitypes.DecimalUint64FromNative(value.PolicyExcluded).String()
	if value.StartedAt != nil {
		result.StartedAt = proto.Clone(value.StartedAt).(*timestamppb.Timestamp)
	}
	result.CleanClose = value.CleanClose
	result.Reconciliation = api.RecorderReconciliationState(value.Reconciliation)
	switch v := value.CloseTime.(type) {
	case *native.RecorderEpochCoverage_ClosedAt:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.CloseTime = &api.RecorderEpochCoverage_ClosedAt{ClosedAt: proto.Clone(v.ClosedAt).(*timestamppb.Timestamp)}
	case *native.RecorderEpochCoverage_NotRecorded:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.CloseTime = &api.RecorderEpochCoverage_NotRecorded{NotRecorded: &emptypb.Empty{}}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	switch v := value.Predecessor.(type) {
	case *native.RecorderEpochCoverage_Previous:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Predecessor = &api.RecorderEpochCoverage_Previous{Previous: p.projectPreviousRecorderEpoch(v.Previous)}
	case *native.RecorderEpochCoverage_NoPrevious:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.Predecessor = &api.RecorderEpochCoverage_NoPrevious{NoPrevious: &emptypb.Empty{}}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectRecorderCoverageGap(value *native.RecorderCoverageGap) *api.RecorderCoverageGap {
	if value == nil {
		return nil
	}
	result := &api.RecorderCoverageGap{}
	result.Epoch = p.projectRecorderEpochIdentity(value.Epoch)
	result.FromSequence = apitypes.DecimalUint64FromNative(value.FromSequence).String()
	result.ThroughSequence = apitypes.DecimalUint64FromNative(value.ThroughSequence).String()
	result.Reason = api.GapReason(value.Reason)
	result.KnownLost = p.projectKnownLost(value.KnownLost)
	result.Envelope = api.GapEnvelope(value.Envelope)
	switch v := value.UtcBounds.(type) {
	case *native.RecorderCoverageGap_UnknownUtc:
		if v == nil {
			p.err = storage.ErrAPIResourceIntegrity
			break
		}
		result.UtcBounds = &api.RecorderCoverageGap_UnknownUtc{UnknownUtc: &emptypb.Empty{}}
	default:
		p.err = storage.ErrAPIResourceIntegrity
	}
	return result
}

func (p *observationProjection) projectRecorderCoverageSnapshot(value *native.RecorderCoverageSnapshot) *api.RecorderCoverageSnapshot {
	if value == nil {
		return nil
	}
	result := &api.RecorderCoverageSnapshot{}
	result.From = p.projectAdmissionPosition(value.From)
	result.Through = p.projectAdmissionPosition(value.Through)
	for _, item := range value.Epochs {
		result.Epochs = append(result.Epochs, p.projectRecorderEpochCoverage(item))
	}
	for _, item := range value.Gaps {
		result.Gaps = append(result.Gaps, p.projectRecorderCoverageGap(item))
	}
	return result
}

// signed admits native bounded gauges/revisions without wrapping a full-width value.
func (p *observationProjection) signed(value uint64) int64 {
	if value > math.MaxInt64 {
		p.err = storage.ErrAPIResourceIntegrity
		return 0
	}
	return int64(value)
}
