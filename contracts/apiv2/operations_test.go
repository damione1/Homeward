package apiv2

import (
	"testing"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

func TestOperationEnvelopeRequiresNamedTypedTerminalOrExplicitPending(t *testing.T) {
	name, _ := apitypes.ParseOperationName("homes/synthetic/operations/cmd-synthetic")
	metadata, err := anypb.New(&api.CommandMetadata{Capability: "homes/synthetic/entities/entity-a/capabilities/power-a", Phase: api.CommandPhase_COMMAND_PHASE_ACCEPTED})
	if err != nil {
		t.Fatal(err)
	}
	pending := &longrunningpb.Operation{Name: name.String(), Metadata: metadata}
	if err := AdmitOperationEnvelope(pending, name); err != nil {
		t.Fatal(err)
	}
	result, err := anypb.New(&api.ExecuteCapabilityResponse{Capability: "homes/synthetic/entities/entity-a/capabilities/power-a", Outcome: api.CommandOutcome_COMMAND_OUTCOME_ACKNOWLEDGED_UNVERIFIED})
	if err != nil {
		t.Fatal(err)
	}
	terminal := proto.CloneOf(pending)
	terminal.Done = true
	terminal.Result = &longrunningpb.Operation_Response{Response: result}
	if err := AdmitOperationEnvelope(terminal, name); err != nil {
		t.Fatal(err)
	}
	uncertain := proto.CloneOf(pending)
	uncertain.Done = true
	uncertain.Result = &longrunningpb.Operation_Error{Error: apierrors.New(api.ApiErrorReason_API_ERROR_REASON_EFFECT_OUTCOME_UNKNOWN).Status()}
	if err := AdmitOperationEnvelope(uncertain, name); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*longrunningpb.Operation){
		func(v *longrunningpb.Operation) { v.Name = "homes/foreign/operations/cmd-synthetic" },
		func(v *longrunningpb.Operation) { v.Metadata = nil },
		func(v *longrunningpb.Operation) { v.Done = true },
		func(v *longrunningpb.Operation) { v.Result = &longrunningpb.Operation_Response{Response: result} },
		func(v *longrunningpb.Operation) { v.ProtoReflect().SetUnknown([]byte{0xf8, 0x07, 1}) },
		func(v *longrunningpb.Operation) { v.Metadata.ProtoReflect().SetUnknown([]byte{0xf8, 0x07, 1}) },
	} {
		changed := proto.CloneOf(pending)
		mutate(changed)
		if err := AdmitOperationEnvelope(changed, name); err == nil {
			t.Fatal("admitted inconsistent or foreign operation")
		}
	}
	uncertain.GetError().Code = 0
	if err := AdmitOperationEnvelope(uncertain, name); err == nil {
		t.Fatal("failed operation admitted a success status")
	}
}
