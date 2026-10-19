package contracts

import (
	"crypto/sha256"
	"math"
	"unicode/utf8"

	"connectrpc.com/connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"google.golang.org/protobuf/proto"
)

// These functions admit wire structure only. Source policy semantics and the
// current caller's permissions belong to the authorization service.
func validPolicyVersion(v *auth.PolicyVersion) bool {
	if v == nil || !knownFieldsOnly(v.ProtoReflect()) || v.Revision == 0 || v.Revision > math.MaxInt64 || len(v.SourceSha256) != sha256.Size {
		return false
	}
	var nonzero byte
	for _, b := range v.SourceSha256 {
		nonzero |= b
	}
	return nonzero != 0
}
func validPolicySource(source []byte) bool {
	return len(source) > 0 && len(source) <= MaxAuthorizationPolicySourceBytes && utf8.Valid(source)
}
func validPolicySnapshot(v *auth.PolicySnapshot) bool {
	return v != nil && knownFieldsOnly(v.ProtoReflect()) && validAccountID(v.AccountId) && validPolicySource(v.YamlSource) &&
		v.Schema == AuthorizationPolicySchema && v.SyntaxProfile == AuthorizationPolicySyntaxProfile && validPolicyVersion(v.Version) && validTimestamp(v.UpdatedAt)
}
func AdmitGetPolicyRequest(v *auth.GetPolicyRequest) error {
	if v == nil || !knownFieldsOnly(v.ProtoReflect()) || !validAccountID(v.AccountId) {
		return ErrInvalidAuthContract
	}
	return nil
}
func AdmitGetPolicyResponse(v *auth.GetPolicyResponse) (AdmittedAuthWire[*auth.GetPolicyResponse], error) {
	valid := v != nil
	if valid {
		switch result := v.Result.(type) {
		case *auth.GetPolicyResponse_Present:
			valid = result != nil && validPolicySnapshot(result.Present)
		case *auth.GetPolicyResponse_Absent:
			valid = result != nil && result.Absent != nil
		default:
			valid = false
		}
	}
	if !valid || !knownFieldsOnly(v.ProtoReflect()) {
		return AdmittedAuthWire[*auth.GetPolicyResponse]{}, ErrInvalidAuthContract
	}
	return AdmittedAuthWire[*auth.GetPolicyResponse]{value: proto.Clone(v).(*auth.GetPolicyResponse)}, nil
}
func AdmitReplacePolicyRequest(v *auth.ReplacePolicyRequest) error {
	if v == nil || !validAccountID(v.AccountId) || !validPolicySource(v.YamlSource) {
		return ErrInvalidAuthContract
	}
	switch expected := v.Precondition.(type) {
	case *auth.ReplacePolicyRequest_Expected:
		if expected != nil && validPolicyVersion(expected.Expected) && knownFieldsOnly(v.ProtoReflect()) {
			return nil
		}
	case *auth.ReplacePolicyRequest_ExpectAbsent:
		if expected != nil && expected.ExpectAbsent != nil && knownFieldsOnly(v.ProtoReflect()) {
			return nil
		}
	}
	return ErrInvalidAuthContract
}
func AdmitReplacePolicyResponse(v *auth.ReplacePolicyResponse) (AdmittedAuthWire[*auth.ReplacePolicyResponse], error) {
	if v == nil || !knownFieldsOnly(v.ProtoReflect()) || !validPolicySnapshot(v.Policy) {
		return AdmittedAuthWire[*auth.ReplacePolicyResponse]{}, ErrInvalidAuthContract
	}
	switch v.RightsChange {
	case auth.PolicyRightsChange_POLICY_RIGHTS_CHANGE_CHANGED, auth.PolicyRightsChange_POLICY_RIGHTS_CHANGE_UNCHANGED:
		return AdmittedAuthWire[*auth.ReplacePolicyResponse]{value: proto.Clone(v).(*auth.ReplacePolicyResponse)}, nil
	case auth.PolicyRightsChange_POLICY_RIGHTS_CHANGE_UNSPECIFIED:
		return AdmittedAuthWire[*auth.ReplacePolicyResponse]{}, ErrInvalidAuthContract
	default:
		return AdmittedAuthWire[*auth.ReplacePolicyResponse]{}, ErrInvalidAuthContract
	}
}
func validPolicyFieldPath(path string) bool {
	if len(path) > 128 {
		return false
	}
	for _, c := range path {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '.' || c == '[' || c == ']') {
			return false
		}
	}
	return true
}
func AuthorizationPolicyErrorConnectCode(v *auth.AuthorizationPolicyError) (connect.Code, error) {
	if v == nil || !knownFieldsOnly(v.ProtoReflect()) || !validPolicyFieldPath(v.FieldPath) {
		return 0, ErrInvalidAuthContract
	}
	switch v.Code {
	case auth.AuthorizationPolicyErrorCode_AUTHORIZATION_POLICY_ERROR_CODE_VERSION_CONFLICT:
		if v.FieldPath == "" {
			return connect.CodeAborted, nil
		}
	case auth.AuthorizationPolicyErrorCode_AUTHORIZATION_POLICY_ERROR_CODE_INVALID_SOURCE, auth.AuthorizationPolicyErrorCode_AUTHORIZATION_POLICY_ERROR_CODE_INVALID_VERSION:
		if v.FieldPath != "" {
			return connect.CodeInvalidArgument, nil
		}
	case auth.AuthorizationPolicyErrorCode_AUTHORIZATION_POLICY_ERROR_CODE_SCOPE_DENIED:
		if v.FieldPath == "" {
			return connect.CodePermissionDenied, nil
		}
	case auth.AuthorizationPolicyErrorCode_AUTHORIZATION_POLICY_ERROR_CODE_AUTHORITY_UNAVAILABLE:
		if v.FieldPath == "" {
			return connect.CodeUnavailable, nil
		}
	case auth.AuthorizationPolicyErrorCode_AUTHORIZATION_POLICY_ERROR_CODE_UNSPECIFIED:
	default:
	}
	return 0, ErrInvalidAuthContract
}
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
