package contracts

import (
	"testing"

	wire "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	"google.golang.org/protobuf/proto"
)

func TestNHA053EcobeePublicFieldRefusesPrivatePathsAndOtherFamilies(t *testing.T) {
	plan := nha053ContractPlan()
	plan.Family = wire.SourceFamily_SOURCE_FAMILY_OPERATIONAL
	plan.Identity = "operational.main"
	plan.SchemaVersion = "homeward.operational/1"
	plan.Effect = wire.Effect_EFFECT_CONFIGURATION
	plan.Changes = []*wire.Change{{Effect: wire.Effect_EFFECT_CONFIGURATION, Operation: wire.ChangeOperation_CHANGE_OPERATION_REPLACED, Field: "ecobee"}}
	if err := AdmitConfigurationPlanResponse(&wire.BuildPlanResponse{Plan: plan}); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"ecobee.address", "ecobee.credential_secret", "ecobee.nha053-private-ecobee", "ecobee_future"} {
		copy := proto.Clone(plan).(*wire.Plan)
		copy.Changes[0].Field = field
		if AdmitConfigurationPlanResponse(&wire.BuildPlanResponse{Plan: copy}) == nil {
			t.Fatal("private/future Ecobee field admitted")
		}
	}
	for _, fixture := range []struct {
		family wire.SourceFamily
		schema string
		effect wire.Effect
	}{
		{wire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V1, "ha-declarative-1", wire.Effect_EFFECT_CONFIGURATION},
		{wire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V2, "ha-declarative-2", wire.Effect_EFFECT_CONFIGURATION},
		{wire.SourceFamily_SOURCE_FAMILY_AUTHORIZATION, "homeward.authorization/1", wire.Effect_EFFECT_HUMAN_RIGHTS},
		{wire.SourceFamily_SOURCE_FAMILY_OWNERSHIP, "homeward.heating-ownership/1", wire.Effect_EFFECT_TECHNICAL_OWNERSHIP},
		{wire.SourceFamily_SOURCE_FAMILY_DASHBOARD, "homeward-dashboard-1", wire.Effect_EFFECT_DASHBOARD},
		{wire.SourceFamily_SOURCE_FAMILY_AUTOMATION, "ha-logic-1", wire.Effect_EFFECT_RUN_TARGETS},
		{wire.SourceFamily_SOURCE_FAMILY_SCRIPT, "ha-logic-1", wire.Effect_EFFECT_RUN_TARGETS},
		{wire.SourceFamily_SOURCE_FAMILY_SCENE, "ha-scene-1", wire.Effect_EFFECT_RUN_TARGETS},
	} {
		copy := proto.Clone(plan).(*wire.Plan)
		copy.Family, copy.SchemaVersion, copy.Effect, copy.Changes[0].Effect = fixture.family, fixture.schema, fixture.effect, fixture.effect
		if AdmitConfigurationPlanResponse(&wire.BuildPlanResponse{Plan: copy}) == nil {
			t.Fatal("Ecobee field crossed operational family")
		}
	}
	copy := proto.Clone(plan).(*wire.Plan)
	copy.ApplicationAvailable = true
	if AdmitConfigurationPlanResponse(&wire.BuildPlanResponse{Plan: copy}) == nil {
		t.Fatal("preview invented application authority")
	}
}
