package server

import (
	"connectrpc.com/connect"
	"context"
	"errors"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	native "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"testing"
	"time"
)

func TestNHA198SessionErrorsUseCanonicalCredentialBoundary(t *testing.T) {
	for _, cause := range []error{storage.ErrSessionExpired, storage.ErrSessionRevoked,
		storage.ErrSessionNotFound, storage.ErrAccountNotFound, storage.ErrSessionAccountDisabled} {
		err := authCredentialError(context.Background(), cause)
		var rpc *connect.Error
		if !errors.As(err, &rpc) || rpc.Code() != connect.CodeUnauthenticated {
			t.Fatal("credential diagnosis leaked", err)
		}
		found := false
		for _, detail := range rpc.Details() {
			value, decodeErr := detail.Value()
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if info, ok := value.(*errdetails.ErrorInfo); ok {
				if info.Domain != apierrors.Domain() || info.Reason != "AUTHENTICATION_REQUIRED" || len(info.Metadata) != 0 {
					t.Fatal("wrong canonical credential detail")
				}
				found = true
			}
		}
		if !found {
			t.Fatal("canonical credential detail missing")
		}
	}
	var unavailable *connect.Error
	err := authCredentialError(context.Background(), storage.ErrSessionUnavailable)
	if !errors.As(err, &unavailable) || unavailable.Code() != connect.CodeUnavailable {
		t.Fatal("outage became terminal credential refusal", err)
	}
	if authAPIFault(storage.ErrSessionNotFound).Reason() != api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND {
		t.Fatal("authorized named lookup lost NOT_FOUND")
	}
}

func TestNHA203UnrecordedAuthenticationNeverInventsEvidence(t *testing.T) {
	absent := native.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_UNSPECIFIED
	value, err := publicAuthenticationEvidence(absent, time.Time{})
	if err != nil || value != api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_NOT_RECORDED {
		t.Fatal("unrecorded evidence not explicit", err)
	}
	if _, err := publicAuthenticationEvidence(absent, time.Now()); !errors.Is(err, storage.ErrAPIResourceIntegrity) {
		t.Fatal("timestamp without evidence accepted", err)
	}
	for _, evidence := range []native.AuthenticationSatisfaction{
		native.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_ONLY,
		native.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_FIDO2,
		native.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_RECOVERY_CODE,
	} {
		if _, err := publicAuthenticationEvidence(evidence, time.Time{}); !errors.Is(err, storage.ErrAPIResourceIntegrity) {
			t.Fatal("evidence without time accepted", err)
		}
		if _, err := publicAuthenticationEvidence(evidence, time.Now()); err != nil {
			t.Fatal("recorded native evidence rejected", err)
		}
	}
	if _, err := publicAuthenticationEvidence(native.AuthenticationSatisfaction(99), time.Now()); !errors.Is(err, storage.ErrAPIResourceIntegrity) {
		t.Fatal("unknown evidence admitted", err)
	}
}
