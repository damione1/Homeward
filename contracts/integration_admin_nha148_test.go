package contracts

import (
	"os"
	"strconv"
	"strings"
	"testing"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
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
		if len(columns) != 7 {
			t.Fatal("invalid corpus")
		}
		stateVal, err := strconv.Atoi(columns[2])
		if err != nil {
			t.Fatal(err)
		}
		var paths []string
		if columns[4] != "" {
			paths = strings.Split(columns[4], ",")
		}
		r := &api.UpdateIntegrationInstanceRequest{
			IntegrationInstance: &api.IntegrationInstance{
				Name:         columns[1],
				DesiredState: api.IntegrationDesiredState(stateVal),
				Etag:         columns[3],
			},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
			RequestId:  columns[5],
		}
		if (AdmitIntegrationAdminRequest(r) == nil) != (columns[6] == "true") {
			t.Fatal(columns[0])
		}
	}
}
func TestNHA148UnknownNestedWireRemainsDecodableButNotAdmitted(t *testing.T) {
	request := &api.GetIntegrationInstanceRequest{Name: "homes/home-1/integrationInstances/i-nha148"}
	raw, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, 0x10, 0x01)
	var decoded api.GetIntegrationInstanceRequest
	if proto.Unmarshal(raw, &decoded) != nil {
		t.Fatal("wire compatibility lost")
	}
	if AdmitIntegrationAdminRequest(&decoded) == nil {
		t.Fatal("future wire became operational")
	}
	var absent *api.CreateIntegrationInstanceRequest
	if AdmitIntegrationAdminRequest(absent) == nil {
		t.Fatal("typed nil admitted")
	}
}
