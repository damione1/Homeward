package contracts

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestNHA050NativeTerminationCrossLanguageFixtures(t *testing.T) {
	index, err := os.ReadFile(filepath.Join("authorization_golden", "index.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(index)), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			t.Fatal("invalid fixture index")
		}
		// Only termination is still a current native protocol. Retired policy
		// bytes are checked by the historical SHA manifest, without a decoder.
		if parts[1] != "AuthorizationTermination" {
			continue
		}
		t.Run(parts[0], func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("authorization_golden", parts[0]+".bin"))
			if err != nil {
				t.Fatal(err)
			}
			value := new(auth.AuthorizationTermination)
			if err := proto.Unmarshal(data, value); err != nil {
				t.Fatal(err)
			}
			out, err := (proto.MarshalOptions{Deterministic: true}).Marshal(value)
			if err != nil || !bytes.Equal(data, out) {
				t.Fatal("native fixture wire drift", err)
			}
			_, err = AdmitAuthorizationTermination(value)
			if (err == nil) != (parts[2] == "true") {
				t.Fatal("cross-language admission disagrees", err)
			}
		})
	}
}

func TestNHA050PolicyWireCASPresenceAndCopies(t *testing.T) {
	name := "homes/home-a/accounts/account-a/authorizationPolicy"
	request := &api.ReplaceAuthorizationPolicyRequest{Name: name, YamlSource: []byte("exact source\n"), Precondition: &api.ReplaceAuthorizationPolicyRequest_ExpectedEtag{ExpectedEtag: `W/"opaque-native-version"`}}
	if err := AdmitAPIReplaceAuthorizationPolicy(request); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*api.ReplaceAuthorizationPolicyRequest){
		func(v *api.ReplaceAuthorizationPolicyRequest) { v.Precondition = nil },
		func(v *api.ReplaceAuthorizationPolicyRequest) {
			v.Precondition = (*api.ReplaceAuthorizationPolicyRequest_ExpectedEtag)(nil)
		},
		func(v *api.ReplaceAuthorizationPolicyRequest) {
			v.Precondition = &api.ReplaceAuthorizationPolicyRequest_ExpectAbsent{}
		},
		func(v *api.ReplaceAuthorizationPolicyRequest) {
			v.Precondition = &api.ReplaceAuthorizationPolicyRequest_ExpectedEtag{}
		},
		func(v *api.ReplaceAuthorizationPolicyRequest) {
			v.Precondition = &api.ReplaceAuthorizationPolicyRequest_ExpectedEtag{ExpectedEtag: strings.Repeat("x", 129)}
		},
		func(v *api.ReplaceAuthorizationPolicyRequest) { v.Name = "account-a" },
		func(v *api.ReplaceAuthorizationPolicyRequest) { v.YamlSource = []byte{0xff} },
		func(v *api.ReplaceAuthorizationPolicyRequest) {
			v.YamlSource = make([]byte, MaxAuthorizationPolicySourceBytes+1)
		},
		func(v *api.ReplaceAuthorizationPolicyRequest) { v.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 1}) },
	} {
		value := proto.Clone(request).(*api.ReplaceAuthorizationPolicyRequest)
		mutate(value)
		if err := AdmitAPIReplaceAuthorizationPolicy(value); err == nil {
			t.Fatal("invalid policy CAS admitted")
		}
	}
	request.Precondition = &api.ReplaceAuthorizationPolicyRequest_ExpectAbsent{ExpectAbsent: &emptypb.Empty{}}
	request.YamlSource = bytes.Repeat([]byte{'x'}, MaxAuthorizationPolicySourceBytes)
	if err := AdmitAPIReplaceAuthorizationPolicy(request); err != nil {
		t.Fatal("explicit absence or exact source limit rejected", err)
	}
	response := &api.AuthorizationPolicy{Name: name, YamlSource: []byte("exact source\n"), Schema: AuthorizationPolicySchema, SyntaxProfile: AuthorizationPolicySyntaxProfile, Origin: api.AuthorizationPolicyOrigin_AUTHORIZATION_POLICY_ORIGIN_STORED, Etag: `W/"version"`, UpdateTime: timestamppb.New(time.Unix(1, 0))}
	admitted, err := AdmitAPIAuthorizationPolicy(response)
	if err != nil {
		t.Fatal(err)
	}
	response.YamlSource[0] = 'X'
	copy, err := admitted.Snapshot()
	if err != nil || string(copy.YamlSource) != "exact source\n" {
		t.Fatal("source alias", err)
	}
	copy.YamlSource[0] = 'Y'
	again, _ := admitted.Snapshot()
	if string(again.YamlSource) != "exact source\n" {
		t.Fatal("returned source alias")
	}
	defaultPolicy := proto.Clone(again).(*api.AuthorizationPolicy)
	defaultPolicy.Origin = api.AuthorizationPolicyOrigin_AUTHORIZATION_POLICY_ORIGIN_DEFAULT
	defaultPolicy.Etag = ""
	defaultPolicy.UpdateTime = nil
	if _, err := AdmitAPIAuthorizationPolicy(defaultPolicy); err != nil {
		t.Fatal("effective default rejected", err)
	}
	for _, mutate := range []func(*api.AuthorizationPolicy){
		func(v *api.AuthorizationPolicy) { v.Etag = "fabricated" },
		func(v *api.AuthorizationPolicy) { v.UpdateTime = timestamppb.Now() },
		func(v *api.AuthorizationPolicy) {
			v.Origin = api.AuthorizationPolicyOrigin_AUTHORIZATION_POLICY_ORIGIN_STORED
		},
		func(v *api.AuthorizationPolicy) { v.Origin = 0 },
		func(v *api.AuthorizationPolicy) { v.Origin = 99 },
		func(v *api.AuthorizationPolicy) { v.YamlSource = nil },
	} {
		value := proto.Clone(defaultPolicy).(*api.AuthorizationPolicy)
		mutate(value)
		if _, err := AdmitAPIAuthorizationPolicy(value); err == nil {
			t.Fatal("invalid default metadata admitted")
		}
	}
}

func TestNHA050ClosedNativeTermination(t *testing.T) {
	for _, reason := range []auth.AuthorizationTerminationReason{0, 1, 2, 3, 4, 5, 999} {
		_, err := AdmitAuthorizationTermination(&auth.AuthorizationTermination{Reason: reason})
		if (err == nil) != (reason > 0 && reason <= 5) {
			t.Fatalf("termination %v: %v", reason, err)
		}
	}
}
