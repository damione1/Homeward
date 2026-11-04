package contracts

import (
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"google.golang.org/protobuf/proto"
)

// Native termination evidence remains current across public API generations.
func AdmitAuthorizationTermination(v *auth.AuthorizationTermination) (AdmittedAuthWire[*auth.AuthorizationTermination], error) {
	if v == nil || !knownFieldsOnly(v.ProtoReflect()) {
		return AdmittedAuthWire[*auth.AuthorizationTermination]{}, ErrInvalidAuthContract
	}
	switch v.Reason {
	case auth.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_CREDENTIAL_REVOKED,
		auth.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_CREDENTIAL_EXPIRED,
		auth.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_ACCOUNT_DISABLED,
		auth.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_RIGHTS_CHANGED,
		auth.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_AUTHORITY_UNAVAILABLE:
		return AdmittedAuthWire[*auth.AuthorizationTermination]{value: proto.Clone(v).(*auth.AuthorizationTermination)}, nil
	case auth.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_UNSPECIFIED:
	default:
	}
	return AdmittedAuthWire[*auth.AuthorizationTermination]{}, ErrInvalidAuthContract
}
