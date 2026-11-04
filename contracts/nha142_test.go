package contracts

import (
	"strings"
	"testing"
	"time"

	auth "github.com/damione1/Homeward/gen/home/api/v2"
	native "github.com/damione1/Homeward/gen/home/auth/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The current v2 login wire image is shared with the Rust contract test.
func nha142ContractLogin(t *testing.T) *auth.LoginDecision {
	t.Helper()
	decision := new(auth.LoginDecision)
	authResourceFixture(t, "login-success", decision)
	return decision
}

func TestNHA142SessionAuthenticationAdmission(t *testing.T) {
	base := nha142ContractLogin(t)
	if _, err := AdmitAPILoginDecision(base, nha146Now); err != nil {
		t.Fatalf("current session metadata: %v", err)
	}
	methods := []auth.AuthenticationSatisfaction{
		auth.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_ONLY,
		auth.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_FIDO2,
		auth.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_RECOVERY_CODE,
	}
	for _, method := range methods {
		t.Run(method.String(), func(t *testing.T) {
			if err := AdmitAuthenticationSatisfaction(native.AuthenticationSatisfaction(method)); err != nil {
				t.Fatalf("known issuance method rejected: %v", err)
			}
			decision := proto.Clone(base).(*auth.LoginDecision)
			session := decision.GetAuthenticated().Session
			session.AuthenticationSatisfaction = method
			session.AuthenticationTime = proto.Clone(session.CreateTime).(*timestamppb.Timestamp)
			admitted, err := AdmitAPILoginDecision(decision, nha146Now)
			if err != nil {
				t.Fatalf("valid evidence rejected: %v", err)
			}
			session.AuthenticationSatisfaction = auth.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_UNSPECIFIED
			session.AuthenticationTime.Seconds++
			snapshot, err := admitted.Snapshot()
			if err != nil || snapshot.GetAuthenticated().Session.AuthenticationSatisfaction != method || !proto.Equal(snapshot.GetAuthenticated().Session.AuthenticationTime, snapshot.GetAuthenticated().Session.CreateTime) {
				t.Fatal("admission lost or aliased authentication evidence")
			}
			for name, change := range map[string]func(*auth.Session){
				"missing timestamp":    func(s *auth.Session) { s.AuthenticationTime = nil },
				"mismatched timestamp": func(s *auth.Session) { s.AuthenticationTime.Seconds++ },
				"future timestamp": func(s *auth.Session) {
					s.AuthenticationTime.Seconds = nha146Now.Unix() + 1
					s.CreateTime.Seconds = nha146Now.Unix() + 1
				},
				"invalid timestamp": func(s *auth.Session) { s.AuthenticationTime.Nanos = 1_000_000_000 },
			} {
				t.Run(name, func(t *testing.T) {
					candidate := proto.Clone(snapshot).(*auth.LoginDecision)
					change(candidate.GetAuthenticated().Session)
					if _, err := AdmitAPILoginDecision(candidate, nha146Now); err == nil {
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
			if err := AdmitAuthenticationSatisfaction(native.AuthenticationSatisfaction(method)); err == nil {
				t.Fatal("non-evidence accepted for issuance")
			}
			candidate := proto.Clone(base).(*auth.LoginDecision)
			candidate.GetAuthenticated().Session.AuthenticationSatisfaction = method
			candidate.GetAuthenticated().Session.AuthenticationTime = proto.Clone(candidate.GetAuthenticated().Session.CreateTime).(*timestamppb.Timestamp)
			if _, err := AdmitAPILoginDecision(candidate, nha146Now); err == nil {
				t.Fatal("unsupported method with timestamp admitted")
			}
		})
	}
	unrecordedWithTimestamp := proto.Clone(base).(*auth.LoginDecision)
	unrecordedWithTimestamp.GetAuthenticated().Session.AuthenticationTime = timestamppb.New(time.Unix(0, 0))
	if _, err := AdmitAPILoginDecision(unrecordedWithTimestamp, nha146Now); err == nil {
		t.Fatal("authentication evidence with a contradictory timestamp admitted")
	}
}

func TestNHA142UnixResetAdmission(t *testing.T) {
	for _, id := range []string{"account-a", strings.Repeat("a", 128)} {
		if err := AdmitResetSecondFactor(&native.ResetSecondFactorRequest{AccountId: &native.AccountId{Value: id}}); err != nil {
			t.Fatalf("valid Unix reset target rejected: %v", err)
		}
	}
	for _, request := range []*native.ResetSecondFactorRequest{nil, {}, {AccountId: &native.AccountId{}}, {AccountId: &native.AccountId{Value: strings.Repeat("a", 129)}}} {
		if err := AdmitResetSecondFactor(request); err == nil {
			t.Fatal("invalid Unix reset target admitted")
		}
	}
}

func TestNHA142SecondFactorPolicyAdmission(t *testing.T) {
	request := &auth.ChangeSecondFactorPolicyRequest{Name: "homes/home-a/secondFactorPolicy", RequireAdminFactor: true, Etag: `W/"native-version"`}
	admitted, err := AdmitAPIChangeSecondFactorPolicy(request)
	if err != nil {
		t.Fatal(err)
	}
	request.Etag = "mutated"
	snapshot, _ := admitted.Snapshot()
	if snapshot.Etag != `W/"native-version"` {
		t.Fatal("CAS snapshot aliases input")
	}
	for _, change := range []func(*auth.ChangeSecondFactorPolicyRequest){
		func(v *auth.ChangeSecondFactorPolicyRequest) { v.Etag = "" },
		func(v *auth.ChangeSecondFactorPolicyRequest) { v.Etag = strings.Repeat("x", 129) },
		func(v *auth.ChangeSecondFactorPolicyRequest) {
			v.Name = "homes/home-a/accounts/account-a/authorizationPolicy"
		},
		func(v *auth.ChangeSecondFactorPolicyRequest) { v.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 1}) },
	} {
		value := proto.Clone(snapshot).(*auth.ChangeSecondFactorPolicyRequest)
		change(value)
		if _, err := AdmitAPIChangeSecondFactorPolicy(value); err == nil {
			t.Fatal("invalid CAS admitted")
		}
	}
	policy := &auth.SecondFactorPolicy{Name: snapshot.Name, Etag: snapshot.Etag, UpdateTime: timestamppb.New(time.Unix(1792310000, 0))}
	for _, continuation := range []auth.SecondFactorSessionContinuation{0, 1, 2, 99} {
		_, err := AdmitAPIChangeSecondFactorPolicyResponse(&auth.ChangeSecondFactorPolicyResponse{Policy: policy, Continuation: continuation})
		if (err == nil) != (continuation == 1 || continuation == 2) {
			t.Fatal("continuation admission mismatch", continuation)
		}
	}
	for _, change := range []func(*auth.SecondFactorPolicy){
		func(v *auth.SecondFactorPolicy) { v.Etag = "" },
		func(v *auth.SecondFactorPolicy) { v.UpdateTime = nil },
		func(v *auth.SecondFactorPolicy) { v.UpdateTime.Nanos = 1_000_000_000 },
		func(v *auth.SecondFactorPolicy) { v.Name = "secondFactorPolicy" },
	} {
		value := proto.Clone(policy).(*auth.SecondFactorPolicy)
		change(value)
		if _, err := AdmitAPISecondFactorPolicy(value); err == nil {
			t.Fatal("invalid policy metadata admitted")
		}
	}
}
