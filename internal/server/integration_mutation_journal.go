package server

import (
	"context"
	"errors"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
)

// normalized contains only the request's selected semantic intent and opaque
// CAS values. Output-only fields and current runtime projections never enter
// deduplication. A missing client UUID stays absent and reserves a fresh handle.
func (h *integrationHandler) prepareMutation(ctx context.Context, permit *authorization.Permit, home apitypes.HomeName, target storage.ConfigurationIntegrationTarget, publicTarget apitypes.IntegrationInstanceName, requestID string, normalized proto.Message) (context.Context, *longrunningpb.Operation, error) {
	evidence, err := permit.IntegrationAdministrationEvidence()
	if err != nil {
		return nil, nil, integrationFault(err)
	}
	if normalized == nil || proto.Size(normalized) > 128<<10 {
		return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
	}
	id := storage.WithoutIntegrationRequestID()
	if requestID != "" {
		parsed, err := apitypes.ParseRequestID(requestID)
		if err != nil {
			return nil, nil, integrationInvalidField(normalized, "request_id", api.ValidationReason_VALIDATION_REASON_INVALID_REQUEST_ID)
		}
		id, err = storage.WithIntegrationRequestID(parsed)
		if err != nil {
			return nil, nil, integrationFault(err)
		}
	}
	intent, err := (proto.MarshalOptions{Deterministic: true}).Marshal(normalized)
	if err != nil {
		return nil, nil, integrationResourceIntegrity()
	}
	request, err := storage.NewIntegrationAPIRequest(target, id, intent, evidence)
	if err != nil {
		return nil, nil, integrationFault(err)
	}
	if record, present, err := h.native.documents.Store().ProbeConfigurationAPIRequest(permit.Context(), request); err != nil {
		return nil, nil, integrationFault(err)
	} else if present {
		name, err := configurationOperationName(home, record.ID())
		if err != nil {
			return nil, nil, err
		}
		operation, err := h.readOperation(ctx, name, nil)
		return nil, operation, err
	}
	ctx, err = storage.WithConfigurationIntegrationTarget(ctx, target)
	if err != nil {
		return nil, nil, integrationFault(err)
	}
	ctx, err = storage.WithConfigurationAPIRequest(ctx, request)
	if err != nil {
		return nil, nil, integrationFault(err)
	}
	ctx, err = configactivation.WithAPIOutcomeEncoder(ctx, func(result configactivation.Result, cause error) ([]byte, error) {
		record, present := result.APIRequest()
		if !present {
			return nil, integrationResourceIntegrity()
		}
		fault := integrationFault(cause)
		if result.Outcome() == configactivation.RestartRequired {
			fault = apierrors.New(api.ApiErrorReason_API_ERROR_REASON_CONFIG_RESTART_REQUIRED)
		}
		if cause == nil && result.AttemptOutcome() == storage.TransactionNotCommitted {
			fault = apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ETAG_MISMATCH)
		}
		operation, err := integrationRequestOperation(home, record, publicTarget, &fault)
		if err != nil {
			return nil, err
		}
		return (proto.MarshalOptions{Deterministic: true}).Marshal(operation)
	})
	return ctx, nil, err
}

func (h *integrationHandler) fromActivation(ctx context.Context, home apitypes.HomeName, result configactivation.Result, nativeErr error) (*longrunningpb.Operation, error) {
	permit, err := h.permit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
	if err != nil {
		return nil, err
	}
	defer permit.Release()
	var replayed *storage.ConfigurationRequestError
	if errors.As(nativeErr, &replayed) {
		if record, present := replayed.Existing(); present {
			name, err := configurationOperationName(home, record.ID())
			if err != nil {
				return nil, err
			}
			return h.readOperation(ctx, name, nil)
		}
	}
	id := result.Attempt()
	if record, present := result.APIRequest(); present {
		id = record.ID()
	}
	if result.Outcome() == configactivation.CommitUnknown || result.AttemptOutcome() == storage.TransactionIndeterminate {
		fault := apierrors.New(api.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN)
		if id.Valid() {
			if name, err := configurationOperationName(home, id); err == nil {
				fault = fault.WithOperation(name)
			}
		}
		return nil, fault
	}
	if !id.Valid() {
		if record, present, err := h.native.documents.Store().ProbeConfigurationAPIContext(ctx); err != nil {
			return nil, integrationFault(err)
		} else if present {
			id = record.ID()
		}
	}
	if id.Valid() {
		name, err := configurationOperationName(home, id)
		if err != nil {
			return nil, err
		}
		return h.readOperation(ctx, name, &result)
	}
	if nativeErr != nil {
		return nil, integrationFault(nativeErr)
	}
	return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
}
