package contracts

import (
	"bytes"
	"encoding/hex"
	history "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"os"
	"strings"
	"testing"
	"time"
)

func nha164HistoryRequest() *history.QueryObservationsRequest {
	start := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	return &history.QueryObservationsRequest{Parent: "homes/synthetic/entities/nha164-entity", Period: &history.HistoryPeriod{Start: timestamppb.New(start), End: timestamppb.New(start.Add(time.Hour))}}
}

func TestNHA164HistoryGoResponseBytesAndEmptyProgress(t *testing.T) {
	home, _ := apitypes.ParseHomeName("homes/synthetic")
	period := &history.HistoryPeriod{Start: &timestamppb.Timestamp{Seconds: 1}, End: &timestamppb.Timestamp{Seconds: 2}}
	scope := &history.HistoryReadScope{Requested: period, NotRetainedPrefix: proto.CloneOf(period), RetentionCutoff: &timestamppb.Timestamp{Seconds: 2}, AsOf: &timestamppb.Timestamp{Seconds: 3}, Resolution: history.HistoryResolution_HISTORY_RESOLUTION_RAW, Limits: &history.HistoryLimits{MaxPoints: 100, MaxResponseBytes: 256 << 10, MaxTokenBytes: 512, QueryTimeoutMillis: 2000, MaxReaders: 2, MaxPeriodHours: 168}}
	text, err := os.ReadFile("../client/wire/tests/fixtures/nha164_v2_not_retained.hex")
	if err != nil {
		t.Fatal(err)
	}
	golden, err := hex.DecodeString(strings.TrimSpace(string(text)))
	if err != nil {
		t.Fatal(err)
	}
	traceScope := proto.CloneOf(scope)
	traceScope.Limits.MaxPoints = 31
	responses := []proto.Message{&history.QueryObservationsResponse{Scope: scope}, &history.QueryExecutionTracesResponse{Scope: traceScope}}
	expected := [][]byte{golden, bytes.Replace(golden, []byte{0x08, 0x64}, []byte{0x08, 0x1f}, 1)}
	for index, response := range responses {
		if err = AdmitHistoryV2Response(response, home); err != nil {
			t.Fatal(err)
		}
		raw, encodeErr := (proto.MarshalOptions{Deterministic: true}).Marshal(response)
		if encodeErr != nil || !bytes.Equal(raw, expected[index]) {
			t.Fatalf("shared wire oracle: %x %v", raw, encodeErr)
		}
		decoded := response.ProtoReflect().Type().New().Interface()
		if err = proto.Unmarshal(expected[index], decoded); err != nil || AdmitHistoryV2Response(decoded, home) != nil {
			t.Fatal("response roundtrip", err)
		}
		decoded.ProtoReflect().SetUnknown([]byte{0xf8, 0x07, 0x01})
		if AdmitHistoryV2Response(decoded, home) == nil {
			t.Fatal("unknown response field admitted")
		}
	}
	retained := proto.CloneOf(traceScope)
	retained.Retained = proto.CloneOf(period)
	retained.NotRetainedPrefix = nil
	progress := &history.QueryExecutionTracesResponse{Scope: retained, ExecutionEpoch: "opaque-test-selector", NextPageToken: "bounded-physical-progress"}
	if AdmitHistoryV2Response(progress, home) != nil {
		t.Fatal("empty bounded scan rejected")
	}
	progress.NextPageToken = ""
	if AdmitHistoryV2Response(progress, home) != nil {
		t.Fatal("complete empty page rejected")
	}
	progress.NextPageToken = strings.Repeat("x", 513)
	if AdmitHistoryV2Response(progress, home) == nil {
		t.Fatal("oversized continuation admitted")
	}
	progress.NextPageToken = ""
	retained.Resolution = history.HistoryResolution(999)
	if AdmitHistoryV2Response(progress, home) == nil {
		t.Fatal("unknown resolution admitted")
	}
	if AdmitHistoryV2Response(responses[0], home) != nil {
		t.Fatal("independent capture mutated")
	}
}

func TestNHA164HistoryWireRequestPresenceAndRoundTrip(t *testing.T) {
	request := nha164HistoryRequest()
	if _, err := AdmitHistoryV2Selection(request); err != nil {
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
	if _, err = AdmitHistoryV2Selection(decoded); err != nil || decoded.PageSize != 0 {
		t.Fatal("default presence lost", err)
	}
	for _, size := range []int32{0, 1, 101} {
		changed := proto.CloneOf(request)
		changed.PageSize = size
		if _, err := AdmitHistoryV2Selection(changed); err != nil {
			t.Fatalf("default/capped page %d refused", size)
		}
	}
	request.PageSize = -1
	if _, err := AdmitHistoryV2Selection(request); err == nil {
		t.Fatal("negative page admitted")
	}
	request.PageSize = 0
	request.ProtoReflect().SetUnknown([]byte{0xf8, 0x07, 0x01})
	if _, err := AdmitHistoryV2Selection(request); err == nil {
		t.Fatal("future field admitted")
	}
	var zero *history.QueryObservationsRequest
	if _, err := AdmitHistoryV2Selection(zero); err == nil {
		t.Fatal("typed nil admitted")
	}
}

func TestNHA164HistoryResponseRequiresScopeAndPresence(t *testing.T) {
	home, _ := apitypes.ParseHomeName("homes/synthetic")
	for _, response := range []proto.Message{&history.QueryObservationsResponse{}, &history.QueryAggregatesResponse{}, &history.QueryExecutionTracesResponse{}} {
		if AdmitHistoryV2Response(response, home) == nil {
			t.Fatalf("missing scope admitted: %T", response)
		}
	}
	descriptor := &history.HistoryNumericDescriptor{Unit: "historical-W", CanonicalUnit: "W", Scale: 1, Dimension: history.HistoryDimension_HISTORY_DIMENSION_POWER, Precision: -2, Semantic: history.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT, Aggregation: history.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT}
	if !historyV2Descriptor(descriptor) {
		t.Fatal("negative historical precision rejected")
	}
	descriptor.Dimension = history.HistoryDimension(999)
	if historyV2Descriptor(descriptor) {
		t.Fatal("unknown dimension admitted")
	}
}
