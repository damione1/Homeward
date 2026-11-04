package contracts

import (
	"bytes"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
	"math"
	"strings"
)

func observationSingletonName(value, suffix string) bool {
	parent, ok := strings.CutSuffix(value, "/"+suffix)
	if !ok {
		return false
	}
	_, err := apitypes.ParseHomeName(parent)
	return err == nil
}

// AdmitObservationResponse validates independent v2 projections. It does not
// translate them back into native DTOs or infer absent measurements as zero.
func AdmitObservationResponse(m proto.Message) error {
	if observationNil(m) || observationNilOneof(m) || !observationV2Tree(m.ProtoReflect(), 0) {
		return ErrInvalidObservabilityContract
	}
	limit := ObservationPageMaxBytes
	switch v := m.(type) {
	case *api.Health:
		limit = 16 << 10
	case *api.Metrics:
		limit = 32 << 10
	case *api.IntegrationHealth:
		limit = 8 << 10
		parent, ok := strings.CutSuffix(v.Name, "/health")
		if !ok {
			return ErrInvalidObservabilityContract
		}
		if _, err := apitypes.ParseIntegrationInstanceName(parent); err != nil {
			return ErrInvalidObservabilityContract
		}
	case *api.QueryRuntimeLogsResponse:
		var last uint64
		for _, row := range v.Rows {
			if row.Sequence <= last || row.Sequence > v.UpperSequence {
				return ErrInvalidObservabilityContract
			}
			last = row.Sequence
		}
	case *api.QueryAuditsResponse:
		var last int64 = math.MaxInt64
		for i, row := range v.Rows {
			if i > 0 && row.Id >= last {
				return ErrInvalidObservabilityContract
			}
			last = row.Id
			if v.Source == api.ObservationAuditSource_OBSERVATION_AUDIT_SOURCE_AUTHENTICATION && row.GetSecretAccess() != nil || v.Source == api.ObservationAuditSource_OBSERVATION_AUDIT_SOURCE_SECRET_ACCESS && row.GetAuthentication() != nil {
				return ErrInvalidObservabilityContract
			}
		}
	case *api.QueryRunTraceResponse:
		if _, err := apitypes.ParseRuntimeRunName(v.Run); err != nil {
			return ErrInvalidObservabilityContract
		}
		var run string
		var last uint32
		for i, row := range v.Rows {
			if row.Run != v.Run || i > 0 && (row.Run != run || row.LocalIndex <= last) {
				return ErrInvalidObservabilityContract
			}
			run = row.Run
			last = row.LocalIndex
		}
	case *api.QueryRecorderCoverageResponse:
	default:
		return ErrInvalidObservabilityContract
	}
	if !observationV2Sized(m, limit) {
		return ErrInvalidObservabilityContract
	}
	return nil
}
func observationV2ListLimit(m proto.Message, name protoreflect.Name) int {
	switch m.(type) {
	case *api.Metrics:
		if name == "queues" {
			return 9
		}
		if name == "queue_lags" {
			return 4
		}
		return 8
	case *api.Health:
		return 5
	case *api.ObservationServerHealth, *api.ObservationStoredMetadataInvalid:
		return 6
	case *api.ObservationInstalledRecorderMetrics:
		return 3
	case *api.ObservationActionMetrics, *api.ObservationInstalledRunMetrics:
		return 2
	case *api.RecorderCoverageSnapshot:
		if name == "epochs" {
			return 64
		}
		return 512
	default:
		return ObservationPageMaxRows
	}
}

