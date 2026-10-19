package contracts

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestNHA050PolicyCrossLanguageFixtures(t *testing.T) {
	index, err := os.ReadFile(filepath.Join("authorization_golden", "index.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(index)), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			t.Fatal("invalid fixture index")
		}
		t.Run(parts[0], func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("authorization_golden", parts[0]+".bin"))
			if err != nil {
				t.Fatal(err)
			}
			var value proto.Message
			switch parts[1] {
			case "ReplacePolicyRequest":
				value = new(auth.ReplacePolicyRequest)
			case "GetPolicyResponse":
				value = new(auth.GetPolicyResponse)
			case "AuthorizationTermination":
				value = new(auth.AuthorizationTermination)
			case "AuthorizationPolicyError":
				value = new(auth.AuthorizationPolicyError)
			default:
				t.Fatal("unhandled fixture")
			}
			if err := proto.Unmarshal(data, value); err != nil {
				t.Fatal(err)
			}
			out, err := (proto.MarshalOptions{Deterministic: true}).Marshal(value)
			if err != nil || !bytes.Equal(data, out) {
				t.Fatal("fixture wire drift", err)
			}
			switch v := value.(type) {
			case *auth.ReplacePolicyRequest:
				err = AdmitReplacePolicyRequest(v)
			case *auth.GetPolicyResponse:
				_, err = AdmitGetPolicyResponse(v)
			case *auth.AuthorizationTermination:
				_, err = AdmitAuthorizationTermination(v)
			case *auth.AuthorizationPolicyError:
				_, err = AuthorizationPolicyErrorConnectCode(v)
			}
			if (err == nil) != (parts[2] == "true") {
				t.Fatal("cross-language admission disagrees", err)
			}
		})
	}
}

func TestNHA050PolicyWireCASPresenceAndCopies(t *testing.T) {
	account := &auth.AccountId{Value: "account"}
	version := &auth.PolicyVersion{Revision: 1, SourceSha256: bytes.Repeat([]byte{1}, 32)}
	request := &auth.ReplacePolicyRequest{AccountId: account, YamlSource: []byte("source"), Precondition: &auth.ReplacePolicyRequest_Expected{Expected: version}}
	if err := AdmitReplacePolicyRequest(request); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*auth.ReplacePolicyRequest){
		func(v *auth.ReplacePolicyRequest) { v.Precondition = nil },
		func(v *auth.ReplacePolicyRequest) { v.Precondition = (*auth.ReplacePolicyRequest_Expected)(nil) },
		func(v *auth.ReplacePolicyRequest) { v.Precondition = &auth.ReplacePolicyRequest_ExpectAbsent{} },
		func(v *auth.ReplacePolicyRequest) { v.GetExpected().Revision = 0 },
		func(v *auth.ReplacePolicyRequest) { v.GetExpected().Revision = math.MaxInt64 + 1 },
		func(v *auth.ReplacePolicyRequest) { v.GetExpected().SourceSha256 = make([]byte, 32) },
		func(v *auth.ReplacePolicyRequest) { v.GetExpected().SourceSha256 = []byte{1} },
		func(v *auth.ReplacePolicyRequest) { v.YamlSource = []byte{0xff} },
		func(v *auth.ReplacePolicyRequest) { v.YamlSource = make([]byte, MaxAuthorizationPolicySourceBytes+1) },
		func(v *auth.ReplacePolicyRequest) { v.GetExpected().ProtoReflect().SetUnknown([]byte{0x18, 1}) },
	} {
		v := proto.Clone(request).(*auth.ReplacePolicyRequest)
		mutate(v)
		if err := AdmitReplacePolicyRequest(v); err == nil {
			t.Fatal("invalid policy CAS admitted")
		}
	}
	request.Precondition = &auth.ReplacePolicyRequest_ExpectAbsent{ExpectAbsent: &emptypb.Empty{}}
	request.YamlSource = bytes.Repeat([]byte{'x'}, MaxAuthorizationPolicySourceBytes)
	if err := AdmitReplacePolicyRequest(request); err != nil {
		t.Fatal("explicit absence or exact source limit rejected", err)
	}
	response := &auth.GetPolicyResponse{Result: &auth.GetPolicyResponse_Present{Present: &auth.PolicySnapshot{AccountId: account, YamlSource: []byte("source"), Schema: AuthorizationPolicySchema, SyntaxProfile: AuthorizationPolicySyntaxProfile, Version: version, UpdatedAt: timestamppb.New(time.Unix(1, 0))}}}
	admitted, err := AdmitGetPolicyResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	response.GetPresent().YamlSource[0] = 'X'
	copy, err := admitted.Snapshot()
	if err != nil || string(copy.GetPresent().YamlSource) != "source" {
		t.Fatal("source alias", err)
	}
	copy.GetPresent().Version.SourceSha256[0] = 8
	again, _ := admitted.Snapshot()
	if again.GetPresent().Version.SourceSha256[0] != 1 {
		t.Fatal("hash alias")
	}
	if _, err := AdmitGetPolicyResponse(&auth.GetPolicyResponse{}); err == nil {
		t.Fatal("missing result admitted")
	}
	if _, err := AdmitGetPolicyResponse(&auth.GetPolicyResponse{Result: (*auth.GetPolicyResponse_Present)(nil)}); err == nil {
		t.Fatal("typed nil result admitted")
	}
	if _, err := AdmitGetPolicyResponse(&auth.GetPolicyResponse{Result: &auth.GetPolicyResponse_Absent{Absent: &emptypb.Empty{}}}); err != nil {
		t.Fatal(err)
	}
}

func TestNHA050ClosedPolicyErrorsAndTermination(t *testing.T) {
	for _, n := range auth.AuthorizationPolicyErrorCode_name {
		code := auth.AuthorizationPolicyErrorCode(auth.AuthorizationPolicyErrorCode_value[n])
		path := ""
		if code == auth.AuthorizationPolicyErrorCode_AUTHORIZATION_POLICY_ERROR_CODE_INVALID_SOURCE || code == auth.AuthorizationPolicyErrorCode_AUTHORIZATION_POLICY_ERROR_CODE_INVALID_VERSION {
			path = "grants[0].scope"
		}
		_, err := AuthorizationPolicyErrorConnectCode(&auth.AuthorizationPolicyError{Code: code, FieldPath: path})
		if (err == nil) != (code != 0) {
			t.Fatalf("code %v: %v", code, err)
		}
	}
	for _, reason := range []auth.AuthorizationTerminationReason{0, 1, 2, 3, 4, 5, 999} {
		_, err := AdmitAuthorizationTermination(&auth.AuthorizationTermination{Reason: reason})
		if (err == nil) != (reason > 0 && reason <= 5) {
			t.Fatalf("termination %v: %v", reason, err)
		}
	}
	if _, err := AuthorizationPolicyErrorConnectCode(&auth.AuthorizationPolicyError{Code: auth.AuthorizationPolicyErrorCode_AUTHORIZATION_POLICY_ERROR_CODE_INVALID_SOURCE, FieldPath: "grants[0].private-user-id/secret"}); err == nil {
		t.Fatal("unsafe field path admitted")
	}
}
