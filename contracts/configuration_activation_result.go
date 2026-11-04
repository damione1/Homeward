package contracts

import (
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/protobuf/types/known/anypb"
)

// ParseConfigurationActivationResult admits the closed method-specific result
// envelopes and returns their immutable shared value. It cannot infer success
// from an unknown Any type or an absent wrapped result.
func ParseConfigurationActivationResult(raw *anypb.Any) (*api.ConfigurationActivationResult, error) {
	if raw == nil {
		return nil, ErrInvalidConfigurationOperation
	}
	switch raw.TypeUrl {
	case "type.googleapis.com/home.api.v2.PublishConfigurationDocumentResponse":
		value := new(api.PublishConfigurationDocumentResponse)
		if !configurationOperationPayload(value, raw) || value.Result == nil {
			return nil, ErrInvalidConfigurationOperation
		}
		return value.Result, nil
	case "type.googleapis.com/home.api.v2.RollbackConfigurationDocumentResponse":
		value := new(api.RollbackConfigurationDocumentResponse)
		if !configurationOperationPayload(value, raw) || value.Result == nil {
			return nil, ErrInvalidConfigurationOperation
		}
		return value.Result, nil
	case "type.googleapis.com/home.api.v2.ApplyConfigurationPlanResponse":
		value := new(api.ApplyConfigurationPlanResponse)
		if !configurationOperationPayload(value, raw) || value.Result == nil {
			return nil, ErrInvalidConfigurationOperation
		}
		return value.Result, nil
	case "type.googleapis.com/home.api.v2.RollbackConfigurationResponse":
		value := new(api.RollbackConfigurationResponse)
		if !configurationOperationPayload(value, raw) || value.Result == nil {
			return nil, ErrInvalidConfigurationOperation
		}
		return value.Result, nil
	default:
		return nil, ErrInvalidConfigurationOperation
	}
}
