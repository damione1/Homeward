package contracts

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The historical login-success wire image is shared with the Rust contract test.
func nha142ContractLogin(t *testing.T) *auth.LoginDecision {
	t.Helper()
	decision := new(auth.LoginDecision)
	authFixture(t, "login-success", decision)
	return decision
}

func TestNHA142SessionAuthenticationAdmission(t *testing.T) {
	base := nha142ContractLogin(t)
	if _, err := AdmitLoginDecision(base, nha146Now); err != nil {
		t.Fatalf("legacy session with absent MFA metadata: %v", err)
	}
	methods := []auth.AuthenticationSatisfaction{
		auth.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_ONLY,
		auth.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_FIDO2,
		auth.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_RECOVERY_CODE,
	}
	for _, method := range methods {
		t.Run(method.String(), func(t *testing.T) {
			if err := AdmitAuthenticationSatisfaction(method); err != nil {
				t.Fatalf("known issuance method rejected: %v", err)
			}
			decision := proto.Clone(base).(*auth.LoginDecision)
			session := decision.GetAuthenticated().Session
			session.AuthenticationSatisfaction = method
			session.AuthenticatedAt = proto.Clone(session.CreatedAt).(*timestamppb.Timestamp)
			admitted, err := AdmitLoginDecision(decision, nha146Now)
			if err != nil {
				t.Fatalf("valid evidence rejected: %v", err)
			}
			session.AuthenticationSatisfaction = auth.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_UNSPECIFIED
			session.AuthenticatedAt.Seconds++
			snapshot, err := admitted.Snapshot()
			if err != nil || snapshot.GetAuthenticated().Session.AuthenticationSatisfaction != method || !proto.Equal(snapshot.GetAuthenticated().Session.AuthenticatedAt, snapshot.GetAuthenticated().Session.CreatedAt) {
				t.Fatal("admission lost or aliased authentication evidence")
			}
			for name, change := range map[string]func(*auth.Session){
				"missing timestamp":    func(s *auth.Session) { s.AuthenticatedAt = nil },
				"mismatched timestamp": func(s *auth.Session) { s.AuthenticatedAt.Seconds++ },
				"future timestamp": func(s *auth.Session) {
					s.AuthenticatedAt.Seconds = nha146Now.Unix() + 1
					s.CreatedAt.Seconds = nha146Now.Unix() + 1
				},
				"invalid timestamp": func(s *auth.Session) { s.AuthenticatedAt.Nanos = 1_000_000_000 },
			} {
				t.Run(name, func(t *testing.T) {
					candidate := proto.Clone(snapshot).(*auth.LoginDecision)
					change(candidate.GetAuthenticated().Session)
					if _, err := AdmitLoginDecision(candidate, nha146Now); err == nil {
						t.Fatal("invalid authentication evidence admitted")
					}
				})
			}
		})
	}
	for name, method := range map[string]auth.AuthenticationSatisfaction{
		"unspecified": auth.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_UNSPECIFIED,
		"future":      auth.AuthenticationSatisfaction(99),
	} {
		t.Run(name, func(t *testing.T) {
			if err := AdmitAuthenticationSatisfaction(method); err == nil {
				t.Fatal("non-evidence accepted for issuance")
			}
			candidate := proto.Clone(base).(*auth.LoginDecision)
			candidate.GetAuthenticated().Session.AuthenticationSatisfaction = method
			candidate.GetAuthenticated().Session.AuthenticatedAt = proto.Clone(candidate.GetAuthenticated().Session.CreatedAt).(*timestamppb.Timestamp)
			if _, err := AdmitLoginDecision(candidate, nha146Now); err == nil {
				t.Fatal("unsupported method with timestamp admitted")
			}
		})
	}
	legacyWithExplicitZeroTime := proto.Clone(base).(*auth.LoginDecision)
	legacyWithExplicitZeroTime.GetAuthenticated().Session.AuthenticatedAt = timestamppb.New(time.Unix(0, 0))
	if _, err := AdmitLoginDecision(legacyWithExplicitZeroTime, nha146Now); err == nil {
		t.Fatal("legacy unspecified method with explicitly present zero-time metadata admitted")
	}
}

func nha142ContractVersion(revision uint64) *auth.PolicyVersion {
	return &auth.PolicyVersion{Revision: revision, SourceSha256: bytes.Repeat([]byte{0x42}, 32)}
}

