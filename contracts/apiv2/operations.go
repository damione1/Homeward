package apiv2

import (
	"errors"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

var ErrOperationEnvelope = errors.New("invalid operation envelope")

// AdmitOperationEnvelope validates the shared standard LRO envelope only.
// Each owning API then unpacks its exact metadata/result allowlist and admits
// domain invariants. A valid envelope grants no permission or replay ability.
func AdmitOperationEnvelope(raw *longrunningpb.Operation, expected apitypes.OperationName) error {
	if raw == nil || !expected.Valid() || raw.Name != expected.String() || proto.Size(raw) > 1<<20 || len(raw.ProtoReflect().GetUnknown()) != 0 {
		return ErrOperationEnvelope
	}
	validAny := func(value *anypb.Any) bool {
		return value != nil && value.TypeUrl != "" && len(value.TypeUrl) <= 256 && len(value.ProtoReflect().GetUnknown()) == 0
	}
	if !validAny(raw.Metadata) || proto.Size(raw.Metadata) > 64<<10 {
		return ErrOperationEnvelope
	}
	if !raw.Done {
		if raw.Result != nil {
			return ErrOperationEnvelope
		}
		return nil
	}
	switch result := raw.Result.(type) {
	case *longrunningpb.Operation_Response:
		if result == nil || !validAny(result.Response) {
			return ErrOperationEnvelope
		}
	case *longrunningpb.Operation_Error:
		if result == nil {
			return ErrOperationEnvelope
		}
		if _, err := apierrors.AdmitStatus(result.Error); err != nil {
			return ErrOperationEnvelope
		}
	default:
		return ErrOperationEnvelope
	}
	return nil
}
