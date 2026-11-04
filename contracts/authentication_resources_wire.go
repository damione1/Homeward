package contracts

import (
	"time"
	"unicode/utf8"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/proto"
)

func validAPIAccountName(value string) bool {
	_, err := apitypes.ParseAccountName(value)
	return err == nil
}

// These admissions validate the current public wire. Native credential verification
// and authorization remain owned by the server; no v1 request is constructed.
func AdmitAPIAccount(value *api.Account) (AdmittedAuthWire[*api.Account], error) {
	if value == nil || !knownFieldsOnly(value.ProtoReflect()) || !validAPIAccountName(value.Name) ||
		!validText(value.LoginName) || len(value.LoginName) > 256 || value.Role != api.AccountRole_ACCOUNT_ROLE_ADMIN && value.Role != api.AccountRole_ACCOUNT_ROLE_USER ||
		value.State != api.AccountState_ACCOUNT_STATE_ACTIVE && value.State != api.AccountState_ACCOUNT_STATE_DISABLED ||
		!validTimestamp(value.CreateTime) || !validTimestamp(value.UpdateTime) || value.UpdateTime.AsTime().Before(value.CreateTime.AsTime()) ||
		value.Etag == "" || len(value.Etag) > 128 || value.InitialPassword != "" {
		return AdmittedAuthWire[*api.Account]{}, ErrInvalidAuthenticationResource
	}
	return AdmittedAuthWire[*api.Account]{value: proto.Clone(value).(*api.Account)}, nil
}

func validAPIFactorChallenge(challenge *api.FactorChallenge, now time.Time) bool {
	if challenge == nil || !knownFieldsOnly(challenge.ProtoReflect()) || !validText(challenge.ChallengeId) || len(challenge.ChallengeId) > 128 || !validFuture(challenge.ExpireTime, now) || len(challenge.Options) == 0 {
		return false
	}
	seenFido, seenRecovery := false, false
	for _, option := range challenge.Options {
		if option == nil {
			return false
		}
		switch method := option.Method.(type) {
		case *api.FactorOption_Fido2:
			if seenFido || method == nil || method.Fido2 == nil {
				return false
			}
			f := method.Fido2
			if len(f.Challenge) == 0 || !validText(f.RpId) || !validText(f.Origin) || !validText(f.InstanceId) || len(f.InstanceId) > 128 {
				return false
			}
			seenFido = true
		case *api.FactorOption_RecoveryCode:
			if seenRecovery || method == nil || method.RecoveryCode == nil {
				return false
			}
			seenRecovery = true
		default:
			return false
		}
	}
	return true
}

// AdmitAPIFactorProofForChallenge only checks the offered wire shape. It never
// verifies a recovery code, FIDO signature, challenge binding, RP ID or UV.
func AdmitAPIFactorProofForChallenge(proof *api.FactorProof, challenge *api.FactorChallenge, now time.Time) error {
	if proof == nil || !knownFieldsOnly(proof.ProtoReflect()) || !validAPIFactorChallenge(challenge, now) {
		return ErrInvalidAuthenticationResource
	}
	for _, option := range challenge.Options {
		switch chosen := proof.Proof.(type) {
		case *api.FactorProof_Fido2:
			if _, offered := option.Method.(*api.FactorOption_Fido2); offered && chosen != nil && chosen.Fido2 != nil {
				f := chosen.Fido2
				if len(f.CredentialId) != 0 && len(f.AuthenticatorData) != 0 && len(f.ClientDataJson) != 0 && len(f.Signature) != 0 && len(f.ClientDataHash) == 32 {
					return nil
				}
			}
		case *api.FactorProof_RecoveryCode:
			if _, offered := option.Method.(*api.FactorOption_RecoveryCode); offered && chosen != nil && validText(chosen.RecoveryCode) {
				return nil
			}
		default:
			return ErrInvalidAuthenticationResource
		}
	}
	return ErrInvalidAuthenticationResource
}

func AdmitAPICompleteLoginRequest(request *api.CompleteLoginRequest, challenge *api.FactorChallenge, now time.Time) error {
	if request == nil || !knownFieldsOnly(request.ProtoReflect()) || !validAPIFactorChallenge(challenge, now) ||
		!validText(request.ChallengeId) || request.ChallengeId != challenge.ChallengeId {
		return ErrInvalidAuthenticationResource
	}
	return AdmitAPIFactorProofForChallenge(request.Proof, challenge, now)
}

func AdmitAPIBeginFactorEnrollment(request *api.BeginFactorEnrollmentRequest) error {
	if request == nil || !knownFieldsOnly(request.ProtoReflect()) || request.Kind != api.FactorKind_FACTOR_KIND_FIDO2 || !utf8.ValidString(request.EnrollmentToken) {
		return ErrInvalidAuthenticationResource
	}
	if _, err := apitypes.ParseAccountName(request.Parent); err != nil {
		return ErrInvalidAuthenticationResource
	}
	return nil
}

func AdmitAPILoginDecision(decision *api.LoginDecision, now time.Time) (AdmittedAuthWire[*api.LoginDecision], error) {
	if decision == nil || !knownFieldsOnly(decision.ProtoReflect()) {
		return AdmittedAuthWire[*api.LoginDecision]{}, ErrInvalidAuthenticationResource
	}
	valid := false
	switch outcome := decision.Outcome.(type) {
	case *api.LoginDecision_Authenticated:
		valid = outcome != nil && admitAPISessionTokens(outcome.Authenticated, now, 0) == nil
	case *api.LoginDecision_Challenge:
		valid = outcome != nil && validAPIFactorChallenge(outcome.Challenge, now)
	case *api.LoginDecision_EnrollmentRequired:
		valid = outcome != nil && outcome.EnrollmentRequired != nil &&
			validAPIAccountName(outcome.EnrollmentRequired.Account) &&
			validText(outcome.EnrollmentRequired.EnrollmentToken) &&
			validFuture(outcome.EnrollmentRequired.ExpireTime, now)
	default:
		// Includes absent and unknown future oneof branches.
		valid = false
	}
	if !valid {
		return AdmittedAuthWire[*api.LoginDecision]{}, ErrInvalidAuthenticationResource
	}
	return AdmittedAuthWire[*api.LoginDecision]{value: proto.Clone(decision).(*api.LoginDecision)}, nil
}
