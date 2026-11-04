package contracts

import (
	"testing"

	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/protobuf/proto"
)

func TestNativeIntegrationPublicFieldsRefusePrivatePathsAndOtherFamilies(t *testing.T) {
	for _, nativeField := range []string{"ecobee", "sonos", "android_tv"} {
		t.Run(nativeField, func(t *testing.T) {
			plan := nha053ContractPlan()
			plan.Family = wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL
			plan.Identity = "operational.main"
			plan.SchemaVersion = "homeward.operational/1"
			plan.Effect = wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_CONFIGURATION
			plan.Changes = []*wire.ConfigurationPlanChange{{Effect: wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_CONFIGURATION, Operation: wire.ConfigurationPlanChangeOperation_CONFIGURATION_PLAN_CHANGE_OPERATION_REPLACED, Field: nativeField}}
			if err := AdmitConfigurationPlanResponse(plan); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{nativeField + ".address", nativeField + ".credential_secret", nativeField + ".private-instance", nativeField + "_future"} {
				copy := proto.Clone(plan).(*wire.ConfigurationPlan)
				copy.Changes[0].Field = field
				if AdmitConfigurationPlanResponse(copy) == nil {
					t.Fatal("private/future Ecobee field admitted")
				}
			}
			for _, fixture := range []struct {
				family wire.ConfigurationPlanSourceFamily
				schema string
				effect wire.ConfigurationPlanEffect
			}{
				{wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DECLARATIVE_V1, "ha-declarative-1", wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_CONFIGURATION},
				{wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DECLARATIVE_V2, "ha-declarative-2", wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_CONFIGURATION},
				{wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTHORIZATION, "homeward.authorization/1", wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_HUMAN_RIGHTS},
				{wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OWNERSHIP, "homeward.heating-ownership/1", wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_TECHNICAL_OWNERSHIP},
				{wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DASHBOARD, "homeward-dashboard-1", wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_DASHBOARD},
				{wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTOMATION, "ha-logic-1", wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_RUN_TARGETS},
				{wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_SCRIPT, "ha-logic-1", wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_RUN_TARGETS},
				{wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_SCENE, "ha-scene-1", wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_RUN_TARGETS},
			} {
				copy := proto.Clone(plan).(*wire.ConfigurationPlan)
				copy.Family, copy.SchemaVersion, copy.Effect, copy.Changes[0].Effect = fixture.family, fixture.schema, fixture.effect, fixture.effect
				if AdmitConfigurationPlanResponse(copy) == nil {
					t.Fatal("Ecobee field crossed operational family")
				}
			}
			copy := proto.Clone(plan).(*wire.ConfigurationPlan)
			copy.ApplicationAvailable = true
			if AdmitConfigurationPlanResponse(copy) == nil {
				t.Fatal("preview invented application authority")
			}
		})
	}
}
