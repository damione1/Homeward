package contracts

import (
	"bytes"
	cfg "github.com/damione1/Homeward/gen/home/configuration/v1"
	wire "github.com/damione1/Homeward/gen/home/integrationadmin/v1"
	"google.golang.org/protobuf/proto"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestNHA148GoRustSharedMutationBoundary(t *testing.T) {
	raw, err := os.ReadFile("integration_admin_cases.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		columns := strings.Split(line, "\t")
		if len(columns) != 8 {
			t.Fatal("invalid corpus")
		}
		n := func(i int) int {
			value, err := strconv.Atoi(columns[i])
			if err != nil {
				t.Fatal(err)
			}
			return value
		}
		r := &wire.SetInstanceEnabledRequest{IntegrationInstanceId: "nha148-instance", DesiredState: wire.DesiredState(n(1)), Precondition: &wire.MutationPrecondition{Expected: &cfg.DocumentVersion{Revision: int64(n(2)), SourceSha256: bytes.Repeat([]byte{byte(n(4))}, n(3))}, ExpectedWorkspaceSha256: bytes.Repeat([]byte{byte(n(6))}, n(5))}}
		if (AdmitIntegrationAdminRequest(r) == nil) != (columns[7] == "true") {
			t.Fatal(columns[0])
		}
	}
}
func TestNHA148UnknownNestedWireRemainsDecodableButNotAdmitted(t *testing.T) {
	request := &wire.GetInstanceRequest{IntegrationInstanceId: "nha148"}
	raw, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, 0x10, 0x01)
	var decoded wire.GetInstanceRequest
	if proto.Unmarshal(raw, &decoded) != nil {
		t.Fatal("wire compatibility lost")
	}
	if AdmitIntegrationAdminRequest(&decoded) == nil {
		t.Fatal("future wire became operational")
	}
	var absent *wire.CreateInstanceRequest
	if AdmitIntegrationAdminRequest(absent) == nil {
		t.Fatal("typed nil admitted")
	}
}
