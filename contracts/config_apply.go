package contracts

import (
	"errors"

	configuration "github.com/damione1/Homeward/gen/home/configuration/v1"
	wire "github.com/damione1/Homeward/gen/home/configurationapply/v1"
	plan "github.com/damione1/Homeward/gen/home/configurationplan/v1"
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
func configurationRollbackHistory(value *wire.RollbackRequest) bool {
	if value == nil {
		return false
	}
	if value.Family == plan.SourceFamily_SOURCE_FAMILY_AUTHORIZATION {
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
	case *wire.ApplyRequest:
		valid = v != nil && planCandidate(v.Candidate) && (v.Receipt == nil || len(*v.Receipt) >= 44 && len(*v.Receipt) <= 4200) && configurationVersion(v.Expected) && configurationHash(v.ExpectedWorkspaceSha256) && applySourcePreconditions(v.Sources) && (v.Candidate.Family == plan.SourceFamily_SOURCE_FAMILY_OPERATIONAL && len(v.Sources) == 0 || v.Candidate.Family != plan.SourceFamily_SOURCE_FAMILY_OPERATIONAL && len(v.Sources) == len(v.Candidate.Files))
	case *wire.RollbackRequest:
		valid = v != nil && configurationID(v.Id) && configurationVersion(v.Expected) && configurationHash(v.ExpectedWorkspaceSha256) && configurationRollbackHistory(v) && applySourcePreconditions(v.Sources) && (v.Family == plan.SourceFamily_SOURCE_FAMILY_UNSPECIFIED && len(v.Sources) == 0 && v.Identity == "" || v.Family == plan.SourceFamily_SOURCE_FAMILY_OPERATIONAL && len(v.Sources) == 0 && v.Identity == "operational.main" || v.Family != plan.SourceFamily_SOURCE_FAMILY_OPERATIONAL && planFamily(v.Family) && len(v.Sources) > 0 && planLabel(v.Identity, 255))
	default:
		return ErrInvalidConfigurationApplyContract
	}
	if !valid {
		return ErrInvalidConfigurationApplyContract
	}
	return nil
}

func AdmitConfigurationApplyResponse(message proto.Message) error {
	if message == nil || !configurationKnown(message.ProtoReflect()) {
		return ErrInvalidConfigurationApplyContract
	}
	var response *configuration.WriteDocumentResponse
	switch v := message.(type) {
	case *wire.ApplyResponse:
		if v != nil {
			response = &configuration.WriteDocumentResponse{Activation: v.Activation, Current: v.Current}
		}
	case *wire.RollbackResponse:
		if v != nil {
			response = &configuration.WriteDocumentResponse{Activation: v.Activation, Current: v.Current}
		}
	default:
		return ErrInvalidConfigurationApplyContract
	}
	if response == nil || AdmitConfigurationResponse(response) != nil {
		return ErrInvalidConfigurationApplyContract
	}
	return nil
}
