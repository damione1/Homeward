package observability

import (
	"github.com/damione1/Homeward/contracts"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/emptypb"
)

type MetricSlot struct {
	field  protoreflect.FieldDescriptor
	queue  obs.QueueClass
	ledger obs.AttemptLedger
	run    obs.RunKind
	gap    obs.GapReason
}

func (s MetricSlot) Descriptor() protoreflect.FieldDescriptor { return s.field }
func (s MetricSlot) QueueClass() (obs.QueueClass, bool)       { return s.queue, s.queue != 0 }
func (s MetricSlot) AttemptLedger() (obs.AttemptLedger, bool) { return s.ledger, s.ledger != 0 }
func (s MetricSlot) RunKind() (obs.RunKind, bool)             { return s.run, s.run != 0 }
func (s MetricSlot) GapReason() (obs.GapReason, bool)         { return s.gap, s.gap != 0 }

// This is a finite catalogue of schema field identities, not a metric registry.
// There is no registration method, arbitrary name, label map, or producer ID.
func MetricCatalogue() []MetricSlot {
	result := make([]MetricSlot, 0, 124)
	queues := (&obs.InstalledQueueMetrics{}).ProtoReflect().Descriptor().Fields()
	for _, q := range []obs.QueueClass{obs.QueueClass_QUEUE_CLASS_UI, obs.QueueClass_QUEUE_CLASS_EDGE, obs.QueueClass_QUEUE_CLASS_RECORDER, obs.QueueClass_QUEUE_CLASS_VIEW, obs.QueueClass_QUEUE_CLASS_AUTOMATION_PENDING, obs.QueueClass_QUEUE_CLASS_SCRIPT_PENDING, obs.QueueClass_QUEUE_CLASS_MQTT_INBOUND, obs.QueueClass_QUEUE_CLASS_WORKER_COMMANDS, obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS} {
		for n := protoreflect.FieldNumber(1); n <= 9; n++ {
			result = append(result, MetricSlot{field: queues.ByNumber(n), queue: q})
		}
	}
	result = append(result, MetricSlot{field: queues.ByNumber(10), queue: obs.QueueClass_QUEUE_CLASS_WORKER_COMMANDS}, MetricSlot{field: queues.ByNumber(11), queue: obs.QueueClass_QUEUE_CLASS_WORKER_COMMANDS}, MetricSlot{field: queues.ByNumber(10), queue: obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS})
	lag := (&obs.LagMeasurement{}).ProtoReflect().Descriptor().Fields().ByNumber(1)
	for _, q := range []obs.QueueClass{obs.QueueClass_QUEUE_CLASS_UI, obs.QueueClass_QUEUE_CLASS_EDGE, obs.QueueClass_QUEUE_CLASS_RECORDER, obs.QueueClass_QUEUE_CLASS_VIEW} {
		result = append(result, MetricSlot{field: lag, queue: q})
	}
	recorder := (&obs.InstalledRecorderMetrics{}).ProtoReflect().Descriptor().Fields()
	for n := protoreflect.FieldNumber(1); n <= 11; n++ {
		result = append(result, MetricSlot{field: recorder.ByNumber(n)})
	}
	gaps := (&obs.CommittedGapMetrics{}).ProtoReflect().Descriptor().Fields()
	for _, reason := range []obs.GapReason{obs.GapReason_GAP_REASON_OVERFLOW, obs.GapReason_GAP_REASON_RECONCILIATION, obs.GapReason_GAP_REASON_ABANDONED} {
		for n := protoreflect.FieldNumber(2); n <= 4; n++ {
			result = append(result, MetricSlot{field: gaps.ByNumber(n), gap: reason})
		}
	}
	actions := (&obs.ActionMetrics{}).ProtoReflect().Descriptor().Fields()
	result = append(result, MetricSlot{field: actions.ByNumber(1)}, MetricSlot{field: actions.ByNumber(3)})
	first := (&obs.FirstAttemptMetrics{}).ProtoReflect().Descriptor().Fields().ByNumber(2)
	for _, ledger := range []obs.AttemptLedger{obs.AttemptLedger_ATTEMPT_LEDGER_VOLATILE, obs.AttemptLedger_ATTEMPT_LEDGER_DURABLE} {
		result = append(result, MetricSlot{field: first, ledger: ledger})
	}
	runs := (&obs.RunKindMetrics{}).ProtoReflect().Descriptor().Fields()
	for _, kind := range []obs.RunKind{obs.RunKind_RUN_KIND_AUTOMATION, obs.RunKind_RUN_KIND_SCRIPT} {
		for n := protoreflect.FieldNumber(2); n <= 5; n++ {
			result = append(result, MetricSlot{field: runs.ByNumber(n), run: kind})
		}
	}
	result = append(result, MetricSlot{field: (&obs.InstalledRunMetrics{}).ProtoReflect().Descriptor().Fields().ByNumber(2)})
	journal := (&obs.JournalMetrics{}).ProtoReflect().Descriptor().Fields()
	for n := protoreflect.FieldNumber(1); n <= 3; n++ {
		result = append(result, MetricSlot{field: journal.ByNumber(n)})
	}
	return result
}