func observationV2Tree(m protoreflect.Message, depth int) bool {
	if !m.IsValid() || depth > 16 || len(m.GetUnknown()) != 0 || observationNilOneof(m.Interface()) {
		return false
	}
	d := m.Descriptor()
	for i := 0; i < d.Oneofs().Len(); i++ {
		o := d.Oneofs().Get(i)
		if !o.IsSynthetic() && m.WhichOneof(o) == nil {
			return false
		}
	}
	for i := 0; i < d.Fields().Len(); i++ {
		f := d.Fields().Get(i)
		if o := f.ContainingOneof(); o != nil && !m.Has(f) {
			continue
		}
		if f.IsMap() {
			return false
		}
		if f.IsList() {
			list := m.Get(f).List()
			if list.Len() > observationV2ListLimit(m.Interface(), f.Name()) {
				return false
			}
			var enumSeen uint64
			for j := 0; j < list.Len(); j++ {
				v := list.Get(j)
				if f.Kind() == protoreflect.MessageKind && !observationV2Tree(v.Message(), depth+1) {
					return false
				}
				if f.Kind() == protoreflect.EnumKind {
					n := v.Enum()
					if n <= 0 || n >= 64 || f.Enum().Values().ByNumber(n) == nil || enumSeen&(uint64(1)<<n) != 0 {
						return false
					}
					enumSeen |= uint64(1) << n
				}
			}
			continue
		}
		switch f.Kind() {
		case protoreflect.MessageKind:
			if !m.Has(f) || !observationV2Tree(m.Get(f).Message(), depth+1) {
				return false
			}
		case protoreflect.EnumKind:
			if n := m.Get(f).Enum(); n == 0 || f.Enum().Values().ByNumber(n) == nil {
				return false
			}
		case protoreflect.StringKind:
			if f.Name() != "next_page_token" && !observationText(m.Get(f).String(), ObservationTokenMaxBytes) || f.Name() == "next_page_token" && len(m.Get(f).String()) > ObservationTokenMaxBytes {
				return false
			}
		}
	}
	return observationV2Local(m.Interface())
}

