package contracts

import (
	"testing"

	configuration "github.com/damione1/Homeward/gen/home/configuration/v1"
	wire "github.com/damione1/Homeward/gen/home/configurationapply/v1"
	plan "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	"google.golang.org/protobuf/proto"
)

func TestNHA055ApplyContractsRejectUnknownsMissingCASAndExplicitEmptyReceipt(t *testing.T) {
	hash := make([]byte, 32)
	hash[0] = 1
	request := &wire.ApplyRequest{Candidate: &plan.Candidate{Family: plan.SourceFamily_SOURCE_FAMILY_OPERATIONAL, Identity: "operational.main", Entry: "configuration.yaml", Files: []*plan.SourceFile{{LogicalPath: "configuration.yaml", ExactSource: []byte("external_workers: []\n")}}}, Expected: &configuration.DocumentVersion{Revision: 1, SourceSha256: hash}, ExpectedWorkspaceSha256: hash}
	if AdmitConfigurationApplyRequest(request) != nil {
		t.Fatal("optional absent receipt made ordinary YAML editing unavailable")
	}
	for _, change := range []func(*wire.ApplyRequest){
		func(v *wire.ApplyRequest) { v.Expected = nil },
		func(v *wire.ApplyRequest) { v.Expected.Revision = 0 },
		func(v *wire.ApplyRequest) { v.Expected.SourceSha256 = nil },
		func(v *wire.ApplyRequest) { v.ExpectedWorkspaceSha256 = nil },
		func(v *wire.ApplyRequest) { value := ""; v.Receipt = &value },
		func(v *wire.ApplyRequest) { v.Candidate.Family = plan.SourceFamily(255) },
		func(v *wire.ApplyRequest) { v.Candidate.Files[0].ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01}) },
	} {
		invalid := proto.Clone(request).(*wire.ApplyRequest)
		change(invalid)
		if AdmitConfigurationApplyRequest(invalid) == nil {
			t.Fatal("unadmitted apply crossed the transport boundary")
		}
	}
}
