package contracts

import (
	"bytes"
	"errors"
	"math"
	"reflect"
	"strings"
	"unicode/utf8"

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	ObservationPageMaxRows   = 100
	ObservationPageMaxBytes  = 256 << 10
	ObservationTokenMaxBytes = 512
	ObservationEventMaxBytes = 1 << 10
)

var ErrInvalidObservabilityContract = errors.New("observability: invalid contract")

func observationNil(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Func, reflect.Map, reflect.Slice, reflect.Chan:
		return r.IsNil()
	default:
		return false
	}
}

func observationText(s string, limit int) bool {
	return s != "" && len(s) <= limit && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n")
}

func observationID(b []byte, size int) bool {
	if len(b) != size {
		return false
	}
	var nonzero byte
	for _, v := range b {
		nonzero |= v
	}
	return nonzero != 0
}

// Reject a typed-nil oneof wrapper before generated reflection accesses it.
func observationNilOneof(m proto.Message) bool {
	r := reflect.ValueOf(m)
	if r.Kind() != reflect.Pointer || r.IsNil() {
		return true
	}
	r = r.Elem()
	if r.Kind() != reflect.Struct {
		return false
	}
	for i := 0; i < r.NumField(); i++ {
		f := r.Field(i)
		if f.Kind() == reflect.Interface && !f.IsNil() {
			v := f.Elem()
			if v.Kind() == reflect.Pointer && v.IsNil() {
				return true
			}
		}
	}
	return false
}

func observationPage(size *uint32, token *string) bool {
	return (size == nil || *size >= 1 && *size <= ObservationPageMaxRows) &&
		(token == nil || observationText(*token, ObservationTokenMaxBytes))
}

func observationListLimit(m proto.Message, name protoreflect.Name) int {
	switch m.(type) {
	case *obs.MetricsSnapshot:
		if name == "queues" {
			return 9
		}
		if name == "queue_lags" {
			return 4
		}
		return 8
	case *obs.HealthSnapshot:
		return 5
	case *obs.ServerHealth, *obs.StoredMetadataInvalid:
		return 6
	case *obs.InstalledRecorderMetrics:
		return 3
	case *obs.ActionMetrics, *obs.InstalledRunMetrics:
		return 2
	case *obs.RecorderCoverageSnapshot:
		if name == "epochs" {
			return 64
		}
		return 512
	default:
		return ObservationPageMaxRows
	}
}

func observationDomainMask(domain protoreflect.EnumDescriptor) uint64 {
	var mask uint64
	for i := 0; i < domain.Values().Len(); i++ {
		n := domain.Values().Get(i).Number()
		if n > 0 && n < 64 {
			mask |= uint64(1) << n
		}
	}
	return mask
}

// All required oneofs and message presence are checked from the sole schema.
// Reflection does not claim compile-time exhaustiveness for Go conversions.
func observationTree(m protoreflect.Message, depth int) bool {
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
			if list.Len() > observationListLimit(m.Interface(), f.Name()) {
				return false
			}
			var enumSeen uint64
			for j := 0; j < list.Len(); j++ {
				v := list.Get(j)
				if f.Kind() == protoreflect.MessageKind && !observationTree(v.Message(), depth+1) {
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
			if !m.Has(f) || !observationTree(m.Get(f).Message(), depth+1) {
				return false
			}
		case protoreflect.EnumKind:
			if n := m.Get(f).Enum(); n == 0 || f.Enum().Values().ByNumber(n) == nil {
				return false
			}
		case protoreflect.StringKind:
			if !observationText(m.Get(f).String(), ObservationTokenMaxBytes) {
				return false
			}
		}
	}
	return observationLocal(m.Interface())
}

