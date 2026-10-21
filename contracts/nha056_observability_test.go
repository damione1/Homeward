package contracts

import (
	"math"
	"strings"
	"testing"
	"time"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestNHA056WireAdmissionPresenceBoundsAndUnknowns(t *testing.T) {
	var typedNil *obs.QueryRuntimeLogsRequest
	for _, in := range []proto.Message{nil, typedNil,
		&obs.QueryRuntimeLogsRequest{PageSize: proto.Uint32(0)},
		&obs.QueryRuntimeLogsRequest{PageSize: proto.Uint32(101)},
		&obs.QueryRuntimeLogsRequest{PageToken: proto.String(strings.Repeat("a", 513))},
		&obs.QueryAuditsRequest{},
		&obs.QueryAuditsRequest{Source: obs.AuditSource(999)},
		&obs.ReadRunTraceRequest{},
	} {
		if AdmitObservabilityRequest(in) == nil {
			t.Errorf("admitted invalid request %T", in)
		}
	}
	for _, in := range []proto.Message{&obs.GetHealthRequest{}, &obs.GetMetricsRequest{},
		&obs.QueryRuntimeLogsRequest{},
		&obs.QueryAuditsRequest{Source: obs.AuditSource_AUDIT_SOURCE_AUTHENTICATION, PageSize: proto.Uint32(100)},
	} {
		if err := AdmitObservabilityRequest(in); err != nil {
			t.Errorf("valid %T rejected: %v", in, err)
		}
	}
	future := &obs.GetHealthRequest{}
	future.ProtoReflect().SetUnknown([]byte{0x08, 0x01})
	if AdmitObservabilityRequest(future) == nil || len(future.ProtoReflect().GetUnknown()) == 0 {
		t.Fatal("unknown wire data must be retained by codec and refused in admission")
	}
}

func TestNHA056EveryLogCodeHasAnExplicitSemanticAdmission(t *testing.T) {
	descriptor := obs.LogCode_LOG_CODE_UNSPECIFIED.Descriptor()
	for i := 0; i < descriptor.Values().Len(); i++ {
		code := obs.LogCode(descriptor.Values().Get(i).Number())
		accepted := 0
		for _, component := range []obs.LogComponent{obs.LogComponent_LOG_COMPONENT_PROCESS, obs.LogComponent_LOG_COMPONENT_PUBLIC_TRANSPORT, obs.LogComponent_LOG_COMPONENT_OPERATOR_TRANSPORT, obs.LogComponent_LOG_COMPONENT_WORKER_TRANSPORT, obs.LogComponent_LOG_COMPONENT_MQTT, obs.LogComponent_LOG_COMPONENT_SUPERVISOR} {
			for _, level := range []obs.LogLevel{obs.LogLevel_LOG_LEVEL_INFO, obs.LogLevel_LOG_LEVEL_WARNING, obs.LogLevel_LOG_LEVEL_ERROR} {
				value := &obs.RuntimeEvent{Code: code, Component: component, Level: level, OccurredAt: timestamppb.New(time.Now().UTC()), Author: &obs.RuntimeAuthor{Result: &obs.RuntimeAuthor_Process{Process: &emptypb.Empty{}}}}
				if AdmitObservabilityValue(value) == nil {
					accepted++
				}
			}
		}
		if code == obs.LogCode_LOG_CODE_UNSPECIFIED {
			if accepted != 0 {
				t.Fatal("zero code admitted")
			}
		} else if accepted == 0 {
			t.Fatalf("log code %v has no explicit conversion", code)
		}
	}
}

func TestNHA056WireCounterAndOneofAdmission(t *testing.T) {
	for _, in := range []proto.Message{&obs.CounterMeasurement{},
		&obs.CounterMeasurement{Result: &obs.CounterMeasurement_Saturated{Saturated: &obs.SaturatedCounter{Value: 2}}},
		&obs.CounterMeasurement{Result: (*obs.CounterMeasurement_Saturated)(nil)},
		&obs.GenerationPresence{},
		&obs.TraceOrdinal{Result: &obs.TraceOrdinal_Native{Native: math.MaxUint64}},
	} {
		if AdmitObservabilityValue(in) == nil {
			t.Errorf("admitted invalid value %T", in)
		}
	}
	for _, in := range []proto.Message{
		&obs.CounterMeasurement{Result: &obs.CounterMeasurement_Exact{Exact: 0}},
		&obs.CounterMeasurement{Result: &obs.CounterMeasurement_Exact{Exact: math.MaxUint64}},
		&obs.CounterMeasurement{Result: &obs.CounterMeasurement_Saturated{Saturated: &obs.SaturatedCounter{Value: math.MaxUint64}}},
		&obs.GenerationPresence{Result: &obs.GenerationPresence_NotLoaded{NotLoaded: &emptypb.Empty{}}},
	} {
		if err := AdmitObservabilityValue(in); err != nil {
			t.Errorf("valid %T rejected: %v", in, err)
		}
	}
}

// Added after the first response admission implementation, with the same
// representative values and business invariants as the Rust response oracles.
func nha056RuntimeResponse() *obs.QueryRuntimeLogsResponse {
	return &obs.QueryRuntimeLogsResponse{Result: &obs.QueryRuntimeLogsResponse_Page{Page: &obs.RuntimeLogPage{
		ProcessEpoch: &obs.ProcessObservationEpoch{Value: []byte{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}}, UpperSequence: 1,
		Rows: []*obs.RuntimeLogRow{{Sequence: 1, Event: &obs.RuntimeEvent{
			Code: obs.LogCode_LOG_CODE_PROCESS_READY, Level: obs.LogLevel_LOG_LEVEL_INFO, Component: obs.LogComponent_LOG_COMPONENT_PROCESS,
			OccurredAt: timestamppb.New(time.Unix(1_792_314_000, 0).UTC()), Author: &obs.RuntimeAuthor{Result: &obs.RuntimeAuthor_Process{Process: &emptypb.Empty{}}},
		}}}, Continuation: &obs.PageContinuation{Result: &obs.PageContinuation_End{End: &emptypb.Empty{}}},
	}}}
}

func TestNHA056ResponsePresenceAndNestedFutureValues(t *testing.T) {
	valid := nha056RuntimeResponse()
	if err := AdmitObservabilityResponse(valid); err != nil {
		t.Fatal("valid runtime page", err)
	}
	missingTime := proto.Clone(valid).(*obs.QueryRuntimeLogsResponse)
	missingTime.GetPage().Rows[0].Event.OccurredAt = nil
	missingAuthorKind := proto.Clone(valid).(*obs.QueryRuntimeLogsResponse)
	missingAuthorKind.GetPage().Rows[0].Event.Author.Result = nil
	for _, invalid := range []*obs.QueryRuntimeLogsResponse{missingTime, missingAuthorKind} {
		if AdmitObservabilityResponse(invalid) == nil {
			t.Fatal("missing nested presence admitted")
		}
	}
	futureEnum := proto.Clone(valid).(*obs.QueryRuntimeLogsResponse)
	futureEnum.GetPage().Rows[0].Event.Level = obs.LogLevel(999)
	encoded, err := proto.Marshal(futureEnum)
	if err != nil {
		t.Fatal(err)
	}
	decoded := &obs.QueryRuntimeLogsResponse{}
	if err := proto.Unmarshal(encoded, decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.GetPage().Rows[0].Event.Level != obs.LogLevel(999) || AdmitObservabilityResponse(decoded) == nil {
		t.Fatal("future enum was lost or admitted")
	}
	unknownAuthor := valid.GetPage().Rows[0].Event.Author
	encoded, err = proto.Marshal(unknownAuthor)
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, 0xa0, 0x06, 0x01)
	if err := proto.Unmarshal(encoded, unknownAuthor); err != nil {
		t.Fatal(err)
	}
	encoded, err = proto.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(encoded, decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.GetPage().Rows[0].Event.Author.ProtoReflect().GetUnknown()) == 0 || AdmitObservabilityResponse(decoded) == nil {
		t.Fatal("nested future field was lost or admitted")
	}
}

func TestNHA056TraceResponseRunAndOrdinalCutAgree(t *testing.T) {
	at := timestamppb.New(time.Unix(1_792_314_000, 0).UTC())
	response := &obs.ReadRunTraceResponse{Result: &obs.ReadRunTraceResponse_Page{Page: &obs.TracePage{
		Controller: &obs.ControllerHandle{Value: []byte{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}}, RunId: 7,
		Cuts:         &obs.TraceCutMetadata{Coherence: obs.TraceCutCoherence_TRACE_CUT_COHERENCE_SEPARATE_OWNER_CUTS, Order: obs.TraceOrderState_TRACE_ORDER_STATE_HEALTHY, ControllerCapturedAt: at, NodeCapturedAt: timestamppb.New(time.Unix(1_792_314_001, 0).UTC())},
		Rows:         []*obs.TraceRow{{RunId: 7, LocalIndex: 0, Ordinal: &obs.TraceOrdinal{Result: &obs.TraceOrdinal_Native{Native: 1}}, Record: &obs.TraceRow_Controller{Controller: &obs.ControllerTraceRecord{Decision: obs.TraceDecision_TRACE_DECISION_FINISHED, MaxExceeded: &obs.ControllerTraceRecord_NotApplicable{NotApplicable: &emptypb.Empty{}}}}}},
		Continuation: &obs.PageContinuation{Result: &obs.PageContinuation_End{End: &emptypb.Empty{}}},
	}}}
	if err := AdmitObservabilityResponse(response); err != nil {
		t.Fatal("valid trace page", err)
	}
	row := response.GetPage().Rows[0]
	row.RunId = 8
	if AdmitObservabilityResponse(response) == nil {
		t.Fatal("other run admitted")
	}
	row.RunId = 7
	row.Ordinal.Result = &obs.TraceOrdinal_Exhausted{Exhausted: &emptypb.Empty{}}
	if AdmitObservabilityResponse(response) == nil {
		t.Fatal("exhausted ordinal with healthy cut admitted")
	}
	response.GetPage().Cuts.Order = obs.TraceOrderState_TRACE_ORDER_STATE_EXHAUSTED
	if err := AdmitObservabilityResponse(response); err != nil {
		t.Fatal("explicit exhausted cut rejected", err)
	}
}
