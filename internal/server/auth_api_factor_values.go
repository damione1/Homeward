package server

import (
	"bytes"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	native "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Ceremony values remain owned by the native MFA verifier and encrypted store.
// These are explicit value conversions, never a call to an older RPC or profile.
func nativeFactorProof(value *api.FactorProof) (*native.FactorProof, error) {
	if value == nil {
		return nil, authmfa.NewError(authmfa.ErrorInvalidProof)
	}
	switch proof := value.Proof.(type) {
	case *api.FactorProof_Fido2:
		if proof == nil || proof.Fido2 == nil {
			return nil, authmfa.NewError(authmfa.ErrorInvalidProof)
		}
		f := proof.Fido2
		return &native.FactorProof{Proof: &native.FactorProof_Fido2{Fido2: &native.Fido2Assertion{CredentialId: bytes.Clone(f.CredentialId), AuthenticatorData: bytes.Clone(f.AuthenticatorData), ClientDataJson: bytes.Clone(f.ClientDataJson), Signature: bytes.Clone(f.Signature), UserHandle: bytes.Clone(f.UserHandle), ClientDataHash: bytes.Clone(f.ClientDataHash)}}}, nil
	case *api.FactorProof_RecoveryCode:
		if proof == nil {
			return nil, authmfa.NewError(authmfa.ErrorInvalidProof)
		}
		return &native.FactorProof{Proof: &native.FactorProof_RecoveryCode{RecoveryCode: proof.RecoveryCode}}, nil
	default:
		return nil, authmfa.NewError(authmfa.ErrorInvalidProof)
	}
}
func nativeFactorAttestation(value *api.Fido2Attestation) (*native.Fido2Attestation, error) {
	if value == nil {
		return nil, authmfa.NewError(authmfa.ErrorInvalidProof)
	}
	return &native.Fido2Attestation{CredentialId: bytes.Clone(value.CredentialId), ClientDataJson: bytes.Clone(value.ClientDataJson), AttestationObject: bytes.Clone(value.AttestationObject), ClientDataHash: bytes.Clone(value.ClientDataHash)}, nil
}
func publicFactorChallenge(value *native.FactorChallenge) (*api.FactorChallenge, error) {
	if value == nil || value.Id == nil || value.ExpiresAt == nil || value.ExpiresAt.CheckValid() != nil {
		return nil, storage.ErrAPIResourceIntegrity
	}
	result := &api.FactorChallenge{ChallengeId: value.Id.Value, ExpireTime: proto.Clone(value.ExpiresAt).(*timestamppb.Timestamp)}
	for _, option := range value.Options {
		if option == nil {
			return nil, storage.ErrAPIResourceIntegrity
		}
		switch offered := option.Method.(type) {
		case *native.FactorOption_Fido2:
			if offered == nil || offered.Fido2 == nil || offered.Fido2.InstanceId == nil {
				return nil, storage.ErrAPIResourceIntegrity
			}
			f := offered.Fido2
			ids := make([][]byte, len(f.AllowedCredentialIds))
			for i, id := range f.AllowedCredentialIds {
				ids[i] = bytes.Clone(id)
			}
			result.Options = append(result.Options, &api.FactorOption{Method: &api.FactorOption_Fido2{Fido2: &api.Fido2AssertionChallenge{Challenge: bytes.Clone(f.Challenge), RpId: f.RpId, AllowedCredentialIds: ids, InstanceId: f.InstanceId.Value, Origin: f.Origin}}})
		case *native.FactorOption_RecoveryCode:
			if offered == nil || offered.RecoveryCode == nil {
				return nil, storage.ErrAPIResourceIntegrity
			}
			result.Options = append(result.Options, &api.FactorOption{Method: &api.FactorOption_RecoveryCode{RecoveryCode: &api.RecoveryCodeChallenge{}}})
		default:
			return nil, storage.ErrAPIResourceIntegrity
		}
	}
	return result, nil
}
func publicEnrollmentOptions(f *native.Fido2EnrollmentChallenge) (*api.Fido2EnrollmentChallenge, error) {
	if f == nil || f.InstanceId == nil {
		return nil, storage.ErrAPIResourceIntegrity
	}
	return &api.Fido2EnrollmentChallenge{Challenge: bytes.Clone(f.Challenge), RpId: f.RpId, UserId: bytes.Clone(f.UserId), UserName: f.UserName, InstanceId: f.InstanceId.Value, Origin: f.Origin}, nil
}
