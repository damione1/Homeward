package apierrors

import (
	"testing"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

func TestOperationReconciliationHandle(t *testing.T) {
	name, err := apitypes.ParseOperationName("homes/synthetic/operations/command-one")
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []api.ApiErrorReason{api.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN, api.ApiErrorReason_API_ERROR_REASON_EFFECT_OUTCOME_UNKNOWN} {
		original := New(reason)
		fault := original.WithOperation(name)
		if _, present := original.Operation(); present {
			t.Fatal("builder mutated original")
		}
		admitted, err := AdmitStatus(fault.Status())
		if err != nil {
			t.Fatal(err)
		}
		if got, present := admitted.Operation(); !present || got != name {
			t.Fatal("lost typed operation")
		}
		for _, mutate := range []func(*errdetails.ResourceInfo){
			func(v *errdetails.ResourceInfo) { v.ResourceType = "homeward/Operation" },
			func(v *errdetails.ResourceInfo) { v.ResourceName = "homes/synthetic/entities/command-one" },
			func(v *errdetails.ResourceInfo) { v.Owner = "SYNTHETIC_SECRET" },
			func(v *errdetails.ResourceInfo) { v.Description = "SYNTHETIC_SECRET" },
			func(v *errdetails.ResourceInfo) { v.ProtoReflect().SetUnknown([]byte{0x28, 1}) },
		} {
			status := fault.Status()
			for i, detail := range status.Details {
				if detail.MessageIs(&errdetails.ResourceInfo{}) {
					value := &errdetails.ResourceInfo{}
					if err := detail.UnmarshalTo(value); err != nil {
						t.Fatal(err)
					}
					mutate(value)
					status.Details[i] = pack(value)
				}
			}
			if _, err := AdmitStatus(status); err == nil {
				t.Fatal("accepted invalid operation detail")
			}
		}
		duplicated := fault.Status()
		duplicated.Details = append(duplicated.Details, duplicated.Details[len(duplicated.Details)-1])
		if _, err := AdmitStatus(duplicated); err == nil {
			t.Fatal("accepted duplicate detail")
		}
	}
	if New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED).WithOperation(name).Reason() != api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED {
		t.Fatal("attachment changed original reason")
	}
	if New(api.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN).WithOperation(apitypes.OperationName{}).Reason() != api.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN {
		t.Fatal("invalid handle erased uncertainty")
	}
}
