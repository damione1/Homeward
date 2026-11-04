package contracts

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	auth "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/protobuf/proto"
)

func authResourceFixture(t *testing.T, name string, message proto.Message) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("auth_resource_golden", name+".bin"))
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

func TestNHA203AuthenticationResourceCrossLanguageFixtures(t *testing.T) {
	index, err := os.ReadFile(filepath.Join("auth_resource_golden", "index.tsv"))
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
				authResourceFixture(t, name, v)
				_, err := AdmitAPIAccount(v)
				admitted = err == nil
			case "LoginDecision":
				v := new(auth.LoginDecision)
				authResourceFixture(t, name, v)
				_, err := AdmitAPILoginDecision(v, nha146Now)
				admitted = err == nil
			case "McpKey":
				v := new(auth.McpKey)
				authResourceFixture(t, name, v)
				err := AdmitAPIMcpKey(v, nha146Now)
				admitted = err == nil
			case "IssueMcpKeyRequest":
				v := new(auth.IssueMcpKeyRequest)
				authResourceFixture(t, name, v)
				admitted = AdmitAPIMcpKeyIssue(v) == nil
			case "BeginFactorEnrollmentRequest":
				v := new(auth.BeginFactorEnrollmentRequest)
				authResourceFixture(t, name, v)
				admitted = AdmitAPIBeginFactorEnrollment(v) == nil
			case "FactorProof":
				v := new(auth.FactorProof)
				authResourceFixture(t, name, v)
				login := new(auth.LoginDecision)
				authResourceFixture(t, "login-challenge", login)
				admitted = AdmitAPIFactorProofForChallenge(v, login.GetChallenge(), nha146Now) == nil
			case "CompleteLoginRequest":
				v := new(auth.CompleteLoginRequest)
				authResourceFixture(t, name, v)
				login := new(auth.LoginDecision)
				authResourceFixture(t, "login-challenge", login)
				admitted = AdmitAPICompleteLoginRequest(v, login.GetChallenge(), nha146Now) == nil
			default:
				t.Fatalf("unhandled fixture message %q", kind)
			}
			if admitted != (expected == "true") {
				t.Fatalf("admission=%v want %s", admitted, expected)
			}
		})
	}
	if seen != 32 {
		t.Fatalf("fixture inventory changed: %d", seen)
	}
}
