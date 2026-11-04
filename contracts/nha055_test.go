package contracts

import (
	"testing"

	configuration "github.com/damione1/Homeward/gen/home/api/v2"
	plan "github.com/damione1/Homeward/gen/home/api/v2"
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/protobuf/proto"
)

func TestNHA055ApplyContractsRejectUnknownsMissingCASAndMissingPlan(t *testing.T) {
	hash := make([]byte, 32)
	hash[0] = 1
	request := &wire.ApplyConfigurationPlanRequest{Name: "homes/synthetic/configurationPlans/p-synthetic", Etag: `"synthetic"`, Candidate: &plan.ConfigurationPlanCandidate{Family: plan.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL, Identity: "operational.main", Entry: "configuration.yaml", Files: []*plan.ConfigurationPlanSourceFile{{LogicalPath: "configuration.yaml", ExactSource: []byte("external_workers: []\n")}}}, Expected: &configuration.DocumentVersion{Revision: 1, SourceSha256: hash}, ExpectedWorkspaceSha256: hash}
	if AdmitConfigurationApplyRequest(request) != nil {
		t.Fatal("named plan with independent CAS was refused")
	}
	for _, change := range []func(*wire.ApplyConfigurationPlanRequest){
		func(v *wire.ApplyConfigurationPlanRequest) { v.Expected = nil },
		func(v *wire.ApplyConfigurationPlanRequest) { v.Expected.Revision = 0 },
		func(v *wire.ApplyConfigurationPlanRequest) { v.Expected.SourceSha256 = nil },
		func(v *wire.ApplyConfigurationPlanRequest) { v.ExpectedWorkspaceSha256 = nil },
		func(v *wire.ApplyConfigurationPlanRequest) { v.Name = "" },
		func(v *wire.ApplyConfigurationPlanRequest) { v.Etag = "" },
		func(v *wire.ApplyConfigurationPlanRequest) {
			v.Candidate.Family = plan.ConfigurationPlanSourceFamily(255)
		},
		func(v *wire.ApplyConfigurationPlanRequest) {
			v.Candidate.Files[0].ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
		},
	} {
		invalid := proto.Clone(request).(*wire.ApplyConfigurationPlanRequest)
		change(invalid)
		if AdmitConfigurationApplyRequest(invalid) == nil {
			t.Fatal("unadmitted apply crossed the transport boundary")
		}
	}
}
