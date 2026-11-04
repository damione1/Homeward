package contracts

import (
	"testing"

	configuration "github.com/damione1/Homeward/gen/home/api/v2"
	plan "github.com/damione1/Homeward/gen/home/api/v2"
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

func nha055PolicyHistoryRequest() *wire.RollbackConfigurationRequest {
	hash := make([]byte, 32)
	hash[0] = 1
	return &wire.RollbackConfigurationRequest{Name: "homes/synthetic/configurationDocuments/operational", Expected: &configuration.DocumentVersion{Revision: 3, SourceSha256: hash}, ExpectedWorkspaceSha256: hash, Family: plan.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTHORIZATION, Identity: "target-account", HistoricalPolicyVersion: &configuration.DocumentVersion{Revision: 1, SourceSha256: hash}, Sources: []*wire.SourcePrecondition{{Id: &configuration.DocumentId{Value: "authorization:fixture"}, LogicalPath: "authorization/fixture/policy.yaml", SqlState: &wire.SourcePrecondition_Existing{Existing: &configuration.DocumentVersion{Revision: 2, SourceSha256: hash}}, WorkspaceState: &wire.SourcePrecondition_WorkspaceAbsent{WorkspaceAbsent: &emptypb.Empty{}}}}}
}

func TestNHA055PolicyHistorySelectorXORAndFamilyBoundary(t *testing.T) {
	request := nha055PolicyHistoryRequest()
	if err := AdmitConfigurationApplyRequest(request); err != nil {
		t.Fatal("documentary policy version unexpectedly requires a generation", err)
	}
	history := &configuration.ActiveGeneration{Revision: 4, ManifestSha256: request.ExpectedWorkspaceSha256}
	legacy := proto.Clone(request).(*wire.RollbackConfigurationRequest)
	legacy.HistoricalPolicyVersion = nil
	legacy.Historical = history
	if err := AdmitConfigurationApplyRequest(legacy); err != nil {
		t.Fatal("legacy explicit policy delta selector was removed", err)
	}
	for _, change := range []func(*wire.RollbackConfigurationRequest){
		func(v *wire.RollbackConfigurationRequest) { v.HistoricalPolicyVersion = nil },
		func(v *wire.RollbackConfigurationRequest) { v.Historical = history },
		func(v *wire.RollbackConfigurationRequest) { v.HistoricalPolicyVersion.Revision = 0 },
		func(v *wire.RollbackConfigurationRequest) { v.HistoricalPolicyVersion.SourceSha256 = nil },
		func(v *wire.RollbackConfigurationRequest) {
			v.HistoricalPolicyVersion.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
		},
	} {
		invalid := proto.Clone(request).(*wire.RollbackConfigurationRequest)
		change(invalid)
		if AdmitConfigurationApplyRequest(invalid) == nil {
			t.Fatal("ambiguous or malformed historical policy selector admitted")
		}
	}
	for _, family := range []plan.ConfigurationPlanSourceFamily{plan.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_UNSPECIFIED, plan.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL, plan.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DECLARATIVE_V1, plan.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DECLARATIVE_V2, plan.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OWNERSHIP, plan.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DASHBOARD, plan.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTOMATION, plan.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_SCRIPT, plan.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_SCENE} {
		other := proto.Clone(legacy).(*wire.RollbackConfigurationRequest)
		other.Family = family
		if family == plan.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_UNSPECIFIED {
			other.Identity = ""
			other.Sources = nil
		}
		if family == plan.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL {
			other.Identity = "operational.main"
			other.Sources = nil
		}
		if err := AdmitConfigurationApplyRequest(other); err != nil {
			t.Fatalf("existing generation selector changed for family %v: %v", family, err)
		}
		other.HistoricalPolicyVersion = request.HistoricalPolicyVersion
		if AdmitConfigurationApplyRequest(other) == nil {
			t.Fatalf("family %v accepted a policy selector alongside its generation", family)
		}
		other.Historical = nil
		if AdmitConfigurationApplyRequest(other) == nil {
			t.Fatalf("family %v accepted a policy version without a generation", family)
		}
	}
}
