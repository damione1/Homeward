package contracts

import (
	"unicode/utf8"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/proto"
)

// Policy admissions preserve the owner's exact source. They validate public
// structure, never interpret grants or derive a native identity from a name.
func validAPIETag(value string) bool {
	return validText(value) && len(value) <= 128
}
func validPolicySource(source []byte) bool {
	return len(source) > 0 && len(source) <= MaxAuthorizationPolicySourceBytes && utf8.Valid(source)
}
func AdmitAPIAuthorizationPolicy(value *api.AuthorizationPolicy) (AdmittedAuthWire[*api.AuthorizationPolicy], error) {
	if value == nil || !knownFieldsOnly(value.ProtoReflect()) || !validPolicySource(value.YamlSource) || value.Schema != AuthorizationPolicySchema || value.SyntaxProfile != AuthorizationPolicySyntaxProfile {
		return AdmittedAuthWire[*api.AuthorizationPolicy]{}, ErrInvalidAuthenticationResource
	}
	if _, err := apitypes.ParseAuthorizationPolicyName(value.Name); err != nil {
		return AdmittedAuthWire[*api.AuthorizationPolicy]{}, ErrInvalidAuthenticationResource
	}
	valid := false
	switch value.Origin {
	case api.AuthorizationPolicyOrigin_AUTHORIZATION_POLICY_ORIGIN_DEFAULT:
		valid = value.Etag == "" && value.UpdateTime == nil
	case api.AuthorizationPolicyOrigin_AUTHORIZATION_POLICY_ORIGIN_STORED:
		valid = validAPIETag(value.Etag) && validTimestamp(value.UpdateTime)
	}
	if !valid {
		return AdmittedAuthWire[*api.AuthorizationPolicy]{}, ErrInvalidAuthenticationResource
	}
	return AdmittedAuthWire[*api.AuthorizationPolicy]{value: proto.Clone(value).(*api.AuthorizationPolicy)}, nil
}
func AdmitAPIReplaceAuthorizationPolicy(value *api.ReplaceAuthorizationPolicyRequest) error {
	if value == nil || AdmitAuthenticationRequestShape(value) != nil || !validPolicySource(value.YamlSource) {
		return ErrInvalidAuthenticationResource
	}
	if _, err := apitypes.ParseAuthorizationPolicyName(value.Name); err != nil {
		return ErrInvalidAuthenticationResource
	}
	switch expected := value.Precondition.(type) {
	case *api.ReplaceAuthorizationPolicyRequest_ExpectedEtag:
		if expected != nil && validAPIETag(expected.ExpectedEtag) {
			return nil
		}
	case *api.ReplaceAuthorizationPolicyRequest_ExpectAbsent:
		if expected != nil && expected.ExpectAbsent != nil {
			return nil
		}
	}
	return ErrInvalidAuthenticationResource
}
func AdmitAPISecondFactorPolicy(value *api.SecondFactorPolicy) (AdmittedAuthWire[*api.SecondFactorPolicy], error) {
	if value == nil || !knownFieldsOnly(value.ProtoReflect()) || !validAPIETag(value.Etag) || !validTimestamp(value.UpdateTime) {
		return AdmittedAuthWire[*api.SecondFactorPolicy]{}, ErrInvalidAuthenticationResource
	}
	if _, err := apitypes.ParseSecondFactorPolicyName(value.Name); err != nil {
		return AdmittedAuthWire[*api.SecondFactorPolicy]{}, ErrInvalidAuthenticationResource
	}
	return AdmittedAuthWire[*api.SecondFactorPolicy]{value: proto.Clone(value).(*api.SecondFactorPolicy)}, nil
}
func AdmitAPIChangeSecondFactorPolicy(value *api.ChangeSecondFactorPolicyRequest) (AdmittedAuthWire[*api.ChangeSecondFactorPolicyRequest], error) {
	if value == nil || AdmitAuthenticationRequestShape(value) != nil || !validAPIETag(value.Etag) {
		return AdmittedAuthWire[*api.ChangeSecondFactorPolicyRequest]{}, ErrInvalidAuthenticationResource
	}
	if _, err := apitypes.ParseSecondFactorPolicyName(value.Name); err != nil {
		return AdmittedAuthWire[*api.ChangeSecondFactorPolicyRequest]{}, ErrInvalidAuthenticationResource
	}
	return AdmittedAuthWire[*api.ChangeSecondFactorPolicyRequest]{value: proto.Clone(value).(*api.ChangeSecondFactorPolicyRequest)}, nil
}
func AdmitAPIChangeSecondFactorPolicyResponse(value *api.ChangeSecondFactorPolicyResponse) (AdmittedAuthWire[*api.ChangeSecondFactorPolicyResponse], error) {
	if value == nil || !knownFieldsOnly(value.ProtoReflect()) {
		return AdmittedAuthWire[*api.ChangeSecondFactorPolicyResponse]{}, ErrInvalidAuthenticationResource
	}
	if _, err := AdmitAPISecondFactorPolicy(value.Policy); err != nil {
		return AdmittedAuthWire[*api.ChangeSecondFactorPolicyResponse]{}, err
	}
	switch value.Continuation {
	case api.SecondFactorSessionContinuation_SECOND_FACTOR_SESSION_CONTINUATION_CONTINUES, api.SecondFactorSessionContinuation_SECOND_FACTOR_SESSION_CONTINUATION_REAUTHENTICATION_REQUIRED:
		return AdmittedAuthWire[*api.ChangeSecondFactorPolicyResponse]{value: proto.Clone(value).(*api.ChangeSecondFactorPolicyResponse)}, nil
	default:
		return AdmittedAuthWire[*api.ChangeSecondFactorPolicyResponse]{}, ErrInvalidAuthenticationResource
	}
}
