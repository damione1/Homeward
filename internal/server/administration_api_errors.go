package server

import (
	"context"
	"errors"

	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apimutations"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/configplans"
	"github.com/damione1/Homeward/internal/configsimulation"
	"github.com/damione1/Homeward/internal/storage"
)

func administrationFault(err error) apierrors.Fault {
	var native apierrors.Fault
	if errors.As(err, &native) {
		return native
	}
	var reason api.ApiErrorReason
	var workspace *configdocuments.Error
	var rejected *config.WorkspaceRejection
	var store *storage.Error
	var request *storage.ConfigurationRequestError
	var mutation *apimutations.Error
	switch {
	case errors.Is(err, authorization.ErrDenied), errors.Is(err, configplans.ErrAuthorMismatch):
		reason = api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED
	case errors.Is(err, configactivation.ErrActivationBusy):
		reason = api.ApiErrorReason_API_ERROR_REASON_STATE_PRECONDITION_FAILED
	case errors.Is(err, configactivation.ErrCommitIndeterminate):
		reason = api.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN
	case errors.Is(err, configactivation.ErrWorkspacePreconditionConflict), errors.Is(err, config.ErrStartupSourceChanged), errors.Is(err, configplans.ErrStale), errors.Is(err, authorization.ErrConflict):
		reason = api.ApiErrorReason_API_ERROR_REASON_ETAG_MISMATCH
	case errors.Is(err, contracts.ErrInvalidConfigurationContract), errors.Is(err, contracts.ErrInvalidConfigurationPlanContract), errors.Is(err, contracts.ErrInvalidConfigurationApplyContract), errors.Is(err, contracts.ErrInvalidConfigurationSimulationContract), errors.Is(err, configplans.ErrInvalid), errors.Is(err, configsimulation.ErrInvalid), errors.Is(err, storage.ErrAPIResourceIdentity):
		reason = api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED
	case errors.Is(err, storage.ErrAPIResourceIntegrity):
		reason = api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY
	case errors.Is(err, storage.ErrAPIResourceNotFound):
		reason = api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND
	case errors.Is(err, apimutations.ErrInvalidRequest):
		reason = api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED
	case errors.As(err, &mutation):
		switch mutation.Code() {
		case apimutations.RequestIDReused:
			reason = api.ApiErrorReason_API_ERROR_REASON_REQUEST_ID_REUSED
		case apimutations.Capacity:
			reason = api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED
		case apimutations.StoredResultCorrupt:
			reason = api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY
		default:
			reason = api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE
		}
	case errors.As(err, &request):
		switch request.Code() {
		case storage.ConfigurationRequestInvalid:
			reason = api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED
		case storage.ConfigurationRequestReused:
			reason = api.ApiErrorReason_API_ERROR_REASON_REQUEST_ID_REUSED
		case storage.ConfigurationRequestCapacity:
			reason = api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED
		case storage.ConfigurationRequestIntegrity:
			reason = api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY
		default:
			reason = api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE
		}
	case errors.As(err, &rejected):
		if _, admitted := rejected.SourceSHA256(); admitted {
			reason = api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED
		} else {
			reason = api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE
		}
	case errors.As(err, &workspace):
		switch workspace.Kind() {
		case configdocuments.FailureInvalid:
			reason = api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED
		case configdocuments.Conflict:
			reason = api.ApiErrorReason_API_ERROR_REASON_ETAG_MISMATCH
		case configdocuments.ReconciliationRequired, configdocuments.IncompleteHistory:
			reason = api.ApiErrorReason_API_ERROR_REASON_CONFIG_CHANGED
		case configdocuments.Unavailable:
			reason = api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE
		default:
			reason = api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE
		}
	case errors.As(err, &store):
		switch store.Kind() {
		case storage.ConfigIntegrity:
			reason = api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY
		case storage.ConfigConflict:
			reason = api.ApiErrorReason_API_ERROR_REASON_ETAG_MISMATCH
		case storage.ConfigInvalid:
			reason = api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED
		case storage.ConfigIncomplete:
			reason = api.ApiErrorReason_API_ERROR_REASON_CONFIG_CHANGED
		case storage.ConfigNotFound:
			reason = api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND
		default:
			reason = api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE
		}
	case errors.Is(err, context.Canceled):
		reason = api.ApiErrorReason_API_ERROR_REASON_REQUEST_CANCELLED
	case errors.Is(err, context.DeadlineExceeded):
		reason = api.ApiErrorReason_API_ERROR_REASON_REQUEST_DEADLINE_EXCEEDED
	case errors.Is(err, authorization.ErrUnavailable), errors.Is(err, configplans.ErrUnavailable), errors.Is(err, storage.ErrAPIResourceUnavailable), errors.Is(err, configactivation.ErrActivationUnavailable), errors.Is(err, errorsSimulationUnavailable):
		reason = api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE
	default:
		reason = api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE
	}
	return apierrors.New(reason)
}

func configurationReadError(ctx context.Context, err error) error {
	var violation *contracts.ConfigurationViolation
	if errors.As(err, &violation) && violation != nil {
		detail, buildErr := apierrors.NewViolation((&api.GetConfigurationDocumentRequest{}).ProtoReflect().Descriptor(), violation.Field(), violation.Rule())
		if buildErr != nil {
			return apiRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE))
		}
		return apiRPCError(ctx, apierrors.Validation(detail))
	}
	return apiRPCError(ctx, administrationFault(err))
}