func gauge(value uint64) *obs.GaugeMeasurement {
	return &obs.GaugeMeasurement{Result: &obs.GaugeMeasurement_Value{Value: value}}
}
func unmeasuredGauge() *obs.GaugeMeasurement {
	return &obs.GaugeMeasurement{Result: &obs.GaugeMeasurement_NotMeasured{NotMeasured: &emptypb.Empty{}}}
}

type QueueSnapshot struct{ wire *obs.InstalledQueueMetrics }

// Nil byte counts explicitly mean NotMeasured, including for installed empty
// queues. A present zero means zero accounted bytes. Owners choose applicable
// cumulative counters on the returned independent wire copy.
func NewQueueSnapshot(active, queued, capacity, degraded uint64, queuedBytes, capacityBytes *uint64) (QueueSnapshot, error) {
	if queued > capacity || degraded > active || (queuedBytes == nil) != (capacityBytes == nil) || queuedBytes != nil && *queuedBytes > *capacityBytes {
		return QueueSnapshot{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_METRICS)
	}
	wire := &obs.InstalledQueueMetrics{ActiveQueues: gauge(active), QueuedEntries: gauge(queued), CapacityEntries: gauge(capacity), DegradedQueues: gauge(degraded), QueuedAccountedBytes: unmeasuredGauge(), CapacityAccountedBytes: unmeasuredGauge(), RejectedTotal: unmeasuredCounter(), LostDeliveriesTotal: unmeasuredCounter(), CoalescedTotal: unmeasuredCounter(), InFlightEntries: unmeasuredGauge(), PendingEntries: unmeasuredGauge()}
	if queuedBytes != nil {
		wire.QueuedAccountedBytes = gauge(*queuedBytes)
		wire.CapacityAccountedBytes = gauge(*capacityBytes)
	}
	return QueueSnapshot{wire: wire}, nil
}
func (s QueueSnapshot) Wire() (*obs.InstalledQueueMetrics, error) {
	if s.wire == nil || contracts.AdmitObservabilityValue(s.wire) != nil {
		return nil, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_METRICS)
	}
	return proto.Clone(s.wire).(*obs.InstalledQueueMetrics), nil
}
func SameEpochLag(positions uint64) *obs.LagMeasurement {
	return &obs.LagMeasurement{Result: &obs.LagMeasurement_SameEpochPositions{SameEpochPositions: positions}}
}
func InvalidatedLag() *obs.LagMeasurement {
	return &obs.LagMeasurement{Result: &obs.LagMeasurement_Invalidated{Invalidated: &emptypb.Empty{}}}
}
func EpochMismatchLag() *obs.LagMeasurement {
	return &obs.LagMeasurement{Result: &obs.LagMeasurement_EpochMismatch{EpochMismatch: &emptypb.Empty{}}}
}
