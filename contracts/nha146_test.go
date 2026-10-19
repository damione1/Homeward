package contracts

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"
)

var nha146Now = time.Unix(1792314000, 0)

func authFixture(t *testing.T, name string, message proto.Message) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("auth_golden", name+".bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(data, message); err != nil {
		t.Fatal(err)
	}
	out, err := (proto.MarshalOptions{Deterministic: true}).Marshal(message)
	if err != nil || !bytes.Equal(out, data) {
		t.Fatalf("%s wire roundtrip: %v", name, err)
	}
	return data
}

func TestNHA146CrossLanguageFixtures(t *testing.T) {
	index, err := os.ReadFile(filepath.Join("auth_golden", "index.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, line := range strings.Split(string(index), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			t.Fatalf("invalid fixture row %q", line)
		}
		name, kind, expected := parts[0], parts[1], parts[2]
		if expected != "true" && expected != "false" {
			t.Fatalf("invalid expectation %q", line)
		}
		seen++
		t.Run(name, func(t *testing.T) {
			var admitted bool
			switch kind {
			case "Account":
				v := new(auth.Account)
				authFixture(t, name, v)
				_, err := AdmitAccount(v)
				admitted = err == nil
			case "LoginDecision":
				v := new(auth.LoginDecision)
				authFixture(t, name, v)
				_, err := AdmitLoginDecision(v, nha146Now)
				admitted = err == nil
			case "AuthError":
				v := new(auth.AuthError)
				authFixture(t, name, v)
				_, err := AuthErrorConnectCode(v)
				admitted = err == nil
			case "McpKey":
				v := new(auth.McpKey)
				authFixture(t, name, v)
				_, err := AdmitMcpKey(v, nha146Now)
				admitted = err == nil
			case "CreateMcpKeyRequest":
				v := new(auth.CreateMcpKeyRequest)
				authFixture(t, name, v)
				admitted = AdmitMcpKeyCreateRequest(v) == nil
			case "BeginFactorEnrollmentRequest":
				v := new(auth.BeginFactorEnrollmentRequest)
				authFixture(t, name, v)
				admitted = AdmitBeginFactorEnrollment(v) == nil
			case "FactorProof":
				v := new(auth.FactorProof)
				authFixture(t, name, v)
				login := new(auth.LoginDecision)
				authFixture(t, "login-challenge", login)
				admitted = AdmitFactorProofForChallenge(v, login.GetChallenge(), nha146Now) == nil
			case "CompleteLoginRequest":
				v := new(auth.CompleteLoginRequest)
				authFixture(t, name, v)
				login := new(auth.LoginDecision)
				authFixture(t, "login-challenge", login)
				admitted = AdmitCompleteLoginRequest(v, login.GetChallenge(), nha146Now) == nil
			default:
				t.Fatalf("unhandled fixture message %q", kind)
			}
			if admitted != (expected == "true") {
				t.Fatalf("admission=%v want %s", admitted, expected)
			}
		})
	}
	if seen != 37 {
		t.Fatalf("fixture inventory changed: %d", seen)
	}
}

