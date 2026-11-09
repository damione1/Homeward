package contracts

import (
	"errors"
	"strings"
	"unicode/utf8"

	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/proto"
)

var ErrInvalidConfigurationPlanContract = errors.New("invalid configuration plan contract")

func planLabel(s string, max int) bool {
	return len(s) > 0 && len(s) <= max && utf8.ValidString(s) && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\x00\r\n")
}
func planFamily(f wire.ConfigurationPlanSourceFamily) bool {
	switch f {
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL, wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DECLARATIVE_V1, wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DECLARATIVE_V2, wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTHORIZATION, wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OWNERSHIP, wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DASHBOARD, wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTOMATION, wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_SCRIPT, wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_SCENE:
		return true
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func planCandidate(v *wire.ConfigurationPlanCandidate) bool {
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
	case *wire.CreateConfigurationPlanRequest:
		valid = v != nil && requestedPublicResourceID(v.ConfigurationPlanId) && configurationHomeName(v.Parent) && v.ConfigurationPlan != nil && planCandidate(v.ConfigurationPlan.Candidate)
	case *wire.GetConfigurationPlanRequest:
		valid = v != nil && configurationResourceName(v.Name, "configurationPlans")
	case *wire.ListConfigurationPlansRequest:
		valid = v != nil && configurationHomeName(v.Parent) && configurationPage(v.PageSize, v.PageToken)
	case *wire.RevalidateConfigurationPlanRequest:
		valid = v != nil && planCandidate(v.Candidate) && configurationResourceName(v.Name, "configurationPlans") && len(v.Etag) > 0 && len(v.Etag) <= 128
	default:
		return ErrInvalidConfigurationPlanContract
	}
	if !valid {
		return ErrInvalidConfigurationPlanContract
	}
	return nil
}
func planSchema(f wire.ConfigurationPlanSourceFamily, s string) bool {
	switch f {
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL:
		return s == "homeward.operational/1"
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DECLARATIVE_V1:
		return s == "ha-declarative-1"
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DECLARATIVE_V2:
		return s == "ha-declarative-2"
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTHORIZATION:
		return s == "homeward.authorization/1"
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OWNERSHIP:
		return s == "homeward.heating-ownership/1"
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DASHBOARD:
		return s == "homeward-dashboard-1"
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTOMATION, wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_SCRIPT:
		return s == "ha-logic-1"
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_SCENE:
		return s == "ha-scene-1"
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func planEffect(f wire.ConfigurationPlanSourceFamily, e wire.ConfigurationPlanEffect) bool {
	if e == wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_NONE {
		return true
	}
	switch f {
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL:
		return e == wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_CONFIGURATION || e == wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_TECHNICAL_OWNERSHIP
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DECLARATIVE_V1, wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DECLARATIVE_V2:
		return e == wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_CONFIGURATION
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTHORIZATION:
		return e == wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_HUMAN_RIGHTS
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OWNERSHIP:
		return e == wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_TECHNICAL_OWNERSHIP
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DASHBOARD:
		return e == wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_DASHBOARD
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTOMATION, wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_SCRIPT, wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_SCENE:
		return e == wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_RUN_TARGETS
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func planField(s string) bool {
	switch s {
	case "wled", "android_tv", "sonos", "ecobee", "disabled_integrations", "remote_calendar", "external_worker_bindings", "external_worker_processes", "mqtt", "neviweb", "hilo", "energy", "derived_power", "solar", "workday", "external_workers", "launch_ignored", "input_boolean", "input_number", "counter", "input_button", "input_datetime", "input_select", "input_text", "group", "human_policy", "technical_ownership", "dashboard_views", "dashboard_strategy", "run_triggers", "run_conditions", "run_actions", "run_policy", "run_variables", "run_metadata", "scene_members", "scene_metadata", "definition":
		return true
	default:
		return false
	}
}
func planBase(v *wire.ConfigurationPlanBase) bool {
	if v == nil {
		return false
	}
	switch v.Kind {
	case wire.ConfigurationPlanBaseKind_CONFIGURATION_PLAN_BASE_KIND_ACTIVE_SQL:
		return v.Revision != nil && *v.Revision > 0 && configurationHash(v.SourceSha256)
	case wire.ConfigurationPlanBaseKind_CONFIGURATION_PLAN_BASE_KIND_IMMUTABLE_REFERENCE:
		return v.Revision == nil && configurationHash(v.SourceSha256)
	case wire.ConfigurationPlanBaseKind_CONFIGURATION_PLAN_BASE_KIND_NO_ACTIVE_DOCUMENT:
		return v.Revision == nil && v.SourceSha256 == nil
	case wire.ConfigurationPlanBaseKind_CONFIGURATION_PLAN_BASE_KIND_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func planMetadata(v *wire.ConfigurationPlan) bool {
	if v == nil || v.Candidate != nil || !planAuthor(v.Author) || !planFamily(v.Family) || !planLabel(v.Identity, 255) || !planSchema(v.Family, v.SchemaVersion) || !configurationHash(v.SourceSha256) || !planBase(v.Base) || v.GenerationRevision < 1 || !configurationHash(v.ManifestSha256) || !configurationHash(v.ObservedWorkspaceSha256) || !planEffect(v.Family, v.Effect) || v.ApplicationAvailable || len(v.Changes) > 64 || len(v.Diagnostics) > 5 {
		return false
	}
	for _, diagnostic := range v.Diagnostics {
		switch diagnostic {
		case wire.ConfigurationPlanDiagnosticCode_CONFIGURATION_PLAN_DIAGNOSTIC_CODE_INVALID_SCHEMA, wire.ConfigurationPlanDiagnosticCode_CONFIGURATION_PLAN_DIAGNOSTIC_CODE_EXCLUDED_REFERENCE, wire.ConfigurationPlanDiagnosticCode_CONFIGURATION_PLAN_DIAGNOSTIC_CODE_UNSUPPORTED_SOURCE, wire.ConfigurationPlanDiagnosticCode_CONFIGURATION_PLAN_DIAGNOSTIC_CODE_CAPACITY, wire.ConfigurationPlanDiagnosticCode_CONFIGURATION_PLAN_DIAGNOSTIC_CODE_BASE_UNAVAILABLE:
		case wire.ConfigurationPlanDiagnosticCode_CONFIGURATION_PLAN_DIAGNOSTIC_CODE_UNSPECIFIED:
			return false
		default:
			return false
		}
	}
	maximum := wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_NONE
	previous := ""
	for _, change := range v.Changes {
		if change == nil || !planField(change.Field) || ((change.Field == "wled" || change.Field == "android_tv" || change.Field == "sonos" || change.Field == "ecobee" || change.Field == "remote_calendar" || change.Field == "disabled_integrations") && v.Family != wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL) || change.Field <= previous || !planEffect(v.Family, change.Effect) || change.Effect == wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_NONE || change.Effect > v.Effect || change.ReferencesAdded < 0 || change.ReferencesAdded > 4096 || change.ReferencesRemoved < 0 || change.ReferencesRemoved > 4096 {
			return false
		}
		previous = change.Field
		if change.Effect > maximum {
			maximum = change.Effect
		}
		switch change.Operation {
		case wire.ConfigurationPlanChangeOperation_CONFIGURATION_PLAN_CHANGE_OPERATION_ADDED, wire.ConfigurationPlanChangeOperation_CONFIGURATION_PLAN_CHANGE_OPERATION_REMOVED, wire.ConfigurationPlanChangeOperation_CONFIGURATION_PLAN_CHANGE_OPERATION_REPLACED:
		case wire.ConfigurationPlanChangeOperation_CONFIGURATION_PLAN_CHANGE_OPERATION_UNSPECIFIED:
			return false
		default:
			return false
		}
	}
	if v.SemanticSha256 != nil && v.Effect != maximum {
		return false
	}
	switch v.State {
	case wire.ConfigurationPlanState_CONFIGURATION_PLAN_STATE_ADMITTED:
		return configurationHash(v.SemanticSha256) && len(v.Diagnostics) == 0 && len(v.Receipt) >= 44 && len(v.Receipt) <= 4200 && ((len(v.Changes) == 0) == (v.Effect == wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_NONE))
	case wire.ConfigurationPlanState_CONFIGURATION_PLAN_STATE_REJECTED:
		return v.SemanticSha256 == nil && len(v.Changes) == 0 && len(v.Diagnostics) > 0 && v.Effect != wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_NONE && len(v.Receipt) >= 44 && len(v.Receipt) <= 4200
	case wire.ConfigurationPlanState_CONFIGURATION_PLAN_STATE_STALE:
		return v.Receipt == "" && ((configurationHash(v.SemanticSha256) && len(v.Diagnostics) == 0) || (v.SemanticSha256 == nil && len(v.Diagnostics) > 0))
	case wire.ConfigurationPlanState_CONFIGURATION_PLAN_STATE_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func planResponse(v *wire.ConfigurationPlan) bool {
	if v == nil || !configurationResourceName(v.Name, "configurationPlans") || !configurationTime(v.CreateTime) || !configurationTime(v.UpdateTime) || !configurationTime(v.ExpireTime) || v.UpdateTime.AsTime().Before(v.CreateTime.AsTime()) || v.UpdateTime.AsTime().After(v.ExpireTime.AsTime()) || !v.ExpireTime.AsTime().After(v.CreateTime.AsTime()) || !planLabel(v.Etag, 256) {
		return false
	}
	return planMetadata(v)
}

// AdmitConfigurationPlanMetadata checks a native redacted summary before it
// enters the process-lifetime catalogue. It rejects all candidate/source input.
func AdmitConfigurationPlanMetadata(value *wire.ConfigurationPlan) error {
	if value == nil || !configurationKnown(value.ProtoReflect()) || !planMetadata(value) {
		return ErrInvalidConfigurationPlanContract
	}
	return nil
}
func AdmitConfigurationPlanResponse(message proto.Message) error {
	if message == nil || !configurationKnown(message.ProtoReflect()) {
		return ErrInvalidConfigurationPlanContract
	}
	valid := false
	switch v := message.(type) {
	case *wire.ConfigurationPlan:
		valid = v != nil && planResponse(v)
	case *wire.ListConfigurationPlansResponse:
		valid = v != nil && len(v.ConfigurationPlans) <= 100 && len(v.NextPageToken) <= 1200
		if valid {
			for _, plan := range v.ConfigurationPlans {
				if !planResponse(plan) {
					valid = false
					break
				}
			}
		}
	default:
		return ErrInvalidConfigurationPlanContract
	}
	if !valid {
		return ErrInvalidConfigurationPlanContract
	}
	return nil
}

func planAuthor(value string) bool {
	_, err := apitypes.ParseAccountName(value)
	return err == nil
}
