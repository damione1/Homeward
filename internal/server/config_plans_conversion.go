package server

import (
	wire "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	"github.com/damione1/Homeward/internal/configplans"
)

func planFamilyWire(value configplans.Family) wire.SourceFamily {
	switch value {
	case configplans.Operational:
		return wire.SourceFamily_SOURCE_FAMILY_OPERATIONAL
	case configplans.Declarative1:
		return wire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V1
	case configplans.Declarative2:
		return wire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V2
	case configplans.Authorization:
		return wire.SourceFamily_SOURCE_FAMILY_AUTHORIZATION
	case configplans.Ownership:
		return wire.SourceFamily_SOURCE_FAMILY_OWNERSHIP
	case configplans.Dashboard:
		return wire.SourceFamily_SOURCE_FAMILY_DASHBOARD
	case configplans.Automation:
		return wire.SourceFamily_SOURCE_FAMILY_AUTOMATION
	case configplans.Script:
		return wire.SourceFamily_SOURCE_FAMILY_SCRIPT
	case configplans.Scene:
		return wire.SourceFamily_SOURCE_FAMILY_SCENE
	default:
		return wire.SourceFamily_SOURCE_FAMILY_UNSPECIFIED
	}
}
func planStateWire(value configplans.State) wire.PlanState {
	switch value {
	case configplans.Admitted:
		return wire.PlanState_PLAN_STATE_ADMITTED
	case configplans.Rejected:
		return wire.PlanState_PLAN_STATE_REJECTED
	case configplans.Stale:
		return wire.PlanState_PLAN_STATE_STALE
	default:
		return wire.PlanState_PLAN_STATE_UNSPECIFIED
	}
}
func planBaseWire(value configplans.BaseKind) wire.BaseKind {
	switch value {
	case configplans.ActiveSQL:
		return wire.BaseKind_BASE_KIND_ACTIVE_SQL
	case configplans.ImmutableReference:
		return wire.BaseKind_BASE_KIND_IMMUTABLE_REFERENCE
	case configplans.NoActiveDocument:
		return wire.BaseKind_BASE_KIND_NO_ACTIVE_DOCUMENT
	default:
		return wire.BaseKind_BASE_KIND_UNSPECIFIED
	}
}
func planEffectWire(value configplans.Effect) wire.Effect {
	switch value {
	case configplans.NoEffect:
		return wire.Effect_EFFECT_NONE
	case configplans.DashboardEffect:
		return wire.Effect_EFFECT_DASHBOARD
	case configplans.ConfigurationEffect:
		return wire.Effect_EFFECT_CONFIGURATION
	case configplans.RightsEffect:
		return wire.Effect_EFFECT_HUMAN_RIGHTS
	case configplans.OwnershipEffect:
		return wire.Effect_EFFECT_TECHNICAL_OWNERSHIP
	case configplans.RunTargetsEffect:
		return wire.Effect_EFFECT_RUN_TARGETS
	default:
		return wire.Effect_EFFECT_UNSPECIFIED
	}
}
func planOperationWire(value configplans.ChangeOperation) wire.ChangeOperation {
	switch value {
	case configplans.Added:
		return wire.ChangeOperation_CHANGE_OPERATION_ADDED
	case configplans.Removed:
		return wire.ChangeOperation_CHANGE_OPERATION_REMOVED
	case configplans.Replaced:
		return wire.ChangeOperation_CHANGE_OPERATION_REPLACED
	default:
		return wire.ChangeOperation_CHANGE_OPERATION_UNSPECIFIED
	}
}
func planDiagnosticWire(value configplans.Diagnostic) wire.DiagnosticCode {
	switch value {
	case configplans.InvalidSchema:
		return wire.DiagnosticCode_DIAGNOSTIC_CODE_INVALID_SCHEMA
	case configplans.ExcludedReference:
		return wire.DiagnosticCode_DIAGNOSTIC_CODE_EXCLUDED_REFERENCE
	case configplans.UnsupportedSource:
		return wire.DiagnosticCode_DIAGNOSTIC_CODE_UNSUPPORTED_SOURCE
	case configplans.Capacity:
		return wire.DiagnosticCode_DIAGNOSTIC_CODE_CAPACITY
	case configplans.BaseUnavailable:
		return wire.DiagnosticCode_DIAGNOSTIC_CODE_BASE_UNAVAILABLE
	default:
		return wire.DiagnosticCode_DIAGNOSTIC_CODE_UNSPECIFIED
	}
}