func TestNHA146PresenceAndSnapshot(t *testing.T) {
	var zero AdmittedAuthWire[*auth.LoginDecision]
	if zero.Valid() {
		t.Fatal("zero admitted wrapper appears valid")
	}
	if _, err := zero.Snapshot(); !errors.Is(err, ErrInvalidAuthContract) {
		t.Fatal("zero snapshot admitted")
	}
	var typedNil *auth.LoginDecision
	if _, err := AdmitLoginDecision(typedNil, nha146Now); !errors.Is(err, ErrInvalidAuthContract) {
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
	authFixture(t, "login-challenge", login)
	admitted, err := AdmitLoginDecision(login, nha146Now)
	if err != nil || !admitted.Valid() {
		t.Fatal("valid login rejected")
	}
	login.GetChallenge().Id.Value = "changed-after-admission"
	copy1, err := admitted.Snapshot()
	if err != nil || copy1.GetChallenge().Id.Value != "challenge-a" {
		t.Fatal("admitted snapshot shared source")
	}
	copy1.GetChallenge().Id.Value = "changed-copy"
	copy2, err := admitted.Snapshot()
	if err != nil || copy2.GetChallenge().Id.Value != "challenge-a" {
		t.Fatal("admitted snapshots share state")
	}

	absent := new(auth.CreateMcpKeyRequest)
	authFixture(t, "mcp-create-default", absent)
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
		request := new(auth.CreateMcpKeyRequest)
		authFixture(t, name, request)
		if duration, err := AdmitMcpKeyLifetime(request.Lifetime); err != nil || duration != want {
			t.Fatalf("%s duration=%s %v", name, duration, err)
		}
	}
	zeroDuration := new(auth.CreateMcpKeyRequest)
	authFixture(t, "mcp-create-zero", zeroDuration)
	if zeroDuration.Lifetime == nil {
		t.Fatal("present zero duration lost")
	}
	if _, err := AdmitMcpKeyLifetime(zeroDuration.Lifetime); err == nil {
		t.Fatal("present zero duration admitted")
	}
	validDuration := new(auth.CreateMcpKeyRequest)
	authFixture(t, "mcp-create-one-half-second", validDuration)
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
	authFixture(t, "login-recovery-only", login)
	proof := new(auth.FactorProof)
	authFixture(t, "factor-proof-fido", proof)
	if err := AdmitFactorProofForChallenge(proof, login.GetChallenge(), nha146Now); err == nil {
		t.Fatal("unoffered FIDO option admitted")
	}
	authFixture(t, "factor-proof-recovery", proof)
	if err := AdmitFactorProofForChallenge(proof, login.GetChallenge(), nha146Now); err != nil {
		t.Fatal("offered recovery option rejected")
	}
	fidoOnly := new(auth.LoginDecision)
	authFixture(t, "login-fido-only", fidoOnly)
	if err := AdmitFactorProofForChallenge(proof, fidoOnly.GetChallenge(), nha146Now); err == nil {
		t.Fatal("unoffered recovery option admitted")
	}
	duplicate := new(auth.LoginDecision)
	authFixture(t, "login-duplicate-options", duplicate)
	if _, err := AdmitLoginDecision(duplicate, nha146Now); err == nil {
		t.Fatal("duplicate challenge options admitted")
	}
}

func TestNHA146EnumCatalogueAndConnect(t *testing.T) {
	enums := auth.File_home_auth_v1_auth_proto.Enums()
	counts := map[protoreflect.Name]int{
		"AccountRole": 3, "AccountStatus": 4, "ScopeFamily": 14, "ScopeOperation": 4,
		"FactorKind": 2, "CredentialAudience": 3, "AuthErrorCode": 7,
		"AuthenticationFailure": 4, "PermissionFailure": 4, "ConflictFailure": 5,
		"ExpiredResource": 6, "AuthField": 17,
		"PolicyRightsChange": 3, "AuthorizationPolicyErrorCode": 6, "AuthorizationTerminationReason": 6,
		"AuthenticationSatisfaction": 4, "SecondFactorSessionContinuation": 3, "SecondFactorPolicyErrorCode": 4,
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
	if authv1connect.OperatorAccountServiceName != "home.auth.v1.OperatorAccountService" ||
		authv1connect.AuthServiceName != "home.auth.v1.AuthService" ||
		authv1connect.McpKeyServiceName != "home.auth.v1.McpKeyService" {
		t.Fatal("Connect service generated from unexpected proto")
	}
}

func TestNHA146ErrorMapping(t *testing.T) {
	cases := []struct {
		resource auth.ExpiredResource
		want     connect.Code
	}{
		{auth.ExpiredResource_EXPIRED_RESOURCE_INVITATION, connect.CodeFailedPrecondition},
		{auth.ExpiredResource_EXPIRED_RESOURCE_CHALLENGE, connect.CodeFailedPrecondition},
		{auth.ExpiredResource_EXPIRED_RESOURCE_SESSION, connect.CodeUnauthenticated},
		{auth.ExpiredResource_EXPIRED_RESOURCE_ACCESS_TOKEN, connect.CodeUnauthenticated},
		{auth.ExpiredResource_EXPIRED_RESOURCE_MCP_KEY, connect.CodeUnauthenticated},
	}
	for _, tc := range cases {
		v := &auth.AuthError{Code: auth.AuthErrorCode_AUTH_ERROR_CODE_EXPIRED, Detail: &auth.AuthError_Expired{Expired: tc.resource}}
		got, err := AuthErrorConnectCode(v)
		if err != nil || got != tc.want {
			t.Fatalf("expired %v => %v, %v", tc.resource, got, err)
		}
	}
}
