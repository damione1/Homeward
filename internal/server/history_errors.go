package server

import (
	"context"
	"errors"

	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/damione1/Homeward/internal/storage"
)

// historyFault projects native read failures into the common error vocabulary.
// A stale capture requires an explicit new query, never an automatic restart
// against a different retention, configuration or authorization snapshot.
func historyFault(err error) apierrors.Fault {
	reason := api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		reason = api.ApiErrorReason_API_ERROR_REASON_REQUEST_DEADLINE_EXCEEDED
	case errors.Is(err, context.Canceled):
		reason = api.ApiErrorReason_API_ERROR_REASON_REQUEST_CANCELLED
	case errors.Is(err, authorization.ErrDenied):
		reason = api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED
	case errors.Is(err, historyquery.ErrInvalid), errors.Is(err, contracts.ErrInvalidHistoryContract), errors.Is(err, storage.ErrHistoryInvalid):
		reason = api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED
	case errors.Is(err, historyquery.ErrStale):
		reason = api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED
	case errors.Is(err, historyquery.ErrPointTooLarge), errors.Is(err, historyquery.ErrBudget):
		reason = api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED
	case errors.Is(err, authorization.ErrCapacity):
		reason = api.ApiErrorReason_API_ERROR_REASON_RATE_LIMITED
	case errors.Is(err, historyquery.ErrIntegrity), errors.Is(err, storage.ErrHistoryIntegrity), errors.Is(err, storage.ErrRecorderCoverageIntegrity):
		reason = api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY
	case errors.Is(err, historyquery.ErrUnavailable), errors.Is(err, authorization.ErrUnavailable):
		reason = api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE
	}
	return apierrors.New(reason)
}