func observationLocal(m proto.Message) bool {
	switch v := m.(type) {
	case *timestamppb.Timestamp:
		return v.CheckValid() == nil && !v.AsTime().IsZero()
	case *auth.AccountId:
		return observationText(v.Value, 128)
	case *obs.ProcessObservationEpoch:
		return observationID(v.Value, 16)
	case *obs.ControllerHandle:
		return len(v.Value) == 32 && observationID(v.Value[:16], 16) && observationID(v.Value[16:], 16)
	case *obs.IntegrationReference:
		return observationID(v.Value, 16)
	case *obs.RecorderEpochIdentity:
		return observationID(v.Value, 16)
	case *obs.AdmissionPosition:
		return observationID(v.Epoch, 16)
	case *obs.ActiveRevision:
		return v.Value > 0 && v.Value <= math.MaxInt64
	case *obs.SaturatedCounter:
		return v.Value == math.MaxUint64
	case *obs.KnownLost:
		if n, ok := v.Result.(*obs.KnownLost_LowerBound); ok {
			return n.LowerBound > 0
		}
		return true
	case *obs.QueryRuntimeLogsRequest:
		return observationPage(v.PageSize, v.PageToken)
	case *obs.QueryAuditsRequest:
		return (v.Source == obs.AuditSource_AUDIT_SOURCE_AUTHENTICATION || v.Source == obs.AuditSource_AUDIT_SOURCE_SECRET_ACCESS) && observationPage(v.PageSize, v.PageToken)
	case *obs.ReadRunTraceRequest:
		return v.RunId > 0 && observationPage(v.PageSize, v.PageToken)
	case *obs.GetIntegrationHealthRequest:
		return observationText(v.IntegrationInstanceId, 256)
	case *obs.ReadRecorderCoverageRequest:
		if p := v.GetPosition(); p != nil && bytes.Equal(v.From.Epoch, p.Epoch) {
			return p.Sequence >= v.From.Sequence
		}
		return true
	case *obs.ServerHealth:
		return v.Readiness != obs.ServerReadiness_SERVER_READINESS_READY || v.Generation.GetPresent() != nil && len(v.Causes) == 0
	case *obs.DurableHealth:
		return (v.Availability == obs.DurableAvailability_DURABLE_AVAILABILITY_UNKNOWN) == (v.GetNotObserved() != nil)
	case *obs.PilotPhaseCount:
		return v.Instances <= 1024
	case *obs.SupervisedIntegration:
		return v.Attempts <= 17 && v.Tasks <= 256
	case *obs.HealthSnapshot:
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
		return total <= 1024 && seen == observationDomainMask(obs.PilotPhase_PILOT_PHASE_UNSPECIFIED.Descriptor())
	case *obs.MetricsSnapshot:
		return observationMetrics(v)
	case *obs.QueueAggregate:
		if installed := v.GetInstalled(); installed != nil {
			return observationQueueApplicability(v.QueueClass, installed)
		}
		return true
	case *obs.InstalledQueueMetrics:
		return observationGaugeLE(v.QueuedEntries, v.CapacityEntries) && observationGaugeLE(v.DegradedQueues, v.ActiveQueues) && observationGaugeLE(v.QueuedAccountedBytes, v.CapacityAccountedBytes)
	case *obs.InstalledRecorderMetrics:
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
		return seen == observationDomainMask(obs.GapReason_GAP_REASON_UNSPECIFIED.Descriptor())
	case *obs.ActionMetrics:
		return len(v.FirstAttempts) == 2 && (uint64(1)<<v.FirstAttempts[0].Ledger|uint64(1)<<v.FirstAttempts[1].Ledger) == observationDomainMask(obs.AttemptLedger_ATTEMPT_LEDGER_UNSPECIFIED.Descriptor())
	case *obs.InstalledRunMetrics:
		return len(v.Kinds) == 2 && (uint64(1)<<v.Kinds[0].Kind|uint64(1)<<v.Kinds[1].Kind) == observationDomainMask(obs.RunKind_RUN_KIND_UNSPECIFIED.Descriptor())
	case *obs.ActiveCoverageInterval:
		return bytes.Equal(v.From.Epoch, v.Through.Epoch) && v.From.Sequence > 0 && v.Through.Sequence >= v.From.Sequence &&
			(v.KnownLost.GetLowerBound() == 0 || v.KnownLost.GetLowerBound() <= v.Through.Sequence-v.From.Sequence+1)
	case *obs.RuntimeEvent:
		return observationEventCode(v) && observationSized(v, ObservationEventMaxBytes)
	case *obs.RuntimeLogRow:
		return v.Sequence > 0
	case *obs.PageContinuation:
		return v.GetNextPageToken() == "" || observationText(v.GetNextPageToken(), ObservationTokenMaxBytes)
	case *obs.RuntimeLogPage:
		var last uint64
		for _, row := range v.Rows {
			if row.Sequence <= last || row.Sequence > v.UpperSequence {
				return false
			}
			last = row.Sequence
		}
		return true
	case *obs.CursorEvicted:
		return v.RetainedFrom > 0 && (v.EvictedTotal.GetSaturated() != nil || v.EvictedTotal.GetExact() > 0)
	case *obs.AuditRoute:
		return v.GetRegisteredProcedure() == "" || observationText(v.GetRegisteredProcedure(), 256) && strings.HasPrefix(v.GetRegisteredProcedure(), "/")
	case *obs.StoredMetadataInvalid:
		return len(v.Fields) > 0
	case *obs.AuthenticationAudit:
		return v.Author.GetInternalIntegration() == nil
	case *obs.SecretAccessAudit:
		return v.Author.GetInternalIntegration() != nil
	case *obs.AuditRow:
		return v.Id > 0
	case *obs.AuditPage:
		if v.UpperId < 0 || v.UpperId == 0 && len(v.Rows) != 0 {
			return false
		}
		last := int64(math.MaxInt64)
		for i, row := range v.Rows {
			if row.Id > v.UpperId || i > 0 && row.Id >= last {
				return false
			}
			last = row.Id
			if v.Source == obs.AuditSource_AUDIT_SOURCE_AUTHENTICATION && row.GetSecretAccess() != nil || v.Source == obs.AuditSource_AUDIT_SOURCE_SECRET_ACCESS && row.GetAuthentication() != nil {
				return false
			}
		}
		return true
	case *obs.TraceOrdinal:
		if n, ok := v.Result.(*obs.TraceOrdinal_Native); ok {
			return n.Native > 0 && n.Native < math.MaxUint64
		}
		return true
	case *obs.TraceRow:
		return v.RunId > 0 && v.LocalIndex < 512
	case *obs.TracePage:
		if v.RunId == 0 {
			return false
		}
		var last uint32
		for i, row := range v.Rows {
			if row.RunId != v.RunId || i > 0 && row.LocalIndex <= last || row.Ordinal.GetExhausted() != nil && v.Cuts.Order == obs.TraceOrderState_TRACE_ORDER_STATE_HEALTHY {
				return false
			}
			last = row.LocalIndex
		}
		return true
	case *obs.RecorderEpochCoverage:
		return !v.CleanClose || v.GetClosedAt() != nil
	case *obs.RecorderCoverageGap:
		return v.FromSequence > 0 && v.ThroughSequence >= v.FromSequence &&
			(v.KnownLost.GetLowerBound() == 0 || v.KnownLost.GetLowerBound() <= v.ThroughSequence-v.FromSequence+1) &&
			(v.Envelope != obs.GapEnvelope_GAP_ENVELOPE_EXACT_POSITIONS || v.KnownLost.GetLowerBound() == v.ThroughSequence-v.FromSequence+1)
	case *obs.RecorderCoverageSnapshot:
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

func observationGaugeLE(left, right *obs.GaugeMeasurement) bool {
	l, lok := left.GetResult().(*obs.GaugeMeasurement_Value)
	r, rok := right.GetResult().(*obs.GaugeMeasurement_Value)
	return !lok || !rok || l.Value <= r.Value
}

func observationMetrics(v *obs.MetricsSnapshot) bool {
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
		if q.QueueClass > obs.QueueClass_QUEUE_CLASS_VIEW {
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
	const lagClasses = uint64(1)<<obs.QueueClass_QUEUE_CLASS_UI | uint64(1)<<obs.QueueClass_QUEUE_CLASS_EDGE | uint64(1)<<obs.QueueClass_QUEUE_CLASS_RECORDER | uint64(1)<<obs.QueueClass_QUEUE_CLASS_VIEW
	return queues == observationDomainMask(obs.QueueClass_QUEUE_CLASS_UNSPECIFIED.Descriptor()) && lags == lagClasses
}

func observationEventCode(v *obs.RuntimeEvent) bool {
	switch v.Code {
	case obs.LogCode_LOG_CODE_PROCESS_CONFIG_READ_FAILED, obs.LogCode_LOG_CODE_PROCESS_CONFIG_ADMISSION_FAILED,
		obs.LogCode_LOG_CODE_PROCESS_RUNTIME_START_FAILED, obs.LogCode_LOG_CODE_PROCESS_RUNTIME_FAILED:
		return v.Component == obs.LogComponent_LOG_COMPONENT_PROCESS && v.Level == obs.LogLevel_LOG_LEVEL_ERROR
	case obs.LogCode_LOG_CODE_PROCESS_READY, obs.LogCode_LOG_CODE_PROCESS_STOPPED:
		return v.Component == obs.LogComponent_LOG_COMPONENT_PROCESS && v.Level == obs.LogLevel_LOG_LEVEL_INFO
	case obs.LogCode_LOG_CODE_TRANSPORT_INTERNAL_DIAGNOSTIC:
		return (v.Component == obs.LogComponent_LOG_COMPONENT_PUBLIC_TRANSPORT || v.Component == obs.LogComponent_LOG_COMPONENT_OPERATOR_TRANSPORT || v.Component == obs.LogComponent_LOG_COMPONENT_WORKER_TRANSPORT) && v.Level == obs.LogLevel_LOG_LEVEL_WARNING
	case obs.LogCode_LOG_CODE_MQTT_UNKNOWN_DISCOVERY, obs.LogCode_LOG_CODE_MQTT_INVALID_DISCOVERY, obs.LogCode_LOG_CODE_MQTT_UNCONFIGURED_SOURCE,
		obs.LogCode_LOG_CODE_MQTT_UNSUPPORTED_EFFECT, obs.LogCode_LOG_CODE_MQTT_UNSUPPORTED_COLOR_MODE, obs.LogCode_LOG_CODE_MQTT_INVALID_STATE,
		obs.LogCode_LOG_CODE_MQTT_INVALID_AVAILABILITY, obs.LogCode_LOG_CODE_MQTT_RETIREMENT_FAILED, obs.LogCode_LOG_CODE_MQTT_SNAPSHOT_OVERFLOW,
		obs.LogCode_LOG_CODE_MQTT_UNCONFIGURED_ACTION_ANCHOR, obs.LogCode_LOG_CODE_MQTT_AMBIGUOUS_ACTION_DEVICE, obs.LogCode_LOG_CODE_MQTT_ACTION_OVERFLOW, obs.LogCode_LOG_CODE_MQTT_INVALID_ACTION:
		return v.Component == obs.LogComponent_LOG_COMPONENT_MQTT && v.Level == obs.LogLevel_LOG_LEVEL_WARNING
	case obs.LogCode_LOG_CODE_PILOT_STARTING, obs.LogCode_LOG_CODE_PILOT_RUNNING, obs.LogCode_LOG_CODE_PILOT_STOPPED:
		return v.Component == obs.LogComponent_LOG_COMPONENT_SUPERVISOR && v.Level == obs.LogLevel_LOG_LEVEL_INFO
	case obs.LogCode_LOG_CODE_PILOT_RETRYING, obs.LogCode_LOG_CODE_PILOT_DEGRADED:
		return v.Component == obs.LogComponent_LOG_COMPONENT_SUPERVISOR && v.Level == obs.LogLevel_LOG_LEVEL_WARNING
	case obs.LogCode_LOG_CODE_PILOT_JOIN_TIMEOUT, obs.LogCode_LOG_CODE_PILOT_TASK_CAPACITY, obs.LogCode_LOG_CODE_PILOT_FACTORY_FAILED:
		return v.Component == obs.LogComponent_LOG_COMPONENT_SUPERVISOR && v.Level == obs.LogLevel_LOG_LEVEL_ERROR
	case obs.LogCode_LOG_CODE_UNSPECIFIED:
		return false
	default:
		return false
	}
}

func observationSized(m proto.Message, limit int) bool {
	if proto.Size(m) > limit {
		return false
	}
	json, err := protojson.Marshal(m)
	return err == nil && len(json) <= limit
}

func AdmitObservabilityValue(m proto.Message) error {
	if observationNil(m) || observationNilOneof(m) || !observationTree(m.ProtoReflect(), 0) {
		return ErrInvalidObservabilityContract
	}
	if m.ProtoReflect().Descriptor().ParentFile().Package() != obs.File_home_observability_v1_observability_proto.Package() {
		return ErrInvalidObservabilityContract
	}
	return nil
}

func AdmitObservabilityRequest(m proto.Message) error {
	switch m.(type) {
	case *obs.GetHealthRequest, *obs.GetMetricsRequest, *obs.GetIntegrationHealthRequest,
		*obs.QueryRuntimeLogsRequest, *obs.QueryAuditsRequest, *obs.ReadRunTraceRequest, *obs.ReadRecorderCoverageRequest:
		if err := AdmitObservabilityValue(m); err != nil {
			return err
		}
		if !observationSized(m, 2048) {
			return ErrInvalidObservabilityContract
		}
		return nil
	default:
		return ErrInvalidObservabilityContract
	}
}

func AdmitObservabilityResponse(m proto.Message) error {
	limit := ObservationPageMaxBytes
	switch m.(type) {
	case *obs.GetHealthResponse:
		limit = 16 << 10
	case *obs.GetMetricsResponse:
		limit = 32 << 10
	case *obs.GetIntegrationHealthResponse:
		limit = 8 << 10
	case *obs.QueryRuntimeLogsResponse, *obs.QueryAuditsResponse, *obs.ReadRunTraceResponse, *obs.ReadRecorderCoverageResponse:
	default:
		return ErrInvalidObservabilityContract
	}
	if err := AdmitObservabilityValue(m); err != nil {
		return err
	}
	if !observationSized(m, limit) {
		return ErrInvalidObservabilityContract
	}
	return nil
}

func observationQueueApplicability(class obs.QueueClass, value *obs.InstalledQueueMetrics) bool {
	inflightNotMeasured := value.InFlightEntries.GetNotMeasured() != nil
	pendingNotMeasured := value.PendingEntries.GetNotMeasured() != nil
	switch class {
	case obs.QueueClass_QUEUE_CLASS_WORKER_COMMANDS:
		return !inflightNotMeasured && !pendingNotMeasured && observationGaugeLE(value.InFlightEntries, value.PendingEntries)
	case obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS:
		return !inflightNotMeasured && pendingNotMeasured
	default:
		return inflightNotMeasured && pendingNotMeasured
	}
}