func TestNHA142SecondFactorPolicyAdmission(t *testing.T) {
	request := &auth.SetSecondFactorPolicyRequest{RequireAdminFactor: true, Expected: nha142ContractVersion(1)}
	admitted, err := AdmitSetSecondFactorPolicy(request)
	if err != nil {
		t.Fatalf("valid CAS rejected: %v", err)
	}
	request.Expected.SourceSha256[0] = 0
	snapshot, err := admitted.Snapshot()
	if err != nil || snapshot.Expected.SourceSha256[0] != 0x42 {
		t.Fatal("admitted CAS aliases request")
	}
	for _, revision := range []uint64{1, math.MaxInt64} {
		if _, err := AdmitSetSecondFactorPolicy(&auth.SetSecondFactorPolicyRequest{Expected: nha142ContractVersion(revision)}); err != nil {
			t.Fatalf("valid revision %d rejected: %v", revision, err)
		}
	}
	for name, version := range map[string]*auth.PolicyVersion{
		"missing":           nil,
		"zero revision":     nha142ContractVersion(0),
		"overflow revision": nha142ContractVersion(uint64(math.MaxInt64) + 1),
		"short hash":        {Revision: 1, SourceSha256: bytes.Repeat([]byte{0x42}, 31)},
		"long hash":         {Revision: 1, SourceSha256: bytes.Repeat([]byte{0x42}, 33)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := AdmitSetSecondFactorPolicy(&auth.SetSecondFactorPolicyRequest{Expected: version}); err == nil {
				t.Fatal("invalid CAS admitted")
			}
		})
	}
	policy := &auth.SecondFactorPolicy{Version: nha142ContractVersion(1), UpdatedAt: timestamppb.New(time.Unix(1792310000, 0))}
	if _, err := AdmitGetSecondFactorPolicyResponse(&auth.GetSecondFactorPolicyResponse{Policy: policy}); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}
	for _, continuation := range []auth.SecondFactorSessionContinuation{
		auth.SecondFactorSessionContinuation_SECOND_FACTOR_SESSION_CONTINUATION_CONTINUES,
		auth.SecondFactorSessionContinuation_SECOND_FACTOR_SESSION_CONTINUATION_REAUTHENTICATION_REQUIRED,
	} {
		if _, err := AdmitSetSecondFactorPolicyResponse(&auth.SetSecondFactorPolicyResponse{Policy: policy, Continuation: continuation}); err != nil {
			t.Fatalf("valid continuation rejected: %v", err)
		}
	}
	for _, continuation := range []auth.SecondFactorSessionContinuation{auth.SecondFactorSessionContinuation_SECOND_FACTOR_SESSION_CONTINUATION_UNSPECIFIED, auth.SecondFactorSessionContinuation(99)} {
		if _, err := AdmitSetSecondFactorPolicyResponse(&auth.SetSecondFactorPolicyResponse{Policy: policy, Continuation: continuation}); err == nil {
			t.Fatal("unspecified/future continuation admitted")
		}
	}
	for name, badPolicy := range map[string]*auth.SecondFactorPolicy{
		"missing":         nil,
		"missing version": {UpdatedAt: policy.UpdatedAt},
		"missing date":    {Version: policy.Version},
		"invalid date":    {Version: policy.Version, UpdatedAt: &timestamppb.Timestamp{Seconds: 1792310000, Nanos: 1_000_000_000}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := AdmitGetSecondFactorPolicyResponse(&auth.GetSecondFactorPolicyResponse{Policy: badPolicy}); err == nil {
				t.Fatal("invalid policy admitted")
			}
			if _, err := AdmitSetSecondFactorPolicyResponse(&auth.SetSecondFactorPolicyResponse{Policy: badPolicy, Continuation: auth.SecondFactorSessionContinuation_SECOND_FACTOR_SESSION_CONTINUATION_CONTINUES}); err == nil {
				t.Fatal("invalid set response admitted")
			}
		})
	}
}

func TestNHA142PolicyErrorAndUnixResetAdmission(t *testing.T) {
	for _, tc := range []struct {
		code  auth.SecondFactorPolicyErrorCode
		field string
		valid bool
	}{
		{auth.SecondFactorPolicyErrorCode_SECOND_FACTOR_POLICY_ERROR_CODE_INVALID_VERSION, "expected", true},
		{auth.SecondFactorPolicyErrorCode_SECOND_FACTOR_POLICY_ERROR_CODE_VERSION_CONFLICT, "expected", true},
		{auth.SecondFactorPolicyErrorCode_SECOND_FACTOR_POLICY_ERROR_CODE_AUTHORITY_UNAVAILABLE, "", true},
		{auth.SecondFactorPolicyErrorCode_SECOND_FACTOR_POLICY_ERROR_CODE_INVALID_VERSION, "", false},
		{auth.SecondFactorPolicyErrorCode_SECOND_FACTOR_POLICY_ERROR_CODE_VERSION_CONFLICT, "policy", false},
		{auth.SecondFactorPolicyErrorCode_SECOND_FACTOR_POLICY_ERROR_CODE_AUTHORITY_UNAVAILABLE, "expected", false},
		{auth.SecondFactorPolicyErrorCode_SECOND_FACTOR_POLICY_ERROR_CODE_UNSPECIFIED, "expected", false},
		{auth.SecondFactorPolicyErrorCode(99), "expected", false},
	} {
		_, err := AdmitSecondFactorPolicyError(&auth.SecondFactorPolicyError{Code: tc.code, Field: tc.field})
		if (err == nil) != tc.valid {
			t.Errorf("code=%v field=%q: admitted=%v want=%v", tc.code, tc.field, err == nil, tc.valid)
		}
	}
	for _, id := range []string{"account-a", strings.Repeat("a", 128)} {
		if err := AdmitResetSecondFactor(&auth.ResetSecondFactorRequest{AccountId: &auth.AccountId{Value: id}}); err != nil {
			t.Fatalf("valid Unix reset target rejected: %v", err)
		}
	}
	for _, request := range []*auth.ResetSecondFactorRequest{nil, {}, {AccountId: &auth.AccountId{}}, {AccountId: &auth.AccountId{Value: strings.Repeat("a", 129)}}} {
		if err := AdmitResetSecondFactor(request); err == nil {
			t.Fatal("invalid Unix reset target admitted")
		}
	}
}
