package contracts

import (
	"bytes"
	"encoding/hex"
	history "github.com/damione1/Homeward/gen/home/history/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"os"
	"strings"
	"testing"
	"time"
)

func nha164HistoryRequest() *history.QueryObservationsRequest {
	start := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	return &history.QueryObservationsRequest{EntityId: "nha164-entity", Period: &history.Period{Start: timestamppb.New(start), End: timestamppb.New(start.Add(time.Hour))}, Resolution: history.Resolution_RESOLUTION_RAW}
}

func TestNHA164HistoryGoResponseBytesAndEmptyProgress(t *testing.T) {
	scope := &history.ReadScope{Requested: &history.Period{Start: &timestamppb.Timestamp{Seconds: 1}, End: &timestamppb.Timestamp{Seconds: 2}}, RetentionCutoff: &timestamppb.Timestamp{Seconds: 2}, AsOf: &timestamppb.Timestamp{Seconds: 3}, Resolution: history.Resolution_RESOLUTION_RAW, Limits: &history.Limits{MaxPoints: 100, MaxResponseBytes: 256 << 10, MaxTokenBytes: 512, QueryTimeoutMillis: 2000, MaxReaders: 2, MaxPeriodHours: 168}}
	text, err := os.ReadFile("../client/wire/tests/fixtures/nha164_not_retained.hex")
	if err != nil {
		t.Fatal(err)
	}
	golden, err := hex.DecodeString(strings.TrimSpace(string(text)))
	if err != nil {
		t.Fatal(err)
	}
	traceScope := proto.Clone(scope).(*history.ReadScope)
	traceScope.Limits.MaxPoints = 31
	responses := []proto.Message{&history.QueryObservationsResponse{Result: &history.QueryObservationsResponse_NotRetained{NotRetained: scope}}, &history.QueryAggregatesResponse{Result: &history.QueryAggregatesResponse_Budget{Budget: &history.BudgetExceeded{Reason: history.BudgetReason_BUDGET_REASON_POINT_TOO_LARGE}}}, &history.QueryExecutionTracesResponse{Result: &history.QueryExecutionTracesResponse_NotRetained{NotRetained: traceScope}}}
	expected := [][]byte{golden, {0x22, 0x02, 0x08, 0x01}, bytes.Replace(golden, []byte{0x08, 0x64}, []byte{0x08, 0x1f}, 1)}
	for index, response := range responses {
		if err = AdmitHistoryResponse(response); err != nil {
			t.Fatal(err)
		}
		raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(response)
		if err != nil || !bytes.Equal(raw, expected[index]) {
			t.Fatalf("Go response no longer matches shared Rust wire oracle: %x %v", raw, err)
		}
		decoded := response.ProtoReflect().Type().New().Interface()
		if err = proto.Unmarshal(expected[index], decoded); err != nil || AdmitHistoryResponse(decoded) != nil {
			t.Fatalf("response roundtrip: %v", err)
		}
		decoded.ProtoReflect().SetUnknown([]byte{0xf8, 0x07, 0x01})
		if AdmitHistoryResponse(decoded) == nil {
			t.Fatal("unknown response field admitted")
		}
	}
	scope = proto.Clone(scope).(*history.ReadScope)
	scope.Limits.MaxPoints = 31
	page := &history.ExecutionTracePage{Scope: scope, Epoch: "00000000000000000000000000000164", Continuation: &history.Continuation{Result: &history.Continuation_NextPageToken{NextPageToken: "bounded-physical-progress"}}}
	progress := &history.QueryExecutionTracesResponse{Result: &history.QueryExecutionTracesResponse_Page{Page: page}}
	if AdmitHistoryResponse(progress) != nil {
		t.Fatal("empty progression was confused with complete absence")
	}
	page.Continuation = &history.Continuation{Result: &history.Continuation_End{End: &emptypb.Empty{}}}
	if AdmitHistoryResponse(progress) != nil {
		t.Fatal("complete empty trace page refused")
	}
	page.Continuation = nil
	if AdmitHistoryResponse(progress) == nil {
		t.Fatal("missing completion/progression admitted")
	}
	scope.Resolution = history.Resolution(999)
	if AdmitHistoryResponse(responses[0]) != nil {
		t.Fatal("source response unexpectedly mutated")
	}
	page.Continuation = &history.Continuation{Result: &history.Continuation_End{End: &emptypb.Empty{}}}
	if AdmitHistoryResponse(progress) == nil {
		t.Fatal("unknown response resolution admitted")
	}
}
func TestNHA164HistoryWireRequestPresenceAndRoundTrip(t *testing.T) {
	request := nha164HistoryRequest()
	if _, _, err := AdmitHistoryRequest(request); err != nil {
		t.Fatal(err)
	}
	raw, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	decoded := &history.QueryObservationsRequest{}
	if err = proto.Unmarshal(raw, decoded); err != nil {
		t.Fatal(err)
	}
	if _, _, err = AdmitHistoryRequest(decoded); err != nil || decoded.PageSize != nil {
		t.Fatalf("absence lost: %v", err)
	}
	for _, size := range []uint32{0, 101} {
		changed := proto.Clone(request).(*history.QueryObservationsRequest)
		changed.PageSize = proto.Uint32(size)
		if _, _, err := AdmitHistoryRequest(changed); err == nil {
			t.Fatalf("invalid page %d admitted", size)
		}
	}
	for _, resolution := range []history.Resolution{history.Resolution_RESOLUTION_UNSPECIFIED, history.Resolution_RESOLUTION_HOUR, history.Resolution(999)} {
		changed := proto.Clone(request).(*history.QueryObservationsRequest)
		changed.Resolution = resolution
		if _, _, err := AdmitHistoryRequest(changed); err == nil {
			t.Fatalf("resolution %v admitted", resolution)
		}
	}
	request.ProtoReflect().SetUnknown([]byte{0xf8, 0x07, 0x01})
	if _, _, err := AdmitHistoryRequest(request); err == nil {
		t.Fatal("future field admitted operationally")
	}
	var zero *history.QueryObservationsRequest
	if _, _, err := AdmitHistoryRequest(zero); err == nil {
		t.Fatal("typed nil request admitted")
	}
}
func TestNHA164HistoryResponseRequiresScopeAndPresence(t *testing.T) {
	for _, response := range []proto.Message{&history.QueryObservationsResponse{}, &history.QueryAggregatesResponse{}, &history.QueryExecutionTracesResponse{}, &history.QueryObservationsResponse{Result: &history.QueryObservationsResponse_Page{Page: &history.ObservationPage{}}}} {
		if err := AdmitHistoryResponse(response); err == nil {
			t.Fatalf("zero or incomplete response admitted: %T", response)
		}
	}
	descriptor := &history.NumericDescriptor{Unit: "historical-W", CanonicalUnit: "W", Scale: 1, Dimension: history.Dimension_DIMENSION_POWER, Precision: -2, Semantic: 1, Aggregation: 1}
	if !historyDescriptorValid(descriptor) {
		t.Fatal("negative historical precision rejected")
	}
	descriptor.Dimension = history.Dimension(999)
	if historyDescriptorValid(descriptor) {
		t.Fatal("unknown historical dimension admitted")
	}
}
