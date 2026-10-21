package contracts

import (
	statev1 "github.com/damione1/Homeward/gen/home/state/v1"
	"google.golang.org/protobuf/proto"
)

// AdmitCoreViewWire rejects future executable/request semantics while keeping
// the generated decoder forward compatible. Domain parsing remains in view.
func AdmitCoreViewWire(message proto.Message) error {
	switch request := message.(type) {
	case *statev1.OpenViewRequest:
		if request != nil && request.Filter != nil && knownFieldsOnly(request.ProtoReflect()) {
			return nil
		}
	case *statev1.ReadPageRequest:
		if request != nil && knownFieldsOnly(request.ProtoReflect()) {
			return nil
		}
	case *statev1.WatchViewRequest:
		if request != nil && request.Filter != nil && knownFieldsOnly(request.ProtoReflect()) {
			return nil
		}
	}
	return ErrInvalidActionRPC
}
