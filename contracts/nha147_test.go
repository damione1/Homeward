package contracts

import (
	"testing"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	statev1 "github.com/damione1/Homeward/gen/home/state/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestNHA147DurableAndLiveRemainSeparateAcrossUnknownWireValues(t *testing.T) {
	response := &actions.GetActionResponse{
		Durable: &actions.DurableActionResult{State: actions.DurableActionState_DURABLE_ACTION_STATE_RESULT,
			Error:    actions.DurableActionError_DURABLE_ACTION_ERROR_NONE,
			Response: &actions.CommandResponse{CommandId: "nha147-durable", Status: actions.ActionStatus_ACTION_STATUS_DISPATCHED}},
		Live: &actions.GetActionResponse_Unavailable{Unavailable: &actions.NoLiveActionProgress{}},
	}
	if err := AdmitGetActionResponse(response); err != nil {
		t.Fatal(err)
	}
	encoded, err := proto.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	decoded := new(actions.GetActionResponse)
	if err := proto.Unmarshal(encoded, decoded); err != nil || decoded.GetUnavailable() == nil || decoded.Durable.Response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatal("durable dispatch became live/physical confirmation", decoded, err)
	}
	for _, mutate := range []func(*actions.GetActionResponse){
		func(v *actions.GetActionResponse) { v.Live = nil },
		func(v *actions.GetActionResponse) { v.Live = (*actions.GetActionResponse_Progress)(nil) },
		func(v *actions.GetActionResponse) { v.Durable.State = actions.DurableActionState(999) },
		func(v *actions.GetActionResponse) { v.Durable.Error = actions.DurableActionError(999) },
		func(v *actions.GetActionResponse) { v.Durable.Response.Status = actions.ActionStatus(999) },
		func(v *actions.GetActionResponse) {
			v.Durable.State = actions.DurableActionState_DURABLE_ACTION_STATE_INDETERMINATE
		},
	} {
		invalid := proto.Clone(response).(*actions.GetActionResponse)
		mutate(invalid)
		wire, err := proto.Marshal(invalid)
		if err != nil {
			t.Fatal(err)
		}
		future := new(actions.GetActionResponse)
		if err := proto.Unmarshal(wire, future); err != nil {
			t.Fatal("future wire stopped decoding", err)
		}
		if AdmitGetActionResponse(invalid) == nil {
			t.Fatal("invalid/future operational fact admitted", invalid)
		}
	}
	// Unknown fields survive decoding but cannot acquire operational meaning.
	futureWire := append(encoded, protowire.AppendTag(nil, 999, protowire.VarintType)...)
	futureWire = protowire.AppendVarint(futureWire, 1)
	if err := proto.Unmarshal(futureWire, decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.ProtoReflect().GetUnknown()) == 0 || AdmitGetActionResponse(decoded) == nil {
		t.Fatal("unknown fact admitted")
	}
}

func TestNHA147IndeterminateErrorDetailRetainsResponseAndCopy(t *testing.T) {
	response := &actions.CommandResponse{CommandId: "nha147-unknown", Status: actions.ActionStatus_ACTION_STATUS_INDETERMINATE}
	detail := &actions.ActionOperationFailure{Kind: actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_OUTCOME_INDETERMINATE, Response: ActionResponseCopy(response)}
	response.Status = actions.ActionStatus_ACTION_STATUS_CONFIRMED
	if detail.Response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || AdmitActionOperationFailure(detail) != nil {
		t.Fatal("outcome uncertainty was lost or aliased", detail)
	}
	for _, invalid := range []*actions.ActionOperationFailure{nil, {}, {Kind: actions.ActionOperationFailureKind(999)}} {
		if AdmitActionOperationFailure(invalid) == nil {
			t.Fatal("unknown failure kind admitted")
		}
	}
	commit := &actions.ActionOperationFailure{Kind: actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_COMMIT_OUTCOME_UNKNOWN}
	if err := AdmitActionOperationFailure(commit); err != nil {
		t.Fatal("unknown commit incorrectly requires a fabricated response", err)
	}
}

func TestNHA147ViewWireRejectsFutureAndMissingFilter(t *testing.T) {
	if AdmitCoreViewWire(&statev1.OpenViewRequest{PageSize: 1}) == nil {
		t.Fatal("missing filter silently became all")
	}
	request := &statev1.OpenViewRequest{Filter: &statev1.ViewFilter{}, PageSize: 1}
	if err := AdmitCoreViewWire(request); err != nil {
		t.Fatal(err)
	}
	request.Filter.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 900, protowire.VarintType), 1))
	if AdmitCoreViewWire(request) == nil {
		t.Fatal("future filter semantics admitted")
	}
}