func observationV2Local(m proto.Message) bool {
	switch v := m.(type) {
	case *timestamppb.Timestamp:
		return v.CheckValid() == nil && !v.AsTime().IsZero()
	case *api.ObservationRuntimeAuthor:
		if v.GetHumanAccount() != "" {
			_, err := apitypes.ParseAccountName(v.GetHumanAccount())
			return err == nil
		}
		return true
	case *api.ObservationAuditAuthor:
		if v.GetHumanAccount() != "" {
			_, err := apitypes.ParseAccountName(v.GetHumanAccount())
			return err == nil
		}
		return true
	case *api.ObservationProcessObservationEpoch:
		return observationID(v.Value, 16)
	case *api.ObservationIntegrationReference:
		return observationID(v.Value, 16)
	case *api.RecorderEpochIdentity:
		return observationID(v.Value, 16)
	case *api.AdmissionPosition:
		return observationID(v.Epoch, 16)
	case *api.ObservationActiveRevision:
		return v.Value > 0 && v.Value <= math.MaxInt64
	case *api.ObservationSaturatedCounter:
		return v.Value == math.MaxUint64
	case *api.KnownLost:
		if n, ok := v.Result.(*api.KnownLost_LowerBound); ok {
			return n.LowerBound > 0
		}
		return true
	case *api.ObservationServerHealth:
		return v.Readiness != api.ObservationServerReadiness_OBSERVATION_SERVER_READINESS_READY || v.Generation.GetPresent() != nil && len(v.Causes) == 0
	case *api.ObservationDurableHealth:
		return (v.Availability == api.ObservationDurableAvailability_OBSERVATION_DURABLE_AVAILABILITY_UNKNOWN) == (v.GetNotObserved() != nil)
	case *api.ObservationPilotPhaseCount:
		return v.Instances <= 1024
	case *api.ObservationSupervisedIntegration:
		return v.Attempts <= 17 && v.Tasks <= 256
	case *api.Health:
		if !observationSingletonName(v.Name, "health") {
			return false
		}
		if len(v.Pilots) != 5 {
			return false
		}
		var seen uint64
		var total uint64
		for _, p := range v.Pilots {
			bit := uint64(1) << p.Phase
			if seen&bit != 0 {
				return false
			}
			seen |= bit
			total += p.Instances
		}
		return total <= 1024 && seen == observationDomainMask(api.ObservationPilotPhase_OBSERVATION_PILOT_PHASE_UNSPECIFIED.Descriptor())
	case *api.Metrics:
		return observationSingletonName(v.Name, "metrics") && observationV2Metrics(v)
	case *api.ObservationQueueAggregate:
		if installed := v.GetInstalled(); installed != nil {
			return observationV2QueueApplicability(v.QueueClass, installed)
		}
		return true
	case *api.ObservationInstalledQueueMetrics:
		return observationV2GaugeLE(v.QueuedEntries, v.CapacityEntries) && observationV2GaugeLE(v.DegradedQueues, v.ActiveQueues) && observationV2GaugeLE(v.QueuedAccountedBytes, v.CapacityAccountedBytes)
	case *api.ObservationInstalledRecorderMetrics:
		if len(v.CommittedGaps) != 3 {
			return false
		}
		var seen uint64
		for _, g := range v.CommittedGaps {
			bit := uint64(1) << g.Reason
			if seen&bit != 0 {
				return false
			}
			seen |= bit
		}
		return seen == observationDomainMask(api.GapReason_GAP_REASON_UNSPECIFIED.Descriptor())
	case *api.ObservationActionMetrics:
		return len(v.FirstAttempts) == 2 && (uint64(1)<<v.FirstAttempts[0].Ledger|uint64(1)<<v.FirstAttempts[1].Ledger) == observationDomainMask(api.ObservationAttemptLedger_OBSERVATION_ATTEMPT_LEDGER_UNSPECIFIED.Descriptor())
	case *api.ObservationInstalledRunMetrics:
		_, nameErr := apitypes.ParseRuntimeControllerName(v.Controller)
		return nameErr == nil && len(v.Kinds) == 2 && (uint64(1)<<v.Kinds[0].Kind|uint64(1)<<v.Kinds[1].Kind) == observationDomainMask(api.ObservationRunKind_OBSERVATION_RUN_KIND_UNSPECIFIED.Descriptor())
	case *api.ObservationActiveCoverageInterval:
		return bytes.Equal(v.From.Epoch, v.Through.Epoch) && v.From.Sequence > 0 && v.Through.Sequence >= v.From.Sequence &&
			(v.KnownLost.GetLowerBound() == 0 || v.KnownLost.GetLowerBound() <= v.Through.Sequence-v.From.Sequence+1)
	case *api.ObservationRuntimeEvent:
		return observationV2EventCode(v) && observationV2Sized(v, ObservationEventMaxBytes)
	case *api.ObservationRuntimeLogRow:
		return v.Sequence > 0
	case *api.ObservationAuditRoute:
		return v.GetRegisteredProcedure() == "" || observationText(v.GetRegisteredProcedure(), 256) && strings.HasPrefix(v.GetRegisteredProcedure(), "/")
	case *api.ObservationStoredMetadataInvalid:
		return len(v.Fields) > 0
	case *api.ObservationAuthenticationAudit:
		return v.Author.GetInternalIntegration() == nil
	case *api.ObservationSecretAccessAudit:
		return v.Author.GetInternalIntegration() != nil
	case *api.ObservationAuditRow:
		return v.Id > 0
	case *api.ObservationTraceOrdinal:
		if n, ok := v.Result.(*api.ObservationTraceOrdinal_Native); ok {
			return n.Native > 0 && n.Native < math.MaxUint64
		}
		return true
	case *api.ObservationControllerTraceRecord:
		if v.TriggerPosition == nil {
			return true
		}
		switch v.Decision {
		case api.ObservationTraceDecision_OBSERVATION_TRACE_DECISION_ADMITTED, api.ObservationTraceDecision_OBSERVATION_TRACE_DECISION_QUEUED, api.ObservationTraceDecision_OBSERVATION_TRACE_DECISION_RESTART_PENDING:
			return true
		default:
			return false
		}
	case *api.ObservationTraceRow:
		_, err := apitypes.ParseRuntimeRunName(v.Run)
		return err == nil && v.LocalIndex < 512
	case *api.RecorderEpochCoverage:
		return !v.CleanClose || v.GetClosedAt() != nil
	case *api.RecorderCoverageGap:
		return v.FromSequence > 0 && v.ThroughSequence >= v.FromSequence &&
			(v.KnownLost.GetLowerBound() == 0 || v.KnownLost.GetLowerBound() <= v.ThroughSequence-v.FromSequence+1) &&
			(v.Envelope != api.GapEnvelope_GAP_ENVELOPE_EXACT_POSITIONS || v.KnownLost.GetLowerBound() == v.ThroughSequence-v.FromSequence+1)
	case *api.RecorderCoverageSnapshot:
		if len(v.Epochs) == 0 || len(v.Epochs) > 64 || len(v.Gaps) > 512 || !bytes.Equal(v.From.Epoch, v.Epochs[0].Epoch.Value) || !bytes.Equal(v.Through.Epoch, v.Epochs[len(v.Epochs)-1].Epoch.Value) {
			return false
		}
		if v.From.Sequence > v.Epochs[0].Checkpoint || v.Through.Sequence > v.Epochs[len(v.Epochs)-1].Checkpoint || bytes.Equal(v.From.Epoch, v.Through.Epoch) && v.From.Sequence > v.Through.Sequence {
			return false
		}
		for i, epoch := range v.Epochs {
			for j := 0; j < i; j++ {
				if bytes.Equal(epoch.Epoch.Value, v.Epochs[j].Epoch.Value) {
					return false
				}
			}
		}
		for _, gap := range v.Gaps {
			found := false
			for _, epoch := range v.Epochs {
				if bytes.Equal(gap.Epoch.Value, epoch.Epoch.Value) && gap.ThroughSequence <= epoch.Checkpoint {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	default:
		return true
	}
}

func observationV2GaugeLE(left, right *api.ObservationGaugeMeasurement) bool {
	l, lok := left.GetResult().(*api.ObservationGaugeMeasurement_Value)
	r, rok := right.GetResult().(*api.ObservationGaugeMeasurement_Value)
	return !lok || !rok || l.Value <= r.Value
}

func observationV2Metrics(v *api.Metrics) bool {
	if len(v.Queues) != 9 || len(v.QueueLags) != 4 {
		return false
	}
	var queues, lags, cuts uint64
	for _, q := range v.Queues {
		bit := uint64(1) << q.QueueClass
		if queues&bit != 0 {
			return false
		}
		queues |= bit
	}
	for _, q := range v.QueueLags {
		if q.QueueClass > api.ObservationQueueClass_OBSERVATION_QUEUE_CLASS_VIEW {
			return false
		}
		bit := uint64(1) << q.QueueClass
		if lags&bit != 0 {
			return false
		}
		lags |= bit
	}
	for _, cut := range v.Cuts {
		bit := uint64(1) << cut.Owner
		if cuts&bit != 0 {
			return false
		}
		cuts |= bit
	}
	const lagClasses = uint64(1)<<api.ObservationQueueClass_OBSERVATION_QUEUE_CLASS_UI | uint64(1)<<api.ObservationQueueClass_OBSERVATION_QUEUE_CLASS_EDGE | uint64(1)<<api.ObservationQueueClass_OBSERVATION_QUEUE_CLASS_RECORDER | uint64(1)<<api.ObservationQueueClass_OBSERVATION_QUEUE_CLASS_VIEW
	return queues == observationDomainMask(api.ObservationQueueClass_OBSERVATION_QUEUE_CLASS_UNSPECIFIED.Descriptor()) && lags == lagClasses
}

func observationV2EventCode(v *api.ObservationRuntimeEvent) bool {
	switch v.Code {
	case api.ObservationLogCode_OBSERVATION_LOG_CODE_PROCESS_CONFIG_READ_FAILED, api.ObservationLogCode_OBSERVATION_LOG_CODE_PROCESS_CONFIG_ADMISSION_FAILED,
		api.ObservationLogCode_OBSERVATION_LOG_CODE_PROCESS_RUNTIME_START_FAILED, api.ObservationLogCode_OBSERVATION_LOG_CODE_PROCESS_RUNTIME_FAILED:
		return v.Component == api.ObservationLogComponent_OBSERVATION_LOG_COMPONENT_PROCESS && v.Level == api.ObservationLogLevel_OBSERVATION_LOG_LEVEL_ERROR
	case api.ObservationLogCode_OBSERVATION_LOG_CODE_PROCESS_READY, api.ObservationLogCode_OBSERVATION_LOG_CODE_PROCESS_STOPPED:
		return v.Component == api.ObservationLogComponent_OBSERVATION_LOG_COMPONENT_PROCESS && v.Level == api.ObservationLogLevel_OBSERVATION_LOG_LEVEL_INFO
	case api.ObservationLogCode_OBSERVATION_LOG_CODE_TRANSPORT_INTERNAL_DIAGNOSTIC:
		return (v.Component == api.ObservationLogComponent_OBSERVATION_LOG_COMPONENT_PUBLIC_TRANSPORT || v.Component == api.ObservationLogComponent_OBSERVATION_LOG_COMPONENT_OPERATOR_TRANSPORT || v.Component == api.ObservationLogComponent_OBSERVATION_LOG_COMPONENT_WORKER_TRANSPORT) && v.Level == api.ObservationLogLevel_OBSERVATION_LOG_LEVEL_WARNING
	case api.ObservationLogCode_OBSERVATION_LOG_CODE_MQTT_UNKNOWN_DISCOVERY, api.ObservationLogCode_OBSERVATION_LOG_CODE_MQTT_INVALID_DISCOVERY, api.ObservationLogCode_OBSERVATION_LOG_CODE_MQTT_UNCONFIGURED_SOURCE,
		api.ObservationLogCode_OBSERVATION_LOG_CODE_MQTT_UNSUPPORTED_EFFECT, api.ObservationLogCode_OBSERVATION_LOG_CODE_MQTT_UNSUPPORTED_COLOR_MODE, api.ObservationLogCode_OBSERVATION_LOG_CODE_MQTT_INVALID_STATE,
		api.ObservationLogCode_OBSERVATION_LOG_CODE_MQTT_INVALID_AVAILABILITY, api.ObservationLogCode_OBSERVATION_LOG_CODE_MQTT_RETIREMENT_FAILED, api.ObservationLogCode_OBSERVATION_LOG_CODE_MQTT_SNAPSHOT_OVERFLOW,
		api.ObservationLogCode_OBSERVATION_LOG_CODE_MQTT_UNCONFIGURED_ACTION_ANCHOR, api.ObservationLogCode_OBSERVATION_LOG_CODE_MQTT_AMBIGUOUS_ACTION_DEVICE, api.ObservationLogCode_OBSERVATION_LOG_CODE_MQTT_ACTION_OVERFLOW, api.ObservationLogCode_OBSERVATION_LOG_CODE_MQTT_INVALID_ACTION:
		return v.Component == api.ObservationLogComponent_OBSERVATION_LOG_COMPONENT_MQTT && v.Level == api.ObservationLogLevel_OBSERVATION_LOG_LEVEL_WARNING
	case api.ObservationLogCode_OBSERVATION_LOG_CODE_PILOT_STARTING, api.ObservationLogCode_OBSERVATION_LOG_CODE_PILOT_RUNNING, api.ObservationLogCode_OBSERVATION_LOG_CODE_PILOT_STOPPED:
		return v.Component == api.ObservationLogComponent_OBSERVATION_LOG_COMPONENT_SUPERVISOR && v.Level == api.ObservationLogLevel_OBSERVATION_LOG_LEVEL_INFO
	case api.ObservationLogCode_OBSERVATION_LOG_CODE_PILOT_RETRYING, api.ObservationLogCode_OBSERVATION_LOG_CODE_PILOT_DEGRADED:
		return v.Component == api.ObservationLogComponent_OBSERVATION_LOG_COMPONENT_SUPERVISOR && v.Level == api.ObservationLogLevel_OBSERVATION_LOG_LEVEL_WARNING
	case api.ObservationLogCode_OBSERVATION_LOG_CODE_PILOT_JOIN_TIMEOUT, api.ObservationLogCode_OBSERVATION_LOG_CODE_PILOT_TASK_CAPACITY, api.ObservationLogCode_OBSERVATION_LOG_CODE_PILOT_FACTORY_FAILED:
		return v.Component == api.ObservationLogComponent_OBSERVATION_LOG_COMPONENT_SUPERVISOR && v.Level == api.ObservationLogLevel_OBSERVATION_LOG_LEVEL_ERROR
	case api.ObservationLogCode_OBSERVATION_LOG_CODE_UNSPECIFIED:
		return false
	default:
		return false
	}
}

func observationV2Sized(m proto.Message, limit int) bool {
	if proto.Size(m) > limit {
		return false
	}
	json, err := protojson.Marshal(m)
	return err == nil && len(json) <= limit
}

func observationV2QueueApplicability(class api.ObservationQueueClass, value *api.ObservationInstalledQueueMetrics) bool {
	inflightNotMeasured := value.InFlightEntries.GetNotMeasured() != nil
	pendingNotMeasured := value.PendingEntries.GetNotMeasured() != nil
	switch class {
	case api.ObservationQueueClass_OBSERVATION_QUEUE_CLASS_WORKER_COMMANDS:
		return !inflightNotMeasured && !pendingNotMeasured && observationV2GaugeLE(value.InFlightEntries, value.PendingEntries)
	case api.ObservationQueueClass_OBSERVATION_QUEUE_CLASS_CLOUD_REQUESTS:
		return !inflightNotMeasured && pendingNotMeasured
	default:
		return inflightNotMeasured && pendingNotMeasured
	}
}
