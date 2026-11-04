package contracts

import (
	"errors"
	"reflect"
	"testing"
	"time"

	auth "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	native "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/operator/v1/operatorv1connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"
)

var nha146Now = time.Unix(1792314000, 0)

func TestNHA146PresenceAndSnapshot(t *testing.T) {
	var zero AdmittedAuthWire[*auth.LoginDecision]
	if zero.Valid() {
		t.Fatal("zero admitted wrapper appears valid")
	}
	if _, err := zero.Snapshot(); !errors.Is(err, ErrInvalidAuthContract) {
		t.Fatal("zero snapshot admitted")
	}
	var typedNil *auth.LoginDecision
	if _, err := AdmitAPILoginDecision(typedNil, nha146Now); !errors.Is(err, ErrInvalidAuthenticationResource) {
		t.Fatal("typed nil admitted")
	}
	typed := AdmittedAuthWire[*auth.LoginDecision]{value: typedNil}
	if typed.Valid() {
		t.Fatal("typed nil wrapper appears valid")
	}
	if _, err := typed.Snapshot(); !errors.Is(err, ErrInvalidAuthContract) {
		t.Fatal("typed nil snapshot admitted")
	}
	login := new(auth.LoginDecision)
	authResourceFixture(t, "login-challenge", login)
	admitted, err := AdmitAPILoginDecision(login, nha146Now)
	if err != nil || !admitted.Valid() {
		t.Fatal("valid login rejected")
	}
	login.GetChallenge().ChallengeId = "changed-after-admission"
	copy1, err := admitted.Snapshot()
	if err != nil || copy1.GetChallenge().ChallengeId != "challenge-a" {
		t.Fatal("admitted snapshot shared source")
	}
	copy1.GetChallenge().ChallengeId = "changed-copy"
	copy2, err := admitted.Snapshot()
	if err != nil || copy2.GetChallenge().ChallengeId != "challenge-a" {
		t.Fatal("admitted snapshots share state")
	}

	absent := new(auth.IssueMcpKeyRequest)
	authResourceFixture(t, "mcp-create-default", absent)
	if absent.Lifetime != nil {
		t.Fatal("default lifetime gained presence")
	}
	if duration, err := AdmitMcpKeyLifetime(absent.Lifetime); err != nil || duration != 30*24*time.Hour {
		t.Fatal("missing lifetime must mean 30 days")
	}
	for name, want := range map[string]time.Duration{
		"mcp-create-half-second":     500 * time.Millisecond,
		"mcp-create-one-half-second": 1500 * time.Millisecond,
	} {
		request := new(auth.IssueMcpKeyRequest)
		authResourceFixture(t, name, request)
		if duration, err := AdmitMcpKeyLifetime(request.Lifetime); err != nil || duration != want {
			t.Fatalf("%s duration=%s %v", name, duration, err)
		}
	}
	zeroDuration := new(auth.IssueMcpKeyRequest)
	authResourceFixture(t, "mcp-create-zero", zeroDuration)
	if zeroDuration.Lifetime == nil {
		t.Fatal("present zero duration lost")
	}
	if _, err := AdmitMcpKeyLifetime(zeroDuration.Lifetime); err == nil {
		t.Fatal("present zero duration admitted")
	}
	validDuration := new(auth.IssueMcpKeyRequest)
	authResourceFixture(t, "mcp-create-one-half-second", validDuration)
	unknownDuration := proto.Clone(validDuration.Lifetime).(*durationpb.Duration)
	if _, err := AdmitMcpKeyLifetime(unknownDuration); err != nil {
		t.Fatal("positive duration refused before unknown field")
	}
	unknownDuration.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
	if _, err := AdmitMcpKeyLifetime(unknownDuration); err == nil {
		t.Fatal("unknown duration field admitted")
	}
}

func TestNHA146IdentityIsolation(t *testing.T) {
	types := []reflect.Type{
		reflect.TypeOf(AccountID{}), reflect.TypeOf(SessionID{}), reflect.TypeOf(FactorCredentialID{}),
		reflect.TypeOf(InvitationID{}), reflect.TypeOf(ChallengeID{}), reflect.TypeOf(McpKeyID{}), reflect.TypeOf(InstanceID{}),
	}
	for i, source := range types {
		for j, target := range types {
			if i != j && source.ConvertibleTo(target) {
				t.Fatalf("%v converts to %v", source, target)
			}
		}
	}
	checks := []func() error{
		func() error { _, err := (AccountID{}).Value(); return err },
		func() error { _, err := (SessionID{}).Value(); return err },
		func() error { _, err := (FactorCredentialID{}).Value(); return err },
		func() error { _, err := (InvitationID{}).Value(); return err },
		func() error { _, err := (ChallengeID{}).Value(); return err },
		func() error { _, err := (McpKeyID{}).Value(); return err },
		func() error { _, err := (InstanceID{}).Value(); return err },
	}
	for _, check := range checks {
		if !errors.Is(check(), ErrInvalidAuthID) {
			t.Fatal("zero ID admitted")
		}
	}
}

func TestNHA146FactorOptionCannotDowngrade(t *testing.T) {
	login := new(auth.LoginDecision)
	authResourceFixture(t, "login-recovery-only", login)
	proof := new(auth.FactorProof)
	authResourceFixture(t, "factor-proof-fido", proof)
	if err := AdmitAPIFactorProofForChallenge(proof, login.GetChallenge(), nha146Now); err == nil {
		t.Fatal("unoffered FIDO option admitted")
	}
	authResourceFixture(t, "factor-proof-recovery", proof)
	if err := AdmitAPIFactorProofForChallenge(proof, login.GetChallenge(), nha146Now); err != nil {
		t.Fatal("offered recovery option rejected")
	}
	fidoOnly := new(auth.LoginDecision)
	authResourceFixture(t, "login-fido-only", fidoOnly)
	if err := AdmitAPIFactorProofForChallenge(proof, fidoOnly.GetChallenge(), nha146Now); err == nil {
		t.Fatal("unoffered recovery option admitted")
	}
	duplicate := new(auth.LoginDecision)
	authResourceFixture(t, "login-duplicate-options", duplicate)
	if _, err := AdmitAPILoginDecision(duplicate, nha146Now); err == nil {
		t.Fatal("duplicate challenge options admitted")
	}
}

func TestNHA146EnumCatalogueAndConnect(t *testing.T) {
	enums := native.File_home_auth_v1_auth_proto.Enums()
	counts := map[protoreflect.Name]int{
		"AccountRole": 3, "AccountStatus": 4, "ScopeFamily": 14, "ScopeOperation": 4,
		"AuthenticationSatisfaction": 4, "AuthorizationTerminationReason": 6,
	}
	if enums.Len() != len(counts) {
		t.Fatalf("auth enum count changed: %d", enums.Len())
	}
	for name, count := range counts {
		got := enums.ByName(name)
		if got == nil || got.Values().Len() != count {
			t.Fatalf("%s enum changed; review admission/conversion", name)
		}
	}
	if operatorv1connect.OperatorAccountServiceName != "home.operator.v1.OperatorAccountService" ||
		apiv2connect.AuthServiceName != "home.api.v2.AuthService" ||
		apiv2connect.McpKeyServiceName != "home.api.v2.McpKeyService" {
		t.Fatal("Connect service generated from unexpected proto")
	}
}
