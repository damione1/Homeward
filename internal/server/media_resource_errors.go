package server

import (
	"context"
	"errors"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/mediacontent"
	"github.com/damione1/Homeward/internal/mediasessions"
)

func mediaResourceFault(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var own *mediasessions.Error
	if errors.As(err, &own) {
		var reason api.ApiErrorReason
		switch own.Code() {
		case mediasessions.Invalid:
			reason = api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED
		case mediasessions.Denied, mediasessions.CommittedResponseWithheld:
			reason = api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED
		case mediasessions.NotFound:
			reason = api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND
		case mediasessions.SourceChanged:
			reason = api.ApiErrorReason_API_ERROR_REASON_CONFIG_CHANGED
		case mediasessions.ActiveCapacity, mediasessions.RetentionCapacity:
			reason = api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED
		case mediasessions.Unavailable, mediasessions.Closed:
			reason = api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE
		case mediasessions.CommitUnknown:
			reason = api.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN
		default:
			reason = api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE
		}
		return apierrors.New(reason)
	}
	switch {
	case errors.Is(err, mediacontent.ErrInvalid):
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
	case errors.Is(err, mediacontent.ErrNotFound):
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND)
	case errors.Is(err, mediacontent.ErrUnavailable):
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	case errors.Is(err, mediacontent.ErrUnsupported):
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_CAPABILITY_UNAVAILABLE)
	case errors.Is(err, mediacontent.ErrSourceChanged):
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_CONFIG_CHANGED)
	}
	return businessMutationFault(err)
}
func mediaResourceError(ctx context.Context, err error) error {
	return apiRPCError(ctx, mediaResourceFault(err))
}
