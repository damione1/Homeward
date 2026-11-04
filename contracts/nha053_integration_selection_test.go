package contracts

import (
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestNHA053IntegrationSelectionPublicFieldIsOperationalAndPrivatePathsFailClosed(t *testing.T) {
	plan := nha053ContractPlan()
	plan.Family = wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL
	plan.Identity = "operational.main"
	plan.SchemaVersion = "homeward.operational/1"
	plan.Effect = wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_CONFIGURATION
	plan.Changes = []*wire.ConfigurationPlanChange{{Effect: wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_CONFIGURATION, Operation: wire.ConfigurationPlanChangeOperation_CONFIGURATION_PLAN_CHANGE_OPERATION_ADDED, Field: "disabled_integrations"}}
	if err := AdmitConfigurationPlanResponse(plan); err != nil {
		t.Fatal("148 selection field refused", err)
	}
	for _, field := range []string{"disabled_integrations.nha053-selection-worker", "disabled_integrations[0]", "future_integration_selection"} {
		copy := proto.Clone(plan).(*wire.ConfigurationPlan)
		copy.Changes[0].Field = field
		if AdmitConfigurationPlanResponse(copy) == nil {
			t.Fatal("private instance/future path admitted", field)
		}
	}
	for _, family := range []wire.ConfigurationPlanSourceFamily{wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DECLARATIVE_V1, wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DECLARATIVE_V2, wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTHORIZATION, wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OWNERSHIP, wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DASHBOARD, wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTOMATION, wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_SCRIPT, wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_SCENE} {
		if planFamily(family) == false {
			t.Fatal("known family lost admission")
		}
		copy := proto.Clone(plan).(*wire.ConfigurationPlan)
		copy.Family = family
		switch family {
		case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DECLARATIVE_V1:
			copy.SchemaVersion = "ha-declarative-1"
		case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DECLARATIVE_V2:
			copy.SchemaVersion = "ha-declarative-2"
		case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTHORIZATION:
			copy.SchemaVersion = "homeward.authorization/1"
			copy.Effect = wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_HUMAN_RIGHTS
		case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OWNERSHIP:
			copy.SchemaVersion = "homeward.heating-ownership/1"
			copy.Effect = wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_TECHNICAL_OWNERSHIP
		case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DASHBOARD:
			copy.SchemaVersion = "homeward-dashboard-1"
			copy.Effect = wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_DASHBOARD
		case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTOMATION, wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_SCRIPT:
			copy.SchemaVersion = "ha-logic-1"
			copy.Effect = wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_RUN_TARGETS
		case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_SCENE:
			copy.SchemaVersion = "ha-scene-1"
			copy.Effect = wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_RUN_TARGETS
		}
		copy.Changes[0].Effect = copy.Effect
		if AdmitConfigurationPlanResponse(copy) == nil {
			t.Fatal("148 selector was reinterpreted by another family", family)
		}
	}
}
