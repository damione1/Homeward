package server

import (
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/storage"
)

func TestNHA198SessionErrorsUseTypedContract(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		cause    error
		code     connect.Code
		wireCode authv1.AuthErrorCode
	}{
		{"expired", storage.ErrSessionExpired, connect.CodeUnauthenticated, authv1.AuthErrorCode_AUTH_ERROR_CODE_EXPIRED},
		{"revoked", storage.ErrSessionRevoked, connect.CodeUnauthenticated, authv1.AuthErrorCode_AUTH_ERROR_CODE_UNAUTHENTICATED},
		{"unknown", storage.ErrSessionNotFound, connect.CodeUnauthenticated, authv1.AuthErrorCode_AUTH_ERROR_CODE_UNAUTHENTICATED},
		{"disabled", storage.ErrSessionAccountDisabled, connect.CodePermissionDenied, authv1.AuthErrorCode_AUTH_ERROR_CODE_PERMISSION_DENIED},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			err := sessionRPCError(scenario.cause)
			nha049Code(t, err, scenario.code)
			var rpc *connect.Error
			if !errors.As(err, &rpc) || len(rpc.Details()) != 1 {
				t.Fatal("terminal error omitted typed detail")
			}
			value, decodeErr := rpc.Details()[0].Value()
			detail, ok := value.(*authv1.AuthError)
			if decodeErr != nil || !ok || detail.Code != scenario.wireCode {
				t.Fatal("wrong terminal detail")
			}
			if _, err := contracts.AdmitAuthError(detail); err != nil {
				t.Fatalf("terminal detail violated contract: %v", err)
			}
		})
	}
	var unavailable *connect.Error
	if err := sessionRPCError(storage.ErrSessionUnavailable); !errors.As(err, &unavailable) || unavailable.Code() != connect.CodeUnavailable || len(unavailable.Details()) != 0 {
		t.Fatal("outage acquired a terminal credential detail")
	}
}
