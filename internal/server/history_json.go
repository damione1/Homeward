package server

import (
	"github.com/damione1/Homeward/contracts"
	history "github.com/damione1/Homeward/gen/home/history/v1"
	"github.com/damione1/Homeward/internal/historyquery"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Local to164. Connect turns a Codec.Marshal error into Internal, so an
// oversized JSON encoding becomes the RPC's explicit, small Budget variant.
// Binary protobuf admission and every other service's codec remain unchanged.
type historyJSONCodec struct{ name string }

func (c historyJSONCodec) Name() string { return c.name }
func (c historyJSONCodec) Marshal(value any) ([]byte, error) {
	message, ok := value.(proto.Message)
	if !ok {
		return nil, historyRPCError(historyquery.ErrIntegrity)
	}
	encoded, err := protojson.Marshal(message)
	if err != nil {
		return nil, err
	}
	if len(encoded) <= historyquery.MaxResponseBytes {
		return encoded, nil
	}
	reason := &history.BudgetExceeded{Reason: history.BudgetReason_BUDGET_REASON_CAPACITY}
	var budget proto.Message
	switch message.(type) {
	case *history.QueryObservationsResponse:
		budget = &history.QueryObservationsResponse{Result: &history.QueryObservationsResponse_Budget{Budget: reason}}
	case *history.QueryAggregatesResponse:
		budget = &history.QueryAggregatesResponse{Result: &history.QueryAggregatesResponse_Budget{Budget: reason}}
	case *history.QueryExecutionTracesResponse:
		budget = &history.QueryExecutionTracesResponse{Result: &history.QueryExecutionTracesResponse_Budget{Budget: reason}}
	default:
		return nil, historyRPCError(historyquery.ErrIntegrity)
	}
	if contracts.AdmitHistoryResponse(budget) != nil {
		return nil, historyRPCError(historyquery.ErrIntegrity)
	}
	return protojson.Marshal(budget)
}
func (c historyJSONCodec) Unmarshal(encoded []byte, value any) error {
	message, ok := value.(proto.Message)
	if !ok {
		return historyRPCError(historyquery.ErrInvalid)
	}
	if err := protojson.Unmarshal(encoded, message); err != nil {
		// Protobuf errors may embed an attacker-controlled field name/value.
		// Connect writes decoder errors directly, outside this codec.Marshal.
		return historyRPCError(historyquery.ErrInvalid)
	}
	return nil
}
