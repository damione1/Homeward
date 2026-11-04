package server

import (
	"github.com/damione1/Homeward/internal/historyquery"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// The handler admits and bounds both representations before writing a success.
// Decoder diagnostics are canonical; arbitrary field/value text never escapes.
type historyV2JSONCodec struct{ name string }

func (c historyV2JSONCodec) Name() string { return c.name }
func (c historyV2JSONCodec) Marshal(value any) ([]byte, error) {
	message, ok := value.(proto.Message)
	if !ok {
		return nil, historyFault(historyquery.ErrIntegrity).Connect()
	}
	encoded, err := protojson.Marshal(message)
	if err != nil {
		return nil, historyFault(historyquery.ErrIntegrity).Connect()
	}
	if len(encoded) > historyquery.MaxResponseBytes {
		return nil, historyFault(historyquery.ErrBudget).Connect()
	}
	return encoded, nil
}
func (c historyV2JSONCodec) Unmarshal(encoded []byte, value any) error {
	message, ok := value.(proto.Message)
	if !ok || len(encoded) > 8<<10 {
		return historyFault(historyquery.ErrInvalid).Connect()
	}
	if err := protojson.Unmarshal(encoded, message); err != nil {
		return historyFault(historyquery.ErrInvalid).Connect()
	}
	return nil
}
