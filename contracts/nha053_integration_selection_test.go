package contracts

import (
	wire "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestNHA053IntegrationSelectionPublicFieldIsOperationalAndPrivatePathsFailClosed(t *testing.T) {
	plan := nha053ContractPlan()
	plan.Family = wire.SourceFamily_SOURCE_FAMILY_OPERATIONAL
	plan.Identity = "operational.main"
	plan.SchemaVersion = "homeward.operational/1"
	plan.Effect = wire.Effect_EFFECT_CONFIGURATION
	plan.Changes = []*wire.Change{{Effect: wire.Effect_EFFECT_CONFIGURATION, Operation: wire.ChangeOperation_CHANGE_OPERATION_ADDED, Field: "disabled_integrations"}}
	if err := AdmitConfigurationPlanResponse(&wire.BuildPlanResponse{Plan: plan}); err != nil {
		t.Fatal("148 selection field refused", err)
	}
	for _, field := range []string{"disabled_integrations.nha053-selection-worker", "disabled_integrations[0]", "future_integration_selection"} {
		copy := proto.Clone(plan).(*wire.Plan)
		copy.Changes[0].Field = field
		if AdmitConfigurationPlanResponse(&wire.BuildPlanResponse{Plan: copy}) == nil {
			t.Fatal("private instance/future path admitted", field)
		}
	}
	for _, family := range []wire.SourceFamily{wire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V1, wire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V2, wire.SourceFamily_SOURCE_FAMILY_AUTHORIZATION, wire.SourceFamily_SOURCE_FAMILY_OWNERSHIP, wire.SourceFamily_SOURCE_FAMILY_DASHBOARD, wire.SourceFamily_SOURCE_FAMILY_AUTOMATION, wire.SourceFamily_SOURCE_FAMILY_SCRIPT, wire.SourceFamily_SOURCE_FAMILY_SCENE} {
		if planFamily(family) == false {
			t.Fatal("known family lost admission")
		}
		copy := proto.Clone(plan).(*wire.Plan)
		copy.Family = family
		switch family {
		case wire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V1:
			copy.SchemaVersion = "ha-declarative-1"
		case wire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V2:
			copy.SchemaVersion = "ha-declarative-2"
		case wire.SourceFamily_SOURCE_FAMILY_AUTHORIZATION:
			copy.SchemaVersion = "homeward.authorization/1"
			copy.Effect = wire.Effect_EFFECT_HUMAN_RIGHTS
		case wire.SourceFamily_SOURCE_FAMILY_OWNERSHIP:
			copy.SchemaVersion = "homeward.heating-ownership/1"
			copy.Effect = wire.Effect_EFFECT_TECHNICAL_OWNERSHIP
		case wire.SourceFamily_SOURCE_FAMILY_DASHBOARD:
			copy.SchemaVersion = "homeward-dashboard-1"
			copy.Effect = wire.Effect_EFFECT_DASHBOARD
		case wire.SourceFamily_SOURCE_FAMILY_AUTOMATION, wire.SourceFamily_SOURCE_FAMILY_SCRIPT:
			copy.SchemaVersion = "ha-logic-1"
			copy.Effect = wire.Effect_EFFECT_RUN_TARGETS
		case wire.SourceFamily_SOURCE_FAMILY_SCENE:
			copy.SchemaVersion = "ha-scene-1"
			copy.Effect = wire.Effect_EFFECT_RUN_TARGETS
		}
		copy.Changes[0].Effect = copy.Effect
		if AdmitConfigurationPlanResponse(&wire.BuildPlanResponse{Plan: copy}) == nil {
			t.Fatal("148 selector was reinterpreted by another family", family)
		}
	}
}
