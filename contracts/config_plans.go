package contracts

import (
	"errors"
	"strings"
	"unicode/utf8"

	wire "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	"google.golang.org/protobuf/proto"
)

var ErrInvalidConfigurationPlanContract = errors.New("invalid configuration plan contract")

func planLabel(s string, max int) bool {
	return len(s) > 0 && len(s) <= max && utf8.ValidString(s) && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\x00\r\n")
}
func planFamily(f wire.SourceFamily) bool {
	switch f {
	case wire.SourceFamily_SOURCE_FAMILY_OPERATIONAL, wire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V1, wire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V2, wire.SourceFamily_SOURCE_FAMILY_AUTHORIZATION, wire.SourceFamily_SOURCE_FAMILY_OWNERSHIP, wire.SourceFamily_SOURCE_FAMILY_DASHBOARD, wire.SourceFamily_SOURCE_FAMILY_AUTOMATION, wire.SourceFamily_SOURCE_FAMILY_SCRIPT, wire.SourceFamily_SOURCE_FAMILY_SCENE:
		return true
	case wire.SourceFamily_SOURCE_FAMILY_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func planCandidate(v *wire.Candidate) bool {
	if v == nil || !planFamily(v.Family) || !planLabel(v.Identity, 255) || !planLabel(v.Entry, 4096) || len(v.Files) < 1 || len(v.Files) > 64 {
		return false
	}
	size := 0
	seen := map[string]bool{}
	for _, file := range v.Files {
		if file == nil || !planLabel(file.LogicalPath, 4096) || len(file.ExactSource) < 1 || seen[file.LogicalPath] {
			return false
		}
		seen[file.LogicalPath] = true
		size += len(file.ExactSource)
		if size > 1<<20 {
			return false
		}
	}
	return seen[v.Entry]
}
func AdmitConfigurationPlanRequest(message proto.Message) error {
	if message == nil || !configurationKnown(message.ProtoReflect()) {
		return ErrInvalidConfigurationPlanContract
	}
	valid := false
	switch v := message.(type) {
	case *wire.BuildPlanRequest:
		valid = v != nil && planCandidate(v.Candidate)
	case *wire.RevalidatePlanRequest:
		valid = v != nil && planCandidate(v.Candidate) && len(v.Receipt) >= 44 && len(v.Receipt) <= 4200
	default:
		return ErrInvalidConfigurationPlanContract
	}
	if !valid {
		return ErrInvalidConfigurationPlanContract
	}
	return nil
}
func planSchema(f wire.SourceFamily, s string) bool {
	switch f {
	case wire.SourceFamily_SOURCE_FAMILY_OPERATIONAL:
		return s == "homeward.operational/1"
	case wire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V1:
		return s == "ha-declarative-1"
	case wire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V2:
		return s == "ha-declarative-2"
	case wire.SourceFamily_SOURCE_FAMILY_AUTHORIZATION:
		return s == "homeward.authorization/1"
	case wire.SourceFamily_SOURCE_FAMILY_OWNERSHIP:
		return s == "homeward.heating-ownership/1"
	case wire.SourceFamily_SOURCE_FAMILY_DASHBOARD:
		return s == "homeward-dashboard-1"
	case wire.SourceFamily_SOURCE_FAMILY_AUTOMATION, wire.SourceFamily_SOURCE_FAMILY_SCRIPT:
		return s == "ha-logic-1"
	case wire.SourceFamily_SOURCE_FAMILY_SCENE:
		return s == "ha-scene-1"
	case wire.SourceFamily_SOURCE_FAMILY_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func planEffect(f wire.SourceFamily, e wire.Effect) bool {
	if e == wire.Effect_EFFECT_NONE {
		return true
	}
	switch f {
	case wire.SourceFamily_SOURCE_FAMILY_OPERATIONAL:
		return e == wire.Effect_EFFECT_CONFIGURATION || e == wire.Effect_EFFECT_TECHNICAL_OWNERSHIP
	case wire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V1, wire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V2:
		return e == wire.Effect_EFFECT_CONFIGURATION
	case wire.SourceFamily_SOURCE_FAMILY_AUTHORIZATION:
		return e == wire.Effect_EFFECT_HUMAN_RIGHTS
	case wire.SourceFamily_SOURCE_FAMILY_OWNERSHIP:
		return e == wire.Effect_EFFECT_TECHNICAL_OWNERSHIP
	case wire.SourceFamily_SOURCE_FAMILY_DASHBOARD:
		return e == wire.Effect_EFFECT_DASHBOARD
	case wire.SourceFamily_SOURCE_FAMILY_AUTOMATION, wire.SourceFamily_SOURCE_FAMILY_SCRIPT, wire.SourceFamily_SOURCE_FAMILY_SCENE:
		return e == wire.Effect_EFFECT_RUN_TARGETS
	case wire.SourceFamily_SOURCE_FAMILY_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func planField(s string) bool {
	switch s {
	case "ecobee", "disabled_integrations", "remote_calendar", "external_worker_bindings", "external_worker_processes", "mqtt", "neviweb", "hilo", "energy", "derived_power", "solar", "workday", "external_workers", "launch_ignored", "input_boolean", "input_number", "counter", "input_button", "input_datetime", "input_select", "input_text", "group", "human_policy", "technical_ownership", "dashboard_views", "dashboard_strategy", "run_triggers", "run_conditions", "run_actions", "run_policy", "run_variables", "run_metadata", "scene_members", "scene_metadata", "definition":
		return true
	default:
		return false
	}
}
func planBase(v *wire.Base) bool {
	if v == nil {
		return false
	}
	switch v.Kind {
	case wire.BaseKind_BASE_KIND_ACTIVE_SQL:
		return v.Revision != nil && *v.Revision > 0 && configurationHash(v.SourceSha256)
	case wire.BaseKind_BASE_KIND_IMMUTABLE_REFERENCE:
		return v.Revision == nil && configurationHash(v.SourceSha256)
	case wire.BaseKind_BASE_KIND_NO_ACTIVE_DOCUMENT:
		return v.Revision == nil && v.SourceSha256 == nil
	case wire.BaseKind_BASE_KIND_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func planResponse(v *wire.Plan) bool {
	if v == nil || !planLabel(v.Author, 255) || !planFamily(v.Family) || !planLabel(v.Identity, 255) || !planSchema(v.Family, v.SchemaVersion) || !configurationHash(v.SourceSha256) || !planBase(v.Base) || v.GenerationRevision < 1 || !configurationHash(v.ManifestSha256) || !configurationHash(v.ObservedWorkspaceSha256) || !planEffect(v.Family, v.Effect) || v.ApplicationAvailable || len(v.Changes) > 64 || len(v.Diagnostics) > 5 {
		return false
	}
	for _, diagnostic := range v.Diagnostics {
		switch diagnostic {
		case wire.DiagnosticCode_DIAGNOSTIC_CODE_INVALID_SCHEMA, wire.DiagnosticCode_DIAGNOSTIC_CODE_EXCLUDED_REFERENCE, wire.DiagnosticCode_DIAGNOSTIC_CODE_UNSUPPORTED_SOURCE, wire.DiagnosticCode_DIAGNOSTIC_CODE_CAPACITY, wire.DiagnosticCode_DIAGNOSTIC_CODE_BASE_UNAVAILABLE:
		case wire.DiagnosticCode_DIAGNOSTIC_CODE_UNSPECIFIED:
			return false
		default:
			return false
		}
	}
	maximum := wire.Effect_EFFECT_NONE
	previous := ""
	for _, change := range v.Changes {
		if change == nil || !planField(change.Field) || ((change.Field == "ecobee" || change.Field == "remote_calendar" || change.Field == "disabled_integrations") && v.Family != wire.SourceFamily_SOURCE_FAMILY_OPERATIONAL) || change.Field <= previous || !planEffect(v.Family, change.Effect) || change.Effect == wire.Effect_EFFECT_NONE || change.Effect > v.Effect || change.ReferencesAdded > 4096 || change.ReferencesRemoved > 4096 {
			return false
		}
		previous = change.Field
		if change.Effect > maximum {
			maximum = change.Effect
		}
		switch change.Operation {
		case wire.ChangeOperation_CHANGE_OPERATION_ADDED, wire.ChangeOperation_CHANGE_OPERATION_REMOVED, wire.ChangeOperation_CHANGE_OPERATION_REPLACED:
		case wire.ChangeOperation_CHANGE_OPERATION_UNSPECIFIED:
			return false
		default:
			return false
		}
	}
	if v.SemanticSha256 != nil && v.Effect != maximum {
		return false
	}
	switch v.State {
	case wire.PlanState_PLAN_STATE_ADMITTED:
		return configurationHash(v.SemanticSha256) && len(v.Diagnostics) == 0 && len(v.Receipt) >= 44 && len(v.Receipt) <= 4200 && ((len(v.Changes) == 0) == (v.Effect == wire.Effect_EFFECT_NONE))
	case wire.PlanState_PLAN_STATE_REJECTED:
		return v.SemanticSha256 == nil && len(v.Changes) == 0 && len(v.Diagnostics) > 0 && v.Effect != wire.Effect_EFFECT_NONE && len(v.Receipt) >= 44 && len(v.Receipt) <= 4200
	case wire.PlanState_PLAN_STATE_STALE:
		return v.Receipt == "" && ((configurationHash(v.SemanticSha256) && len(v.Diagnostics) == 0) || (v.SemanticSha256 == nil && len(v.Diagnostics) > 0))
	case wire.PlanState_PLAN_STATE_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func AdmitConfigurationPlanResponse(message proto.Message) error {
	if message == nil || !configurationKnown(message.ProtoReflect()) {
		return ErrInvalidConfigurationPlanContract
	}
	valid := false
	switch v := message.(type) {
	case *wire.BuildPlanResponse:
		valid = v != nil && planResponse(v.Plan) && v.Plan.State != wire.PlanState_PLAN_STATE_STALE
	case *wire.RevalidatePlanResponse:
		valid = v != nil && planResponse(v.Plan)
	default:
		return ErrInvalidConfigurationPlanContract
	}
	if !valid {
		return ErrInvalidConfigurationPlanContract
	}
	return nil
}
