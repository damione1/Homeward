package contracts

import (
	"bytes"
	"math"
	"testing"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

func nha114TraceRecord() *obs.ControllerTraceRecord {
	return &obs.ControllerTraceRecord{Decision: obs.TraceDecision_TRACE_DECISION_ADMITTED,
		MaxExceeded: &obs.ControllerTraceRecord_NotApplicable{NotApplicable: &emptypb.Empty{}}}
}

func TestNHA114TracePositionPresenceAdmission(t *testing.T) {
	legacy := nha114TraceRecord()
	if err := AdmitObservabilityValue(legacy); err != nil {
		t.Fatal("absent position must preserve existing admission", err)
	}
	for _, sequence := range []uint64{0, 42, math.MaxUint64} {
		for _, decision := range []obs.TraceDecision{obs.TraceDecision_TRACE_DECISION_ADMITTED, obs.TraceDecision_TRACE_DECISION_QUEUED, obs.TraceDecision_TRACE_DECISION_RESTART_PENDING} {
			value := nha114TraceRecord()
			value.Decision = decision
			value.TriggerPosition = &obs.AdmissionPosition{Epoch: bytes.Repeat([]byte{1}, 16), Sequence: sequence}
			if err := AdmitObservabilityValue(value); err != nil {
				t.Fatal("valid source boundary rejected", err)
			}
			encoded, err := proto.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			decoded := &obs.ControllerTraceRecord{}
			if err := proto.Unmarshal(encoded, decoded); err != nil || !proto.Equal(value, decoded) || AdmitObservabilityValue(decoded) != nil {
				t.Fatal("codec changed source epoch or sequence", err)
			}
		}
	}
}

func TestNHA114TracePositionRejectsMalformedAndNoninitial(t *testing.T) {
	for _, epoch := range [][]byte{nil, {}, make([]byte, 16), bytes.Repeat([]byte{1}, 15), bytes.Repeat([]byte{1}, 17), bytes.Repeat([]byte{1}, 513)} {
		value := nha114TraceRecord()
		value.TriggerPosition = &obs.AdmissionPosition{Epoch: epoch, Sequence: 42}
		if AdmitObservabilityValue(value) == nil {
			t.Fatalf("admitted malformed epoch with %d bytes", len(epoch))
		}
	}
	for _, decision := range []obs.TraceDecision{obs.TraceDecision_TRACE_DECISION_FINISHED, obs.TraceDecision_TRACE_DECISION_CANCELLED, obs.TraceDecision_TRACE_DECISION_PREPARING, obs.TraceDecision(999)} {
		value := nha114TraceRecord()
		value.Decision = decision
		value.TriggerPosition = &obs.AdmissionPosition{Epoch: bytes.Repeat([]byte{1}, 16), Sequence: 42}
		if AdmitObservabilityValue(value) == nil {
			t.Fatalf("noninitial decision %v admitted a source position", decision)
		}
	}
	value := nha114TraceRecord()
	value.TriggerPosition = &obs.AdmissionPosition{Epoch: bytes.Repeat([]byte{1}, 16), Sequence: 42}
	value.TriggerPosition.ProtoReflect().SetUnknown([]byte{0x18, 0x01})
	encoded, err := proto.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	decoded := &obs.ControllerTraceRecord{}
	if err := proto.Unmarshal(encoded, decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.TriggerPosition.ProtoReflect().GetUnknown()) == 0 || AdmitObservabilityValue(decoded) == nil {
		t.Fatal("unknown source-position data must survive codec and fail admission")
	}
}
