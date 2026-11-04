package server

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"google.golang.org/protobuf/proto"
)

func TestCanonicalFaultSurvivesNestedConnectBoundary(t *testing.T) {
	issue, err := apierrors.NewViolation((&api.UpdateEntityRequest{}).ProtoReflect().Descriptor(), "entity.display_name", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT)
	if err != nil {
		t.Fatal(err)
	}
	for _, fault := range []apierrors.Fault{
		apierrors.Validation(issue),
		apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED),
		apierrors.New(api.ApiErrorReason_API_ERROR_REASON_EFFECT_OUTCOME_UNKNOWN),
		apierrors.New(api.ApiErrorReason_API_ERROR_REASON_CONFIG_RESTART_REQUIRED),
	} {
		t.Run(fault.Reason().String(), func(t *testing.T) {
			original := fault.Connect()
			wrapped := apiRPCError(context.Background(), fmt.Errorf("native wrapper: %w", original))
			var actual *connect.Error
			if !errors.As(wrapped, &actual) || actual.Code() != original.Code() || actual.Message() != original.Message() {
				t.Fatal("canonical fault was reclassified")
			}
			var retained apierrors.Fault
			if !errors.As(actual, &retained) || retained.Reason() != fault.Reason() {
				t.Fatal("typed fault cause was lost")
			}
			if len(actual.Details()) != len(original.Details()) {
				t.Fatal("structured details changed")
			}
			for i, before := range original.Details() {
				after := actual.Details()[i]
				left, err := before.Value()
				if err != nil {
					t.Fatal(err)
				}
				right, err := after.Value()
				if err != nil {
					t.Fatal(err)
				}
				if before.Type() != after.Type() || !proto.Equal(left, right) {
					t.Fatal("typed field/precondition/uncertainty detail changed")
				}
			}
		})
	}
	// An arbitrary Connect code/text has no admitted Fault cause. Never trust
	// an upstream status merely because it claims one of our canonical codes.
	foreign := connect.NewError(connect.CodePermissionDenied, errors.New("SYNTHETIC_UPSTREAM_SECRET_CANARY"))
	actual := apiRPCError(context.Background(), foreign)
	if connect.CodeOf(actual) != connect.CodeInternal {
		t.Fatal("untrusted Connect status bypassed classification")
	}
	if actual.Error() == foreign.Error() {
		t.Fatal("upstream diagnostic was echoed")
	}
}
