package contracts

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"testing"

	wire "github.com/damione1/Homeward/gen/home/configuration/v1"
	"google.golang.org/protobuf/proto"
)

func TestNHA149ConfigurationGoRustAdmissionParity(t *testing.T) {
	data, err := os.ReadFile("configuration_cases.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		columns := strings.Split(line, "\t")
		if len(columns) != 9 {
			t.Fatal("invalid shared case")
		}
		t.Run(columns[0], func(t *testing.T) {
			integer := func(n int) int64 {
				value, err := strconv.ParseInt(columns[n], 10, 64)
				if err != nil {
					t.Fatal(err)
				}
				return value
			}
			id := columns[1]
			if id == "-" {
				id = ""
			}
			request := &wire.WriteDocumentRequest{Id: &wire.DocumentId{Value: id}, Expected: &wire.DocumentVersion{Revision: integer(2), SourceSha256: bytes.Repeat([]byte{byte(integer(4))}, int(integer(3)))}, ExpectedWorkspaceSha256: bytes.Repeat([]byte{byte(integer(6))}, int(integer(5))), ExactYamlSource: bytes.Repeat([]byte{'x'}, int(integer(7)))}
			admitted := AdmitConfigurationRequest(request) == nil
			if admitted != (columns[8] == "true") {
				t.Fatal("Go admission disagrees with shared parity case")
			}
		})
	}
}
func TestNHA149ConfigurationRejectsNestedUnknownAndTypedNil(t *testing.T) {
	var request *wire.ReadDocumentRequest
	if AdmitConfigurationRequest(request) == nil {
		t.Fatal("typed nil admitted")
	}
	request = &wire.ReadDocumentRequest{Id: &wire.DocumentId{Value: "operational.main"}}
	if AdmitConfigurationRequest(request) != nil {
		t.Fatal("valid identity rejected")
	}
	request.Id.ProtoReflect().SetUnknown([]byte{0x10, 0x01})
	if AdmitConfigurationRequest(request) == nil {
		t.Fatal("nested future field operationally admitted")
	}
	encoded, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var decoded wire.ReadDocumentRequest
	if err := proto.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal("wire compatibility should preserve unknown fields", err)
	}
	if AdmitConfigurationRequest(&decoded) == nil {
		t.Fatal("compatible decoding bypassed admission")
	}
}
func TestNHA149ConfigurationActivationGoRustParity(t *testing.T) {
	data, err := os.ReadFile("configuration_activation_cases.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		columns := strings.Split(line, "\t")
		if len(columns) != 5 {
			t.Fatal("invalid shared activation case")
		}
		t.Run(columns[0], func(t *testing.T) {
			integer := func(n int) int64 {
				value, err := strconv.ParseInt(columns[n], 10, 32)
				if err != nil {
					t.Fatal(err)
				}
				return value
			}
			activation := &wire.ActivationResult{Outcome: wire.ActivationOutcome(integer(1)), Transaction: wire.TransactionOutcome(integer(2))}
			if revision := integer(3); revision != 0 {
				activation.GenerationRevision = &revision
			}
			admitted := AdmitConfigurationResponse(&wire.WriteDocumentResponse{Activation: activation}) == nil
			if admitted != (columns[4] == "true") {
				t.Fatal("Go activation admission disagrees with shared Rust case")
			}
		})
	}
}
