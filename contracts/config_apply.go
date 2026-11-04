package contracts

import (
	"errors"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	plan "github.com/damione1/Homeward/gen/home/api/v2"
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/proto"
)

var ErrInvalidConfigurationApplyContract = errors.New("invalid configuration apply contract")

func applySourcePreconditions(values []*wire.SourcePrecondition) bool {
	if len(values) > 64 {
		return false
	}
	ids, paths := map[string]bool{}, map[string]bool{}
	for _, value := range values {
		if value == nil || !configurationID(value.Id) || !planLabel(value.LogicalPath, 4096) || ids[value.Id.Value] || paths[value.LogicalPath] {
			return false
		}
		ids[value.Id.Value], paths[value.LogicalPath] = true, true
		switch state := value.SqlState.(type) {
		case *wire.SourcePrecondition_Existing:
			if state == nil || !configurationVersion(state.Existing) {
				return false
			}
		case *wire.SourcePrecondition_SqlAbsent:
			if state == nil || state.SqlAbsent == nil {
				return false
			}
		default:
			return false
		}
		switch state := value.WorkspaceState.(type) {
		case *wire.SourcePrecondition_WorkspaceSha256:
			if state == nil || !configurationHash(state.WorkspaceSha256) {
				return false
			}
		case *wire.SourcePrecondition_WorkspaceAbsent:
			if state == nil || state.WorkspaceAbsent == nil {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// Authorization supports either its legacy explicit generation delta or an
// exact documentary policy version. Other families retain generation history.
func configurationRollbackHistory(value *wire.RollbackConfigurationRequest) bool {
	if value == nil {
		return false
	}
	if value.Family == plan.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTHORIZATION {
		if (value.Historical == nil) == (value.HistoricalPolicyVersion == nil) {
			return false
		}
		if value.HistoricalPolicyVersion != nil {
			return configurationVersion(value.HistoricalPolicyVersion)
		}
		return configurationGeneration(value.Historical)
	}
	return value.HistoricalPolicyVersion == nil && configurationGeneration(value.Historical)
}

func AdmitConfigurationApplyRequest(message proto.Message) error {
	if message == nil || !configurationKnown(message.ProtoReflect()) {
		return ErrInvalidConfigurationApplyContract
	}
	valid := false
	switch v := message.(type) {
	case *wire.ApplyConfigurationPlanRequest:
		valid = v != nil && configurationResourceName(v.Name, "configurationPlans") && len(v.Etag) > 0 && len(v.Etag) <= 128 && planCandidate(v.Candidate) && configurationVersion(v.Expected) && configurationHash(v.ExpectedWorkspaceSha256) && applySourcePreconditions(v.Sources) && (v.Candidate.Family == plan.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL && len(v.Sources) == 0 || v.Candidate.Family != plan.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL && len(v.Sources) == len(v.Candidate.Files))
	case *wire.RollbackConfigurationRequest:
		valid = v != nil && configurationResourceName(v.Name, "configurationDocuments") && configurationVersion(v.Expected) && configurationHash(v.ExpectedWorkspaceSha256) && configurationRollbackHistory(v) && applySourcePreconditions(v.Sources) && (v.Family == plan.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_UNSPECIFIED && len(v.Sources) == 0 && v.Identity == "" || v.Family == plan.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL && len(v.Sources) == 0 && v.Identity == "operational.main" || v.Family != plan.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL && planFamily(v.Family) && len(v.Sources) > 0 && planLabel(v.Identity, 255))
	default:
		return ErrInvalidConfigurationApplyContract
	}
	if !valid {
		return ErrInvalidConfigurationApplyContract
	}
	return nil
}

func AdmitConfigurationApplyResponse(message proto.Message) error {
	operation, ok := message.(*longrunningpb.Operation)
	if !ok || operation == nil {
		return ErrInvalidConfigurationApplyContract
	}
	name, err := apitypes.ParseOperationName(operation.Name)
	if err != nil || AdmitConfigurationOperation(operation, name) != nil {
		return ErrInvalidConfigurationApplyContract
	}
	return nil
}
