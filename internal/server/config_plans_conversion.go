package server

import (
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/configplans"
)

func planFamilyWire(value configplans.Family) wire.ConfigurationPlanSourceFamily {
	switch value {
	case configplans.Operational:
		return wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL
	case configplans.Declarative1:
		return wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DECLARATIVE_V1
	case configplans.Declarative2:
		return wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DECLARATIVE_V2
	case configplans.Authorization:
		return wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTHORIZATION
	case configplans.Ownership:
		return wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OWNERSHIP
	case configplans.Dashboard:
		return wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DASHBOARD
	case configplans.Automation:
		return wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTOMATION
	case configplans.Script:
		return wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_SCRIPT
	case configplans.Scene:
		return wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_SCENE
	default:
		return wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_UNSPECIFIED
	}
}
func planStateWire(value configplans.State) wire.ConfigurationPlanState {
	switch value {
	case configplans.Admitted:
		return wire.ConfigurationPlanState_CONFIGURATION_PLAN_STATE_ADMITTED
	case configplans.Rejected:
		return wire.ConfigurationPlanState_CONFIGURATION_PLAN_STATE_REJECTED
	case configplans.Stale:
		return wire.ConfigurationPlanState_CONFIGURATION_PLAN_STATE_STALE
	default:
		return wire.ConfigurationPlanState_CONFIGURATION_PLAN_STATE_UNSPECIFIED
	}
}
func planBaseWire(value configplans.BaseKind) wire.ConfigurationPlanBaseKind {
	switch value {
	case configplans.ActiveSQL:
		return wire.ConfigurationPlanBaseKind_CONFIGURATION_PLAN_BASE_KIND_ACTIVE_SQL
	case configplans.ImmutableReference:
		return wire.ConfigurationPlanBaseKind_CONFIGURATION_PLAN_BASE_KIND_IMMUTABLE_REFERENCE
	case configplans.NoActiveDocument:
		return wire.ConfigurationPlanBaseKind_CONFIGURATION_PLAN_BASE_KIND_NO_ACTIVE_DOCUMENT
	default:
		return wire.ConfigurationPlanBaseKind_CONFIGURATION_PLAN_BASE_KIND_UNSPECIFIED
	}
}
func planEffectWire(value configplans.Effect) wire.ConfigurationPlanEffect {
	switch value {
	case configplans.NoEffect:
		return wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_NONE
	case configplans.DashboardEffect:
		return wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_DASHBOARD
	case configplans.ConfigurationEffect:
		return wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_CONFIGURATION
	case configplans.RightsEffect:
		return wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_HUMAN_RIGHTS
	case configplans.OwnershipEffect:
		return wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_TECHNICAL_OWNERSHIP
	case configplans.RunTargetsEffect:
		return wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_RUN_TARGETS
	default:
		return wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_UNSPECIFIED
	}
}
func planOperationWire(value configplans.ChangeOperation) wire.ConfigurationPlanChangeOperation {
	switch value {
	case configplans.Added:
		return wire.ConfigurationPlanChangeOperation_CONFIGURATION_PLAN_CHANGE_OPERATION_ADDED
	case configplans.Removed:
		return wire.ConfigurationPlanChangeOperation_CONFIGURATION_PLAN_CHANGE_OPERATION_REMOVED
	case configplans.Replaced:
		return wire.ConfigurationPlanChangeOperation_CONFIGURATION_PLAN_CHANGE_OPERATION_REPLACED
	default:
		return wire.ConfigurationPlanChangeOperation_CONFIGURATION_PLAN_CHANGE_OPERATION_UNSPECIFIED
	}
}
func planDiagnosticWire(value configplans.Diagnostic) wire.ConfigurationPlanDiagnosticCode {
	switch value {
	case configplans.InvalidSchema:
		return wire.ConfigurationPlanDiagnosticCode_CONFIGURATION_PLAN_DIAGNOSTIC_CODE_INVALID_SCHEMA
	case configplans.ExcludedReference:
		return wire.ConfigurationPlanDiagnosticCode_CONFIGURATION_PLAN_DIAGNOSTIC_CODE_EXCLUDED_REFERENCE
	case configplans.UnsupportedSource:
		return wire.ConfigurationPlanDiagnosticCode_CONFIGURATION_PLAN_DIAGNOSTIC_CODE_UNSUPPORTED_SOURCE
	case configplans.Capacity:
		return wire.ConfigurationPlanDiagnosticCode_CONFIGURATION_PLAN_DIAGNOSTIC_CODE_CAPACITY
	case configplans.BaseUnavailable:
		return wire.ConfigurationPlanDiagnosticCode_CONFIGURATION_PLAN_DIAGNOSTIC_CODE_BASE_UNAVAILABLE
	default:
		return wire.ConfigurationPlanDiagnosticCode_CONFIGURATION_PLAN_DIAGNOSTIC_CODE_UNSPECIFIED
	}
}
